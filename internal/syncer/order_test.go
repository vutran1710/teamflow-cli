package syncer

// The ordered outbox (Task 8c-2b): items leave in the order they were made; human acts (verdicts, drift resolve and
// dismiss) go alone after everything before them; a transient error stops the outbox there and later items wait;
// a permanent refusal drops only that item, with a notice; each batch is settled before the next goes out, so a
// crash between batches resends nothing already answered.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

func driftAct(t *testing.T, st *store.Store, id, method, drift, session string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"drift": drift, "note": "settled"})
	must(t, st.OutboxAdd(id, method, session, ob, string(b), t0))
}

func notes(t *testing.T, st *store.Store, from, n int) []string {
	t.Helper()
	var ids []string
	for i := from; i < from+n; i++ {
		id := fmt.Sprintf("sg_%03d", i)
		spool(t, st, id, protocol.SignalKindNote, "n")
		ids = append(ids, id)
	}
	return ids
}

// Signals, a verdict and drift acts in one outbox: every human act alone, in its place; routine ones in batches
// between them. Sad path: two human acts in a row (no empty batch between them).
func TestFlushSendsHumanActsAloneInTheirPlace(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	notes(t, st, 0, 2)
	driftAct(t, st, "da_1", "drift.resolve", D1, "s1")
	notes(t, st, 2, 1)
	spool(t, st, "sg_v", protocol.SignalKindApprove, "ok")
	driftAct(t, st, "da_2", "drift.dismiss", "dr_2", "s1")
	notes(t, st, 3, 1)
	n, err := Push(context.Background(), st, client(srv), ob)
	must(t, err)
	if n != 7 || len(spooled(t, st)) != 0 {
		t.Fatalf("sent %d, left %v", n, spooled(t, st))
	}
	if got := strings.Join(f.shapes(), ","); got != "2,alone,1,alone,alone,1" {
		t.Fatalf("requests %s", got)
	}
	want := "sg_000,sg_001,drift.resolve " + D1 + ",sg_002,sg_v,drift.dismiss dr_2,sg_003"
	if got := strings.Join(f.order, ","); got != want {
		t.Fatalf("order %s\nwant  %s", got, want)
	}
	sent := ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalSent)
	if len(sent) != 7 {
		t.Fatalf("notices %+v", sent)
	}
	var drift string
	for _, n := range sent {
		if n.About == D1 {
			drift = n.Text
		}
	}
	if drift != "Your queued drift resolve of "+D1+" reached the team once the server was back." {
		t.Fatalf("drift notice %q", drift)
	}
}

// A transient error on one call of a batch (no answer for it) stops the outbox after that batch: the later batch
// and the human act wait, and a later flush sends the kept one first, then the rest, in order. Sad paths: a call
// with no answer, then a 429 on the human act itself (it and everything after it wait).
func TestFlushTransientErrorStopsLaterItemsWait(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	ids := notes(t, st, 0, protocol.MaxBatch+5)
	driftAct(t, st, "da_1", "drift.resolve", D1, "s1")
	tail := notes(t, st, 100, 2)
	stuck := ids[10]
	f.set(func() {
		f.callErr = func(id string) *protocol.RpcError {
			if id == stuck {
				return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "try later"}
			}
			return nil
		}
	})
	_, err := Push(context.Background(), st, client(srv), ob)
	if err == nil {
		t.Fatal("a transient error was not returned")
	}
	if f.requests != 1 {
		t.Fatalf("%d requests: the outbox went on past the transient error", f.requests)
	}
	left := spooled(t, st)
	if left[0] != stuck || left[1] != ids[protocol.MaxBatch] || len(left) != 1+5+1+2 {
		t.Fatalf("left %v", left)
	}
	// Back: the stuck one goes, then the rest; the human act is now refused a slot by a 429, so it and the tail wait.
	f.set(func() {
		f.callErr = nil
	})
	f.set(func() {
		f.fail = func(n int) int {
			if n == 3 {
				return http.StatusTooManyRequests
			}
			return 0
		}
	})
	_, err = Push(context.Background(), st, client(srv), ob)
	if !errors.Is(err, api.ErrOffline) {
		t.Fatalf("429: %v", err)
	}
	if got := strings.Join(spooled(t, st), ","); got != "da_1,"+strings.Join(tail, ",") {
		t.Fatalf("after the 429 the outbox holds %s", got)
	}
	f.set(func() {
		f.fail = nil
	})
	_, err = Push(context.Background(), st, client(srv), ob)
	must(t, err)
	order := strings.Join(f.order, ",")
	if !strings.HasSuffix(order, "drift.resolve "+D1+","+strings.Join(tail, ",")) {
		t.Fatalf("order %s", order)
	}
	if len(f.results) != len(ids)+len(tail) || len(spooled(t, st)) != 0 {
		t.Fatalf("%d results, left %v", len(f.results), spooled(t, st))
	}
}

