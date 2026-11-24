// Package syncer pushes the spool, pulls events into the replica, and turns events into notices for sessions: a
// port of the Python client's sync.py.
//
// The spool is the project key's ordered outbox, and Flush sends it in order. It is safe to repeat: the server
// keeps the first result of each signal id, so a flush whose answer was lost is sent again and answered the same,
// and an item stays in the outbox until its answer arrives. A human act (a verdict, drift.resolve, drift.dismiss)
// goes in a request of its own, never inside a batch, after everything queued before it.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// MaxPages is how many sync.poll pages one Pull reads at most (each holds up to the server's page of events).
const MaxPages = 20

// now is the clock for notices (a variable for tests).
var now = time.Now

// isVerdict says whether a signal kind is a human act (HUMAN_ACTS.verdict_signal_kinds). A human act is sent
// alone as a plain object body, never in an array: the server counts any array, even of one, as a batch.
func isVerdict(k protocol.SignalKind) bool {
	return slices.Contains(protocol.HumanActsVerdictSignalKinds, string(k))
}

// spooledEmit is what a flush reads of a spooled SignalEmit: enough to classify it and word its notice. The params
// are sent as stored.
type spooledEmit struct {
	Signal struct {
		ID    string              `json:"id"`
		Kind  protocol.SignalKind `json:"kind"`
		Title string              `json:"title"`
	} `json:"signal"`
}

// spooledDrift is what a flush reads of a queued drift act.
type spooledDrift struct {
	Drift string `json:"drift"`
}

type pending struct {
	item  store.SpoolItem
	emit  spooledEmit
	drift string // a drift act's drift
	human bool   // a human act: sent alone
	valid bool   // sendable at all
}

func readItem(it store.SpoolItem) pending {
	p := pending{item: it}
	switch {
	case it.Method == "signal.emit":
		p.valid = json.Unmarshal([]byte(it.Params), &p.emit) == nil
		p.human = isVerdict(p.emit.Signal.Kind)
	case slices.Contains(protocol.HumanActsMethods, it.Method):
		var d spooledDrift
		p.valid = json.Unmarshal([]byte(it.Params), &d) == nil && d.Drift != ""
		p.drift, p.human = d.Drift, true
	}
	return p
}

// Answer is what became of one of the caller's own items that left the outbox: the server's result, or the
// refusal that dropped it (Err).
type Answer struct {
	Result json.RawMessage
	Err    error
}

// Flushed is what one flush did: how many items left the outbox, and the answers to the caller's own items.
type Flushed struct {
	Sent    int
	Answers map[string]Answer
}

// maxRounds bounds how often one flush reads the outbox again for items queued while it ran.
const maxRounds = 20

// Push sends the outbox of c's key (Flush with no items of the caller's own) and returns how many items left it.
func Push(ctx context.Context, st *store.Store, c *api.Client) (int, error) {
	f, err := Flush(ctx, st, c, nil)
	return f.Sent, err
}

// Flush sends the outbox of c's key in order: routine signals in batches (MaxBatch and MaxBodyBytes, packed by the
// client, one JSON-RPC array per request), each human act (a verdict, drift.resolve, drift.dismiss) alone after
// everything before it. Each request's answers are recorded before the next goes out. An item leaves the outbox
// only with its answer: accepted, or refused for good (INVALID_PARAMS, a reasoned refusal, too large, never
// sendable): its session is told, unless it is one of the caller's own (mine), whose answers come back in
// Flushed.Answers instead. A transient failure (offline, 5xx, 429, a lost or unreadable answer, UPGRADE_REQUIRED,
// a key no longer a member's) stops the flush there: the item and everything after it wait, and the error is
// returned. Retries send the same ids, which the server answers once. Items queued while it runs are sent too.
// The caller holds the sender lock (see core).
func Flush(ctx context.Context, st *store.Store, c *api.Client, mine map[string]bool) (Flushed, error) {
	f := Flushed{Answers: map[string]Answer{}}
	for range maxRounds {
		items, err := st.Spooled(c.KeyRef())
		if err != nil {
			return f, fmt.Errorf("read the spool: %w", err)
		}
		if len(items) == 0 {
			return f, nil
		}
		before := f.Sent
		if err := flushOnce(ctx, st, c, items, mine, &f); err != nil {
			return f, err
		}
		if f.Sent == before {
			return f, nil
		}
	}
	return f, nil
}

