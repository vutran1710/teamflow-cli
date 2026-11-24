// Package syncer pushes the spool, pulls events into the replica, and turns events into notices for sessions: a
// port of the Python client's sync.py.
//
// Push is safe to repeat: the server keeps the first result of each signal id, so a flush whose answer was lost
// is sent again and answered the same, and a signal stays in the spool until its answer arrives. A verdict (an
// APPROVE or REJECT signal) is a human act: it goes in a request of its own, never inside a batch.
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

// spooledEmit is what Push reads of a spooled SignalEmit: enough to classify it and word its notice. The params
// are sent as stored.
type spooledEmit struct {
	Signal struct {
		ID    string              `json:"id"`
		Kind  protocol.SignalKind `json:"kind"`
		Title string              `json:"title"`
	} `json:"signal"`
}

type pending struct {
	item  store.SpoolItem
	emit  spooledEmit
	valid bool
}

// Push sends the signals spooled for c's key: routine ones in batches (MaxBatch and MaxBodyBytes, packed by the
// client), each verdict alone. It returns how many left the spool. A signal leaves the spool only with its answer:
// accepted (its session is told it reached the team) or refused (its session is told why). Offline, an
// UPGRADE_REQUIRED server, or an answer that is not a verdict on the signal keeps it, and Push returns that error.
func Push(ctx context.Context, st *store.Store, c *api.Client) (int, error) {
	items, err := st.Spooled(c.KeyRef())
	if err != nil {
		return 0, fmt.Errorf("read the spool: %w", err)
	}
	if len(items) == 0 {
		return 0, nil
	}
	var queue []pending
	for _, it := range items {
		p := pending{item: it}
		p.valid = json.Unmarshal([]byte(it.Params), &p.emit) == nil
		queue = append(queue, p)
	}
	done := 0
	var batch []pending
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		calls := make([]api.Call, len(batch))
		for i, p := range batch {
			calls[i] = api.Call{Method: "signal.emit", Params: json.RawMessage(p.item.Params)}
		}
		results, berr := c.Batch(ctx, calls)
		n, serr := settle(st, batch[:len(results)], results)
		done += n
		batch = batch[:0]
		return errors.Join(berr, serr)
	}
	for _, p := range queue {
		if !p.valid { // never sendable: the session is told, and it leaves the spool
			n, err := settle(st, []pending{p}, []api.Result{{Err: errUnreadable}})
			done += n
			if err != nil {
				return done, err
			}
			continue
		}
		if !isVerdict(p.emit.Signal.Kind) {
			batch = append(batch, p)
			continue
		}
		if err := flush(); err != nil {
			return done, err
		}
		var raw json.RawMessage
		cerr := c.Call(ctx, "signal.emit", json.RawMessage(p.item.Params), &raw)
		n, err := settle(st, []pending{p}, []api.Result{{Result: raw, Err: cerr}})
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, flush()
}

var errUnreadable = errors.New("the queued signal could not be read")

// settle records the answers to ps: each answered signal leaves the spool and its session gets a notice. It
// returns how many left, and the first error that kept a signal in the spool.
func settle(st *store.Store, ps []pending, results []api.Result) (int, error) {
	var keep error
	done := 0
	t := now()
	err := st.Tx(func(tx *store.Tx) error {
		for i, p := range ps {
			r := results[i]
			var res protocol.SignalResult
			if r.Err == nil {
				if err := json.Unmarshal(r.Result, &res); err != nil {
					keep = cmpErr(keep, fmt.Errorf("signal.emit answer: %w", err))
					continue
				}
			} else if !refused(r.Err) {
				keep = cmpErr(keep, r.Err)
				continue
			}
			if err := tx.SpoolDone(p.item.Signal); err != nil {
				return err
			}
			typ, about, text := noticeOf(p, res, r.Err)
			if err := tx.Notice(p.item.Session, typ, about, text, t); err != nil {
				return err
			}
			done++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("record the answers: %w", err)
	}
	return done, keep
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// refused says whether a call's error is the server's final word on this one signal (it leaves the spool), rather
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
	case protocol.RejectReasonNotAMember: // the key is no longer good: the caller drops it, the spool waits
		return false
	}
	return ae.Code == protocol.CodeInvalidParams || (ae.Code == protocol.CodeRuleFailed && ae.Reason != "")
}

// noticeOf is the notice for an answered signal: its type, what it is about, and its text.
func noticeOf(p pending, res protocol.SignalResult, callErr error) (protocol.NoticeType, string, string) {
	id := p.item.Signal
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