// A permanent refusal mid-batch drops only that item (its session is told why), and everything after it is sent:
// the rest of the batch, the next batch, the human act after them. Sad paths: a refused signal, a refused drift act
// (DRIFT_CLOSED), an unknown method in the outbox and an unreadable drift act (both never sendable: dropped with a
// notice).
func TestFlushRefusalMidBatchDropsOnlyIt(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	ids := notes(t, st, 0, protocol.MaxBatch+3)
	driftAct(t, st, "da_1", "drift.dismiss", D1, "s1")
	must(t, st.OutboxAdd("odd_1", "topic.delete", "s1", ob, `{}`, t0))
	must(t, st.OutboxAdd("odd_2", "drift.resolve", "s1", ob, `{not json`, t0))
	last := notes(t, st, 200, 1)
	bad := ids[7]
	f.set(func() {
		f.callErr = func(id string) *protocol.RpcError {
			switch id {
			case bad:
				return &protocol.RpcError{Code: protocol.CodeInvalidParams, Message: "signal.title: too long"}
			case D1:
				return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "already settled", Data: map[string]any{"reason": "DRIFT_CLOSED"}}
			}
			return nil
		}
	})
	n, err := Push(context.Background(), st, client(srv), ob)
	must(t, err)
	if n != len(ids)+4 || len(spooled(t, st)) != 0 {
		t.Fatalf("sent %d, left %v", n, spooled(t, st))
	}
	if !strings.HasSuffix(strings.Join(f.order, ","), "drift.dismiss "+D1+","+last[0]) {
		t.Fatalf("order %v", f.order)
	}
	rej := ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalRejected)
	texts := map[string]string{}
	for _, n := range rej {
		texts[n.About] = n.Text
	}
	if len(rej) != 4 || !strings.Contains(texts[bad], "too long") ||
		texts[D1] != "Your drift dismiss of "+D1+" was rejected after it was queued: DRIFT_CLOSED. already settled" ||
		texts["odd_1"] == "" || texts["odd_2"] == "" {
		t.Fatalf("rejections %+v", rej)
	}
}

// Each batch is settled before the next one goes out: when the second request arrives, the first batch's items
// have already left the outbox (a crash here resends none of them). Then the answer to the second is lost: it
// stays, and the next flush sends it again (the same ids, one result each).
func TestFlushSettlesEachBatchBeforeTheNext(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	ids := notes(t, st, 0, 2*protocol.MaxBatch+1)
	other := reopen(t, st)
	var seen []int
	f.onReq = func(n int) {
		items, err := other.Spooled(ob) // another handle, as another process would read it
		if err != nil {
			t.Errorf("read: %v", err)
		}
		f.mu.Lock()
		seen = append(seen, len(items))
		f.mu.Unlock()
	}
	f.set(func() {
		f.lose = func(n int) bool { return n == 2 }
	})
	if _, err := Push(context.Background(), st, client(srv), ob); !errors.Is(err, api.ErrOffline) {
		t.Fatalf("lost answer: %v", err)
	}
	f.mu.Lock()
	sizes := fmt.Sprint(seen)
	f.mu.Unlock()
	if sizes != fmt.Sprint([]int{len(ids), len(ids) - protocol.MaxBatch}) {
		t.Fatalf("outbox sizes as each request arrived: %v", seen)
	}
	if got := spooled(t, st); len(got) != protocol.MaxBatch+1 || got[0] != ids[protocol.MaxBatch] {
		t.Fatalf("left %d", len(got))
	}
	f.set(func() {
		f.lose = nil
	})
	_, err := Push(context.Background(), st, client(srv), ob)
	must(t, err)
	if len(f.results) != len(ids) || len(spooled(t, st)) != 0 {
		t.Fatalf("%d results for %d signals", len(f.results), len(ids))
	}
	if len(ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalSent)) != len(ids) {
		t.Fatal("a signal told twice or never")
	}
}