func flushOnce(ctx context.Context, st *store.Store, c *api.Client, items []store.SpoolItem, mine map[string]bool, f *Flushed) error {
	var run []pending // routine signals waiting for their batch
	sendRun := func() error {
		if len(run) == 0 {
			return nil
		}
		calls := make([]api.Call, len(run))
		for i, p := range run {
			calls[i] = api.Call{Method: p.item.Method, Params: json.RawMessage(p.item.Params)}
		}
		var stop error
		berr := c.BatchEach(ctx, calls, func(start, end int, res []api.Result) bool {
			stop = settle(st, run[start:end], res, mine, f)
			return stop != nil
		})
		run = run[:0]
		return cmpErr(stop, berr)
	}
	for _, it := range items {
		p := readItem(it)
		if !p.valid { // never sendable: its session is told, and it leaves the outbox
			if err := sendRun(); err != nil {
				return err
			}
			if err := settle(st, []pending{p}, []api.Result{{Err: errUnreadable}}, mine, f); err != nil {
				return err
			}
			continue
		}
		if !p.human {
			run = append(run, p)
			continue
		}
		if err := sendRun(); err != nil {
			return err
		}
		var raw json.RawMessage
		cerr := c.Call(ctx, p.item.Method, json.RawMessage(p.item.Params), &raw)
		if err := settle(st, []pending{p}, []api.Result{{Result: raw, Err: cerr}}, mine, f); err != nil {
			return err
		}
	}
	return sendRun()
}

var errUnreadable = errors.New("the queued item could not be read")

