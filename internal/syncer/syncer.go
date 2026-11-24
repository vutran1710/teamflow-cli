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
	"reflect"
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
	Drift      string   `json:"drift"`
	Note       string   `json:"note"`
	ResolvedBy []string `json:"resolved_by"`
}

type pending struct {
	item  store.SpoolItem
	emit  spooledEmit
	drift spooledDrift // a drift act's
	human bool         // a human act: sent alone
	valid bool         // sendable at all
}

func readItem(it store.SpoolItem) pending {
	p := pending{item: it}
	switch {
	case it.Method == "signal.emit":
		p.valid = json.Unmarshal([]byte(it.Params), &p.emit) == nil
		p.human = isVerdict(p.emit.Signal.Kind)
	case slices.Contains(protocol.HumanActsMethods, it.Method):
		p.valid = json.Unmarshal([]byte(it.Params), &p.drift) == nil && p.drift.Drift != ""
		p.human = true
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

// A signal the server keeps failing on (no answer for it, an answer this client can't read, an internal error, an
// HTTP 500) would hold every later item back forever. A failure counts only when it is the item's own: it went
// alone, or other calls in its request were answered (an unreadable answer inside a batch never counts). When a
// whole request fails, or every call in it, nothing counts and the head goes alone next time. A signal with
// GiveUpFails counted failures spanning at least GiveUpAfter is dropped (its session told) only when the item
// right behind it is then freshly accepted in the same flush (never sent before: not the stored answer to an
// earlier send): the server stores again, the signal is the problem. If that item is refused, answered from an
// earlier send, or fails the same way (a store outage), the head stays; with nothing behind it, it is never
// dropped. A human act is never dropped this way (only by the person: teamflow outbox drop). An unreachable
// server (no connection, a timeout, 502, 503, 504, 429), UPGRADE_REQUIRED and a key no longer a member's never
// count: waiting loses nothing.
const (
	GiveUpFails = 10
	GiveUpAfter = 24 * time.Hour
)

// ErrGaveUp is an item dropped after GiveUpFails failures over GiveUpAfter.
var ErrGaveUp = errors.New("gave up")

// Push sends the outbox ob with c (Flush with no items of the caller's own) and returns how many items left it.
func Push(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox) (int, error) {
	f, err := Flush(ctx, st, c, ob, nil)
	return f.Sent, err
}

// Flush sends the outbox ob (a sign-in's outbox of one project) with c, the sign-in's current key for the project,
// in order: routine signals in batches (MaxBatch and MaxBodyBytes, packed by the client, one JSON-RPC array per
// request), each human act (a verdict, drift.resolve, drift.dismiss) alone after everything before it. Each
// request's answers are recorded before the next goes out. An item leaves the outbox only with its answer:
// accepted, or refused for good (INVALID_PARAMS, a reasoned refusal, too large, never sendable, given up on): its
// session is told, unless it is one of the caller's own (mine), whose answers come back in Flushed.Answers
// instead. A transient failure (offline, 5xx, 429, a lost or unreadable answer, UPGRADE_REQUIRED, a key no
// longer a member's) stops the flush there: the item and everything after it wait, and the error is returned.
// Retries send the same ids, which the server answers once. Items queued while it runs are sent too. The caller
// holds the outbox's sender lock (see core).
func Flush(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox, mine map[string]bool) (Flushed, error) {
	f := Flushed{Answers: map[string]Answer{}}
	for range maxRounds {
		items, err := st.Spooled(ob)
		if err != nil {
			return f, fmt.Errorf("read the spool: %w", err)
		}
		if len(items) == 0 {
			return f, nil
		}
		before := f.Sent
		if err := flushOnce(ctx, st, c, ob, items, mine, &f); err != nil {
			return f, err
		}
		if f.Sent == before {
			return f, nil
		}
	}
	return f, nil
}

// errReread ends a pass over the outbox after a head was given up on: the next pass reads it again.
var errReread = errors.New("read the outbox again")

func flushOnce(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox, items []store.SpoolItem,
	mine map[string]bool, f *Flushed) error {
	if err := flushPass(ctx, st, c, ob, items, mine, f); !errors.Is(err, errReread) {
		return err
	}
	return nil
}

func flushPass(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox, items []store.SpoolItem,
	mine map[string]bool, f *Flushed) error {
	var run []pending      // routine signals waiting for their batch
	var candidate *pending // a head past the give-up limit, kept: dropped only if the item behind it is answered
	stopped := func(err error) error {
		if err == nil || candidate == nil {
			return err
		}
		cd := candidate
		candidate = nil
		if gerr := confirmGiveUp(ctx, st, c, ob, *cd, err, mine, f); gerr != nil {
			return gerr
		}
		return errReread // the item behind it was sent here: start over from the outbox as it is now
	}
	sendRun := func() error {
		if len(run) == 0 {
			return nil
		}
		calls := make([]api.Call, len(run))
		ids := make([]string, len(run))
		for i, p := range run {
			calls[i] = api.Call{Method: p.item.Method, Params: json.RawMessage(p.item.Params)}
			ids[i] = p.item.Signal
		}
		if err := st.Tried(ids); err != nil {
			return fmt.Errorf("record the sends: %w", err)
		}
		var stop error
		berr := c.BatchEach(ctx, calls, func(start, end int, res []api.Result, rerr error) bool {
			chunk := run[start:end]
			if rerr != nil { // the whole request failed: each of its calls failed with it
				res = make([]api.Result, end-start)
				for i := range res {
					res[i].Err = rerr
				}
			}
			// A failure is the item's own only when it went alone, or others in its request were answered.
			own := len(chunk) == 1
			for i, r := range res {
				own = own || answered(chunk[i], r)
			}
			candidate, stop = settle(st, chunk, res, mine, f, own, nil)
			if stop != nil && !own { // which call the server failed on is not known: the head goes alone next
				if err := st.SetAlone(chunk[0].item.Signal, true); err != nil {
					stop = err
				}
			}
			return stop != nil
		})
		run = run[:0]
		return stopped(cmpErr(stop, berr))
	}
	for _, it := range items {
		p := readItem(it)
		if !p.valid { // never sendable: its session is told, and it leaves the outbox
			if err := sendRun(); err != nil {
				return err
			}
			if _, err := settle(st, []pending{p}, []api.Result{{Err: errUnreadable}}, mine, f, true, nil); err != nil {
				return err
			}
			continue
		}
		if !p.human && !p.item.Alone {
			run = append(run, p)
			continue
		}
		if err := sendRun(); err != nil {
			return err
		}
		if !p.human { // marked to go alone
			run = append(run, p)
			if err := sendRun(); err != nil {
				return err
			}
			continue
		}
		res, err := sendAlone(ctx, st, c, ob, p)
		if err != nil {
			return err
		}
		if _, err := settle(st, []pending{p}, []api.Result{res}, mine, f, true, nil); err != nil {
			return err // a human act is never a give-up candidate
		}
	}
	return sendRun()
}

// sendAlone sends one item as a single call (a human act; the item behind a give-up candidate).
func sendAlone(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox, p pending) (api.Result, error) {
	if err := st.Tried([]string{p.item.Signal}); err != nil {
		return api.Result{}, fmt.Errorf("record the send: %w", err)
	}
	var raw json.RawMessage
	cerr := c.Call(ctx, p.item.Method, json.RawMessage(p.item.Params), &raw)
	if cerr != nil && p.drift.Drift != "" && p.item.Tries > 0 && (reason(cerr) == protocol.RejectReasonDriftClosed ||
		reason(cerr) == protocol.RejectReasonNotAllowed) {
		raw, cerr = confirmDrift(ctx, st, c, ob, p, cerr)
	}
	return api.Result{Result: raw, Err: cerr}, nil
}

// answered says whether r is the server's final word on p: a result this client can read, or p's own refusal.
func answered(p pending, r api.Result) bool {
	if r.Err != nil {
		return refused(r.Err)
	}
	if p.item.Method != "signal.emit" {
		return true
	}
	var res protocol.SignalResult
	return json.Unmarshal(r.Result, &res) == nil
}

// confirmGiveUp decides on a head past the give-up limit (cd, kept with keep): the item right behind it is sent
// alone. Freshly accepted (never sent before), the server stores and the head is the problem: the head is dropped
// in the same transaction as that answer, and the pass starts over. Refused, or answered with what the server
// stored for an earlier send, it leaves the outbox but proves nothing: the head stays. Failing the same way, both
// stay. With nothing behind it, the head is never dropped.
func confirmGiveUp(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox, cd pending, keep error,
	mine map[string]bool, f *Flushed) error {
	items, err := st.Spooled(ob)
	if err != nil {
		return keep
	}
	at := slices.IndexFunc(items, func(it store.SpoolItem) bool { return it.Signal == cd.item.Signal })
	if at < 0 || at+1 >= len(items) {
		return keep
	}
	nx := readItem(items[at+1])
	if !nx.valid {
		return keep
	}
	var res api.Result
	if nx.human {
		if res, err = sendAlone(ctx, st, c, ob, nx); err != nil {
			return err
		}
	} else {
		if err := st.Tried([]string{nx.item.Signal}); err != nil {
			return err
		}
		rs, berr := c.Batch(ctx, []api.Call{{Method: nx.item.Method, Params: json.RawMessage(nx.item.Params)}})
		res = api.Result{Err: berr}
		if berr == nil && len(rs) == 1 {
			res = rs[0]
		}
	}
	if !answered(nx, res) {
		return keep
	}
	if !fresh(nx, res) {
		// A refusal, or the stored answer to an earlier send: no proof the server stores again. The item behind
		// leaves (its session told); the head stays, and the next item is tried next time.
		if _, err := settle(st, []pending{nx}, []api.Result{res}, mine, f, false, nil); err != nil {
			return err
		}
		return keep
	}
	// The item behind was freshly stored: the head is the problem. Its answer and the head's drop are one
	// transaction.
	_, err = settle(st, []pending{nx}, []api.Result{res}, mine, f, false, &giveUpOf{p: cd, last: keep})
	return err
}

// giveUpOf is a head to drop, with the failure that kept it last.
type giveUpOf struct {
	p    pending
	last error
}

// fresh says whether r proves the server stores now: p was never sent before (no earlier stored answer to come
// back) and it was accepted (a signal), or done (a drift act). A refusal or a dedup proves nothing.
func fresh(p pending, r api.Result) bool {
	if p.item.Tries > 0 || r.Err != nil {
		return false
	}
	if p.item.Method != "signal.emit" {
		return true
	}
	var res protocol.SignalResult
	return json.Unmarshal(r.Result, &res) == nil && res.Accepted
}

func reason(err error) protocol.RejectReason {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return protocol.RejectReason(ae.Reason)
	}
	return ""
}

// confirmDrift settles a drift act sent before (its answer lost) that is now refused as the server refuses an act
// on a closed drift (DRIFT_CLOSED for a resolve, NOT_ALLOWED for a dismiss): the drift may be closed by that
// earlier send. The replica is brought up to date: when the drift is closed with this act's status and note (and,
// when the act named signals, those: the server fills in the drift's own otherwise), the act reached the team
// and its answer is the drift as it stands; otherwise the refusal stands.
func confirmDrift(ctx context.Context, st *store.Store, c *api.Client, ob store.Outbox, p pending, refusal error) (json.RawMessage, error) {
	if _, err := Pull(ctx, st, c, 0); err != nil {
		return nil, refusal
	}
	ds, err := st.Drifts(ob.Project, "")
	if err != nil {
		return nil, refusal
	}
	want := protocol.DriftStatusResolved
	if p.item.Method == "drift.dismiss" {
		want = protocol.DriftStatusDismissed
	}
	for _, d := range ds {
		if d.ID != p.drift.Drift || d.Status != want || d.Resolution != p.drift.Note {
			continue
		}
		if p.item.Method == "drift.resolve" && len(p.drift.ResolvedBy) > 0 && !sameSet(d.ResolvedBy, p.drift.ResolvedBy) {
			continue
		}
		raw, err := json.Marshal(protocol.Drifts{Drifts: []protocol.Drift{d}})
		if err != nil {
			return nil, refusal
		}
		return raw, nil
	}
	return nil, refusal
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

var (
	errUnreadable       = errors.New("the queued item could not be read")
	errUnreadableAnswer = errors.New("the server's answer could not be read")
)

// settle records the answers to ps in one transaction: each answered item leaves the outbox, and its session gets
// a notice (the caller's own get an Answer instead). A transient failure keeps the item, counted against it when
// it is the server failing on that item (own: it went alone, or others in its request were answered; an
// unreadable answer inside a batch never counts). The outbox's head (no earlier item of its request kept), a
// signal past GiveUpFails over GiveUpAfter, is dropped only once an item behind it is freshly accepted (see
// fresh): here when one behind it in the same request was; when it is the last of its request it comes back as
// the candidate (see confirmGiveUp). A human act is never dropped. drop, when given, is a head dropped in the
// same transaction. It returns the candidate, and the first error that kept an item (the flush stops), or the
// store's error.
func settle(st *store.Store, ps []pending, results []api.Result, mine map[string]bool, f *Flushed, own bool,
	drop *giveUpOf) (*pending, error) {
	var keep error
	var cand *pending
	done := 0
	answers := map[string]Answer{}
	t := now()
	err := st.Tx(func(tx *store.Tx) error {
		done, keep, cand = 0, nil, nil
		clear(answers)
		for i, p := range ps {
			r := results[i]
			var res protocol.SignalResult
			callErr := r.Err
			if callErr == nil && p.item.Method == "signal.emit" {
				if err := json.Unmarshal(r.Result, &res); err != nil {
					callErr = fmt.Errorf("%w: %v", errUnreadableAnswer, err)
				}
			}
			head := keep == nil // no earlier item of this request was kept: this one is the outbox's head
			if callErr != nil && !refused(callErr) {
				if !own || !counts(callErr) || (len(ps) > 1 && errors.Is(callErr, errUnreadableAnswer)) {
					keep = cmpErr(keep, callErr)
					continue
				}
				it, err := tx.Failed(p.item.Signal, callErr.Error(), t)
				if err != nil {
					return err
				}
				p.item = it
				over := head && !p.human && it.Fails >= GiveUpFails && t.Sub(it.FirstFail) >= GiveUpAfter
				behind := false // proof the server stores now: an item behind it freshly accepted
				for j := i + 1; j < len(ps); j++ {
					behind = behind || fresh(ps[j], results[j])
				}
				if !over || !behind {
					if over && i == len(ps)-1 && cand == nil {
						c := p
						cand = &c
					}
					keep = cmpErr(keep, callErr)
					continue
				}
				callErr = fmt.Errorf("%w after %d tries over %s: %v", ErrGaveUp, it.Fails, t.Sub(it.FirstFail).Round(time.Hour), callErr)
			}
			if err := tx.SpoolDone(p.item.Signal); err != nil {
				return err
			}
			done++
			if mine[p.item.Signal] {
				answers[p.item.Signal] = Answer{Result: r.Result, Err: callErr}
				continue
			}
			typ, about, text := noticeOf(p, res, callErr)
			if err := tx.Notice(p.item.Session, typ, about, text, t); err != nil {
				return err
			}
		}
		if drop != nil { // the head given up on, with the answer that proved it
			p := drop.p
			why := fmt.Errorf("%w after %d tries over %s: %v", ErrGaveUp, p.item.Fails, t.Sub(p.item.FirstFail).Round(time.Hour), drop.last)
			if err := tx.SpoolDone(p.item.Signal); err != nil {
				return err
			}
			done++
			if mine[p.item.Signal] {
				answers[p.item.Signal] = Answer{Err: why}
			} else {
				typ, about, text := noticeOf(p, protocol.SignalResult{}, why)
				if err := tx.Notice(p.item.Session, typ, about, text, t); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("record the answers: %w", err)
	}
	f.Sent += done
	for id, a := range answers {
		f.Answers[id] = a
	}
	if keep == nil {
		cand = nil
	}
	return cand, keep
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// refused says whether a call's error is the server's final word on this one item (it leaves the outbox), rather
// than a reason to send it again later. NOT_A_MEMBER and UPGRADE_REQUIRED are about the key or the client, never
// the item: they keep it.
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

// counts says whether a transient failure is the server failing on this item (it counts towards giving up on it),
// not an outage, a busy server, an old client or a key no longer good (those never count).
func counts(err error) bool {
	if errors.Is(err, api.ErrServerFault) || errors.Is(err, errUnreadableAnswer) {
		return true
	}
	if errors.Is(err, api.ErrOffline) || errors.Is(err, api.ErrUpgradeRequired) {
		return false
	}
	var ae *api.APIError
	if !errors.As(err, &ae) || ae.Reason == string(protocol.RejectReasonNotAMember) {
		return false
	}
	return ae.Code < 0 // a JSON-RPC answer: the server spoke about this call (an HTTP status is a proxy's)
}

// noticeOf is the notice for an answered item: its type, what it is about, and its text.
func noticeOf(p pending, res protocol.SignalResult, callErr error) (protocol.NoticeType, string, string) {
	id := p.item.Signal
	if p.drift.Drift != "" {
		d := p.drift.Drift
		act := strings.TrimPrefix(p.item.Method, "drift.")
		switch {
		case callErr == nil:
			return protocol.NoticeTypeSignalSent, d, fmt.Sprintf("Your queued drift %s of %s reached the team "+
				"once the server was back.", act, d)
		case errors.Is(callErr, ErrGaveUp):
			return protocol.NoticeTypeSignalRejected, d, fmt.Sprintf("Your drift %s of %s could not be sent: the "+
				"server kept failing on it (%v). It was dropped so what came after it can go; do it again if it still "+
				"matters.", act, d, callErr)
		}
		reason, message := refusalOf(callErr)
		text := strings.TrimSpace(fmt.Sprintf("Your drift %s of %s was rejected after it was queued: %s. %s", act, d, reason, message))
		return protocol.NoticeTypeSignalRejected, d, text
	}
	if errors.Is(callErr, ErrGaveUp) {
		return protocol.NoticeTypeSignalRejected, id, fmt.Sprintf("Your signal %s \"%s\" could not be sent: the server "+
			"kept failing on it (%v). It was dropped so the signals after it can go; record it again if it still matters.",
			id, p.emit.Signal.Title, callErr)
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
		n, more, err := poll(ctx, st, c, timeout, cursors, 0)
		total += n
		if err != nil || !more {
			return total, err
		}
	}
	return total, nil
}

// Poll is one sync.poll of project from its cursor, held by the server up to wait for something new (the live
// sync's fallback), applied as Pull applies a page. timeout bounds the call (it must be longer than wait). It
// returns how many events it read.
func Poll(ctx context.Context, st *store.Store, c *api.Client, project string, wait, timeout time.Duration) (int, error) {
	cursors, err := st.CursorsErr()
	if err != nil {
		return 0, fmt.Errorf("read the cursors: %w", err)
	}
	n, _, err := poll(ctx, st, c, timeout, map[string]int64{project: cursors[project]}, wait)
	return n, err
}

// poll is one sync.poll applied in one transaction; more says a page came back (events or a reset).
func poll(ctx context.Context, st *store.Store, c *api.Client, timeout time.Duration, cursors map[string]int64,
	wait time.Duration) (n int, more bool, err error) {
	p := protocol.NewSyncPoll()
	p.Cursors, p.Wait = cursors, wait.Seconds()
	var batch protocol.SyncBatch
	if err := call(ctx, c, timeout, "sync.poll", p, &batch); err != nil {
		return 0, false, err
	}
	err = st.Tx(func(tx *store.Tx) error {
		reset := map[string]bool{}
		for _, p := range batch.Reset {
			reset[p] = true
			if err := tx.ResetProject(p, batch.Cursors[p]); err != nil {
				return err
			}
		}
		if _, err := ApplyEvents(tx, batch.Events, now(), nil); err != nil {
			return err
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
		return 0, false, err
	}
	return len(batch.Events), len(batch.Events) > 0 || len(batch.Reset) > 0, nil
}

// ApplyEvents applies events in tx, each once: an event at or below its project's cursor in the replica is
// already applied (another sync brought it in first, or a server sent it again) and is skipped. Seqs may have
// holes. It moves no cursor (the caller sets it with the page's cursor) and returns the events applied; done, when
// given, hears each.
func ApplyEvents(tx *store.Tx, events []protocol.SyncEvent, t time.Time, done func(project string, seq int64)) (int, error) {
	cursors, err := tx.CursorsErr()
	if err != nil {
		return 0, fmt.Errorf("read the cursors: %w", err)
	}
	n := 0
	for _, ev := range events {
		project, seq := Position(ev)
		if seq <= cursors[project] {
			continue
		}
		if err := Apply(tx, ev, t); err != nil {
			return n, fmt.Errorf("apply %s: %w", ev.EventType(), err)
		}
		cursors[project] = seq
		n++
		if done != nil {
			done(project, seq)
		}
	}
	return n, nil
}

// Position is an event's project and seq (every variant has both; an unknown one's are read from its JSON).
func Position(ev protocol.SyncEvent) (project string, seq int64) {
	if u, ok := ev.(protocol.UnknownEvent); ok {
		var pos struct {
			Project string `json:"project"`
			Seq     int64  `json:"seq"`
		}
		_ = json.Unmarshal(u.Raw, &pos)
		return pos.Project, pos.Seq
	}
	v := reflect.Indirect(reflect.ValueOf(ev))
	if v.Kind() != reflect.Struct {
		return "", 0
	}
	if f := v.FieldByName("Project"); f.IsValid() && f.Kind() == reflect.String {
		project = f.String()
	}
	if f := v.FieldByName("Seq"); f.IsValid() && f.CanInt() {
		seq = f.Int()
	}
	return project, seq
}

func call(ctx context.Context, c *api.Client, timeout time.Duration, method string, params, out any) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return c.Call(ctx, method, params, out)
}