// The caller's own items (mine) are answered to it, not noticed; the others are noticed. Sad path: one of mine
// refused (its error handed back, no notice), one of mine kept by an offline stop (no answer: still queued).
func TestFlushAnswersTheCallersOwnItems(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	notes(t, st, 0, 1)
	spool(t, st, S1, protocol.SignalKindDecision, "mine")
	spool(t, st, S2, protocol.SignalKindDecision, "mine, refused")
	f.set(func() {
		f.callErr = func(id string) *protocol.RpcError {
			if id == S2 {
				return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "no such topic", Data: map[string]any{"reason": "UNKNOWN_TOPIC"}}
			}
			return nil
		}
	})
	got, err := Flush(context.Background(), st, client(srv), ob, map[string]bool{S1: true, S2: true})
	must(t, err)
	if got.Sent != 3 || len(got.Answers) != 2 {
		t.Fatalf("flushed %+v", got)
	}
	var r protocol.SignalResult
	must(t, json.Unmarshal(got.Answers[S1].Result, &r))
	var ae *api.APIError
	if !r.Accepted || !errors.As(got.Answers[S2].Err, &ae) || ae.Reason != "UNKNOWN_TOPIC" {
		t.Fatalf("answers %+v", got.Answers)
	}
	ns := notices(t, st, "s1")
	if len(ns) != 1 || ns[0].About != "sg_000" {
		t.Fatalf("notices %+v: mine are answered, not noticed", ns)
	}
	// Offline: mine stays queued, with no answer.
	spool(t, st, S3, protocol.SignalKindDecision, "mine, offline")
	f.set(func() {
		f.fail = func(int) int { return http.StatusBadGateway }
	})
	got, err = Flush(context.Background(), st, client(srv), ob, map[string]bool{S3: true})
	if !errors.Is(err, api.ErrOffline) || len(got.Answers) != 0 || len(spooled(t, st)) != 1 {
		t.Fatalf("offline: %+v %v", got, err)
	}
}

// Items queued while a flush runs (by another command, a hook) go out in the same flush, after the ones before
// them: the holder of the sender lock sends what others leave queued.
func TestFlushSendsWhatIsQueuedWhileItRuns(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	notes(t, st, 0, 1)
	other := reopen(t, st)
	f.onReq = func(n int) {
		if n == 1 {
			b, _ := json.Marshal(map[string]any{"drift": D1, "note": "x"})
			if err := other.OutboxAdd("da_late", "drift.resolve", "s1", ob, string(b), t0); err != nil {
				t.Errorf("queue: %v", err)
			}
		}
	}
	n, err := Push(context.Background(), st, client(srv), ob)
	must(t, err)
	if n != 2 || strings.Join(f.order, ",") != "sg_000,drift.resolve "+D1 {
		t.Fatalf("sent %d, order %v", n, f.order)
	}
}

// ------------------------------------------------------------------ giving up, and drift acts resent

func at(t *testing.T, tm time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return tm }
	t.Cleanup(func() { now = old })
}

func item(t *testing.T, st *store.Store, id string) store.SpoolItem {
	t.Helper()
	items, err := st.Spooled(ob)
	must(t, err)
	for _, it := range items {
		if it.Signal == id {
			return it
		}
	}
	t.Fatalf("%s not queued", id)
	return store.SpoolItem{}
}