// settle records the answers to ps in one transaction: each answered item leaves the outbox, and its session gets
// a notice (the caller's own get an Answer instead). It returns the first error that kept an item (a transient
// one: the flush stops), or the store's error.
func settle(st *store.Store, ps []pending, results []api.Result, mine map[string]bool, f *Flushed) error {
	var keep error
	done := 0
	answers := map[string]Answer{}
	t := now()
	err := st.Tx(func(tx *store.Tx) error {
		done, keep = 0, nil
		clear(answers)
		for i, p := range ps {
			r := results[i]
			var res protocol.SignalResult
			if r.Err == nil {
				if p.item.Method == "signal.emit" {
					if err := json.Unmarshal(r.Result, &res); err != nil {
						keep = cmpErr(keep, fmt.Errorf("signal.emit answer: %w", err))
						continue
					}
				}
			} else if !refused(r.Err) {
				keep = cmpErr(keep, r.Err)
				continue
			}
			if err := tx.SpoolDone(p.item.Signal); err != nil {
				return err
			}
			done++
			if mine[p.item.Signal] {
				answers[p.item.Signal] = Answer{Result: r.Result, Err: r.Err}
				continue
			}
			typ, about, text := noticeOf(p, res, r.Err)
			if err := tx.Notice(p.item.Session, typ, about, text, t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record the answers: %w", err)
	}
	f.Sent += done
	for id, a := range answers {
		f.Answers[id] = a
	}
	return keep
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// refused says whether a call's error is the server's final word on this one item (it leaves the outbox), rather
// than a reason to send it again later.
func refused(err error) bool {
	if errors.Is(err, api.ErrTooLarge) || errors.Is(err, errUnreadable) {
		return true
	}
	var ae *api.APIError
	if !errors.As(err, &ae) || errors.Is(err, api.ErrUpgradeRequired) {
		return false
	}
	switch protocol.RejectReason(ae.Reason) {
	case protocol.RejectReasonNotAMember: // the key is no longer good: the caller drops it, the outbox waits
		return false
	}
	return ae.Code == protocol.CodeInvalidParams || (ae.Code == protocol.CodeRuleFailed && ae.Reason != "")
}

// Refused says whether err is a permanent refusal of one item (see Flush), not a reason to try again.
func Refused(err error) bool { return refused(err) }

// noticeOf is the notice for an answered item: its type, what it is about, and its text.
func noticeOf(p pending, res protocol.SignalResult, callErr error) (protocol.NoticeType, string, string) {
	id := p.item.Signal
	if p.drift != "" || (p.item.Method != "signal.emit" && p.valid) {
		act := strings.ReplaceAll(strings.TrimPrefix(p.item.Method, "drift."), "_", " ")
		if callErr == nil {
			return protocol.NoticeTypeSignalSent, p.drift, fmt.Sprintf("Your queued drift %s of %s reached the team "+
				"once the server was back.", act, p.drift)
		}
		reason, message := refusalOf(callErr)
		text := strings.TrimSpace(fmt.Sprintf("Your drift %s of %s was rejected after it was queued: %s. %s", act, p.drift, reason, message))
		return protocol.NoticeTypeSignalRejected, p.drift, text
	}
	if callErr == nil && res.Accepted {
		state := ""
		if res.State != nil {
			state = " (" + string(*res.State) + ")"
		}
		return protocol.NoticeTypeSignalSent, id, fmt.Sprintf("Your queued signal %s \"%s\" reached the team once the "+
			"server was back: accepted%s.", id, p.emit.Signal.Title, state)
	}
	reason, message := "rejected", res.Message
	switch {
	case callErr != nil:
		reason, message = refusalOf(callErr)
	case res.Reason != nil:
		reason = string(*res.Reason)
	}
	text := strings.TrimSpace(fmt.Sprintf("Your signal %s was rejected after it was queued: %s. %s", id, reason, message))
	return protocol.NoticeTypeSignalRejected, id, text
}

func refusalOf(err error) (reason, message string) {
	var ae *api.APIError
	switch {
	case errors.As(err, &ae) && ae.Reason != "":
		return ae.Reason, ae.Message
	case errors.As(err, &ae):
		return "INVALID", ae.Message
	case errors.Is(err, api.ErrTooLarge):
		return "TOO_LARGE", "it is larger than the server takes"
	}
	return "UNREADABLE", err.Error()
}

// Pull reads every new event of the key's projects into the replica: sync.poll with wait 0, up to MaxPages pages.
// A project the server lists in reset (its cursor is beyond the server's head: a restored server) is dropped from
// the replica and pulled again from 0. Each page is applied in one transaction with its cursors. timeout (0: the
// client's own) bounds each poll. It returns how many events it read.
func Pull(ctx context.Context, st *store.Store, c *api.Client, timeout time.Duration) (int, error) {
	total := 0
	for range MaxPages {
		cursors, err := st.CursorsErr()
		if err != nil { // never pull from 0 because the store could not say where we are
			return total, fmt.Errorf("read the cursors: %w", err)
		}
		poll := protocol.NewSyncPoll()
		poll.Cursors, poll.Wait = cursors, 0
		var batch protocol.SyncBatch
		if err := call(ctx, c, timeout, "sync.poll", poll, &batch); err != nil {
			return total, err
		}
		reset := map[string]bool{}
		err = st.Tx(func(tx *store.Tx) error {
			for _, p := range batch.Reset {
				reset[p] = true
				if err := tx.ResetProject(p, batch.Cursors[p]); err != nil {
					return err
				}
			}
			t := now()
			for _, ev := range batch.Events {
				if err := Apply(tx, ev, t); err != nil {
					return fmt.Errorf("apply %s: %w", ev.EventType(), err)
				}
			}
			for p, seq := range batch.Cursors {
				if reset[p] {
					continue
				}
				if err := tx.SetCursor(p, seq); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		total += len(batch.Events)
		if len(batch.Events) == 0 && len(batch.Reset) == 0 {
			break
		}
	}
	return total, nil
}

func call(ctx context.Context, c *api.Client, timeout time.Duration, method string, params, out any) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return c.Call(ctx, method, params, out)
}