// The server keeps failing on the head item (an internal error for it, every time): it holds the rest back while
// it has failed fewer than GiveUpFails times or for less than GiveUpAfter; past both it is dropped, its session
// told, and the rest goes in the same flush. Sad paths: ten failures within a day (kept), a day with fewer than
// ten (kept), then both (dropped).
func TestFlushGivesUpOnAnItemTheServerKeepsFailingOn(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	spool(t, st, S1, protocol.SignalKindDecision, "Poison")
	driftAct(t, st, "da_1", "drift.resolve", D1, "s1") // waits behind it
	f.set(func() {
		f.callErr = func(id string) *protocol.RpcError {
			if id == S1 {
				return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "boom"}
			}
			return nil
		}
	})
	for i := range GiveUpFails {
		at(t, t0.Add(time.Duration(i)*time.Hour))
		if _, err := Push(context.Background(), st, client(srv), ob); err == nil {
			t.Fatalf("try %d: no error", i)
		}
	}
	it := item(t, st, S1)
	if it.Fails != GiveUpFails || it.Tries != GiveUpFails || !it.FirstFail.Equal(t0) || !strings.Contains(it.LastError, "boom") {
		t.Fatalf("after ten tries in ten hours: %+v", it)
	}
	at(t, t0.Add(GiveUpAfter+time.Hour))
	n, err := Push(context.Background(), st, client(srv), ob)
	must(t, err)
	if n != 2 || len(spooled(t, st)) != 0 {
		t.Fatalf("sent %d, left %v", n, spooled(t, st))
	}
	rej := ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalRejected)
	if len(rej) != 1 || rej[0].About != S1 || !strings.Contains(rej[0].Text, "could not be sent") ||
		!strings.Contains(rej[0].Text, "record it again") {
		t.Fatalf("notices %+v", rej)
	}
	if strings.Join(ofTypeAbout(notices(t, st, "s1"), protocol.NoticeTypeSignalSent), ",") != D1 {
		t.Fatal("the item after it was not sent")
	}
	if strings.Count(strings.Join(f.got(), ","), "drift.resolve") != 1 {
		t.Fatalf("order %v: the act went before the give-up or twice", f.got())
	}
	// A day of failures but fewer than ten: kept.
	st2 := openStore(t)
	spoolWith(t, st2, S3, protocol.SignalKindNote, "slow poison", ob)
	f.set(func() {
		f.callErr = func(string) *protocol.RpcError {
			return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "boom"}
		}
	})
	for i := range 3 {
		at(t, t0.Add(time.Duration(i)*48*time.Hour))
		_, _ = Push(context.Background(), st2, client(srv), ob)
	}
	if it := item(t, st2, S3); it.Fails != 3 {
		t.Fatalf("three failures over four days: %+v", it)
	}
}

func ofTypeAbout(ns []store.Notice, typ protocol.NoticeType) []string {
	var out []string
	for _, n := range ofType(ns, typ) {
		out = append(out, n.About)
	}
	return out
}

// Only the server failing on the item counts: an unreachable server, 502/503/504, 429, UPGRADE_REQUIRED and a key
// no longer a member's never do, however long they last (nothing is lost offline). An HTTP 500 and an answer
// this client can't read do, for every item of the request.
func TestFlushCountsOnlyTheServerFailingOnTheItem(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	spool(t, st, S1, protocol.SignalKindNote, "one")
	spool(t, st, S2, protocol.SignalKindNote, "two")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	down := "http://" + l.Addr().String()
	must(t, l.Close())
	for i := range 3 * GiveUpFails {
		at(t, t0.Add(time.Duration(i)*6*time.Hour)) // a week and more
		_, _ = Push(context.Background(), st, api.New(down, key, 200*time.Millisecond), ob)
	}
	for _, code := range []int{502, 503, 504, 429} {
		f.set(func() {
			f.fail = func(int) int { return code }
		})
		_, _ = Push(context.Background(), st, client(srv), ob)
	}
	f.set(func() {
		f.fail = nil
	})
	for _, reason := range []string{"UPGRADE_REQUIRED", "NOT_A_MEMBER"} {
		f.set(func() {
			f.callErr = func(string) *protocol.RpcError {
				return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "no", Data: map[string]any{"reason": reason}}
			}
		})
		_, _ = Push(context.Background(), st, client(srv), ob)
	}
	f.set(func() {
		f.callErr = nil
	})
	if it := item(t, st, S1); it.Fails != 0 || len(spooled(t, st)) != 2 {
		t.Fatalf("an outage counted: %+v", it)
	}
	f.set(func() {
		f.fail = func(int) int { return 500 }
	})
	_, _ = Push(context.Background(), st, client(srv), ob)
	if a, b := item(t, st, S1), item(t, st, S2); a.Fails != 1 || b.Fails != 1 || !strings.Contains(a.LastError, "500") {
		t.Fatalf("a 500: %+v %+v", a, b)
	}
	f.set(func() {
		f.fail = nil
	})
	f.set(func() {
		f.answer = func(e protocol.SignalEmit, _ json.RawMessage) protocol.SignalResult { return protocol.SignalResult{} }
	})
	f.mu.Lock()
	f.results = map[string]json.RawMessage{}
	f.mu.Unlock()
	// An answer this client can't read: the fake answers a string for S1.
	f.set(func() {
		f.rawAnswer = func(id string) json.RawMessage {
			if id == S1 {
				return json.RawMessage(`"not a result"`)
			}
			return nil
		}
	})
	_, _ = Push(context.Background(), st, client(srv), ob)
	if a := item(t, st, S1); a.Fails != 2 {
		t.Fatalf("an unreadable answer: %+v", a)
	}
}

// drift.resolved for D1 as the server would send it after the act.
func resolvedPage(status protocol.DriftStatus, resolution string) page {
	d := protocol.NewDrift()
	d.ID, d.Project, d.Kind, d.Summary, d.Signals, d.Status = D1, P, protocol.DriftKindUnanswered, "x", []string{S1}, status
	d.Resolution, d.ResolvedBy, d.CreatedAt = resolution, []string{}, "2026-11-24T10:00:00Z"
	ev := protocol.NewDriftResolved()
	ev.Project, ev.Seq, ev.At, ev.Drift = P, 7, "2026-11-24T10:00:00Z", d
	b, _ := json.Marshal(ev)
	return page{Events: []json.RawMessage{b}, Cursors: map[string]int64{P: 7}, Reset: []string{}}
}

// A drift act whose answer was lost is sent again and refused as DRIFT_CLOSED. The replica is brought up to date:
// the drift closed with this act's status and note means the first send took, and the session hears it reached
// the team. Sad paths: closed otherwise (another status or note: the refusal stands), and DRIFT_CLOSED on the
// first send (no lookup; the refusal stands).
func TestDriftActResentAfterALostAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lose    bool
		status  protocol.DriftStatus
		note    string
		reached bool
		polled  bool
	}{
		{"closed by this act", true, protocol.DriftStatusResolved, "settled", true, true},
		{"closed by someone else", true, protocol.DriftStatusResolved, "another note", false, true},
		{"dismissed instead", true, protocol.DriftStatusDismissed, "settled", false, true},
		{"refused on the first send", false, protocol.DriftStatusResolved, "settled", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			f, srv := newFake(t)
			driftAct(t, st, "da_1", "drift.resolve", D1, "s1")
			f.set(func() {
				f.poll = func(p protocol.SyncPoll) any {
					if p.Cursors[P] >= 7 {
						return page{Events: []json.RawMessage{}, Cursors: map[string]int64{P: 7}, Reset: []string{}}
					}
					return resolvedPage(tc.status, tc.note)
				}
			})
			closed := &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "the drift is closed",
				Data: map[string]any{"reason": "DRIFT_CLOSED"}}
			if tc.lose {
				f.set(func() {
					f.lose = func(n int) bool { return n == 1 }
				})
				if _, err := Push(context.Background(), st, client(srv), ob); !errors.Is(err, api.ErrOffline) {
					t.Fatalf("lost answer: %v", err)
				}
				f.set(func() {
					f.lose = nil
				})
			}
			f.set(func() {
				f.callErr = func(string) *protocol.RpcError { return closed }
			})
			_, err := Push(context.Background(), st, client(srv), ob)
			must(t, err)
			if len(spooled(t, st)) != 0 {
				t.Fatal("still queued")
			}
			ns := notices(t, st, "s1")
			sent, rej := ofTypeAbout(ns, protocol.NoticeTypeSignalSent), ofType(ns, protocol.NoticeTypeSignalRejected)
			if tc.reached != (len(sent) == 1 && len(rej) == 0) || !tc.reached && (len(rej) != 1 || !strings.Contains(rej[0].Text, "DRIFT_CLOSED")) {
				t.Fatalf("sent %v rejected %+v", sent, rej)
			}
			if tc.polled != (len(f.polls) > 0) {
				t.Fatalf("polled %d times", len(f.polls))
			}
		})
	}
}
