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
// no longer a member's never do, however long they last (nothing is lost offline), nor an unreadable answer
// inside a batch. A 500, an internal error and an unreadable answer for the head alone do.
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
	if a := item(t, st, S1); !a.Alone {
		t.Fatalf("the head after requests that failed whole: %+v", a)
	}
	// The head alone failing (a 500, an internal error): its own failure, counted (dropped only if an item behind
	// it is then answered: see TestStoreOnlyOutageDropsNothing).
	f.set(func() { f.fail = func(int) int { return 500 } })
	_, _ = Push(context.Background(), st, client(srv), ob)
	f.set(func() {
		f.fail = nil
		f.callErr = func(id string) *protocol.RpcError {
			if id == S1 {
				return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "boom"}
			}
			return nil
		}
	})
	_, _ = Push(context.Background(), st, client(srv), ob)
	if a, b := item(t, st, S1), item(t, st, S2); a.Fails != 2 || b.Fails != 0 {
		t.Fatalf("its own failures: %+v %+v", a, b)
	}
	f.set(func() { f.callErr = nil })
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
	if a := item(t, st, S1); a.Fails != 3 {
		t.Fatalf("an unreadable answer alone: %+v", a)
	}
	// Inside a batch, an unreadable answer never counts.
	must(t, st.SetAlone(S1, false))
	_, _ = Push(context.Background(), st, client(srv), ob)
	if a := item(t, st, S1); a.Fails != 3 {
		t.Fatalf("an unreadable answer in a batch counted: %+v", a)
	}
}

// driftServer makes the fake answer drift acts as the real server does (server/tf_server/drifts.py): an open drift
// is closed by the act (resolved_by: the signals named, or the drift's own signals when none); on a closed drift
// resolve is refused DRIFT_CLOSED and dismiss NOT_ALLOWED. sync.poll then carries drift.resolved.
func driftServer(t *testing.T, f *fake) (closeBy func(status protocol.DriftStatus, note string, by []string)) {
	t.Helper()
	var drift *protocol.Drift
	closeBy = func(status protocol.DriftStatus, note string, by []string) {
		d := protocol.NewDrift()
		d.ID, d.Project, d.Kind, d.Summary, d.Signals, d.Status = D1, P, protocol.DriftKindUnanswered, "x", []string{S1, S2}, status
		d.Resolution, d.ResolvedBy, d.CreatedAt = note, by, "2026-11-24T10:00:00Z"
		if len(by) == 0 {
			d.ResolvedBy = d.Signals
		}
		drift = &d
	}
	f.set(func() {
		f.driftAct = func(method string, params json.RawMessage) *protocol.RpcError {
			var in struct {
				Note       string   `json:"note"`
				ResolvedBy []string `json:"resolved_by"`
			}
			_ = json.Unmarshal(params, &in)
			if drift != nil {
				if method == "drift.dismiss" {
					return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "only the initiator dismisses an open drift",
						Data: map[string]any{"reason": "NOT_ALLOWED"}}
				}
				return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "the drift is closed",
					Data: map[string]any{"reason": "DRIFT_CLOSED"}}
			}
			status := protocol.DriftStatusResolved
			if method == "drift.dismiss" {
				status = protocol.DriftStatusDismissed
			}
			closeBy(status, in.Note, in.ResolvedBy)
			return nil
		}
		f.poll = func(p protocol.SyncPoll) any {
			if drift == nil || p.Cursors[P] >= 7 {
				return page{Events: []json.RawMessage{}, Cursors: map[string]int64{P: p.Cursors[P]}, Reset: []string{}}
			}
			ev := protocol.NewDriftResolved()
			ev.Project, ev.Seq, ev.At, ev.Drift = P, 7, "2026-11-24T10:00:00Z", *drift
			b, _ := json.Marshal(ev)
			return page{Events: []json.RawMessage{b}, Cursors: map[string]int64{P: 7}, Reset: []string{}}
		}
	})
	return closeBy
}

func queueDriftAct(t *testing.T, st *store.Store, method string, resolvedBy []string) {
	t.Helper()
	in := map[string]any{"drift": D1, "note": "settled"}
	if method == "drift.resolve" {
		in["resolved_by"] = resolvedBy
		if resolvedBy == nil {
			in["resolved_by"] = []string{}
		}
	}
	b, _ := json.Marshal(in)
	must(t, st.OutboxAdd("da_1", method, "s1", ob, string(b), t0))
}

// A drift act whose answer was lost is sent again; the server, as it really does, refuses it (DRIFT_CLOSED for a
// resolve, NOT_ALLOWED for a dismiss). The replica is brought up to date: the drift closed with this act's status
// and note (and the signals it named, when it named some) means the first send took, and the session hears it
// reached the team. Sad paths: a resolve naming no signals (the server fills them in), a dismiss refused
// NOT_ALLOWED, the first send never applied and someone else closing it meanwhile (the refusal stands), refused
// on the first send (no lookup: the refusal stands).
func TestDriftActResentAfterALostAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		named   []string
		first   string // "lost": applied, answer lost; "down": never applied; "closed": someone closed it before
		reached bool
		polled  bool
		reason  string
	}{
		{"resolve naming no signals", "drift.resolve", nil, "lost", true, true, ""},
		{"resolve naming signals", "drift.resolve", []string{S2}, "lost", true, true, ""},
		{"dismiss (NOT_ALLOWED on the resend)", "drift.dismiss", nil, "lost", true, true, ""},
		{"never applied, closed by someone else", "drift.resolve", nil, "down", false, true, "DRIFT_CLOSED"},
		{"dismiss never applied, closed by someone else", "drift.dismiss", nil, "down", false, true, "NOT_ALLOWED"},
		{"resolve refused on the first send", "drift.resolve", nil, "closed", false, false, "DRIFT_CLOSED"},
		{"dismiss refused on the first send", "drift.dismiss", nil, "closed", false, false, "NOT_ALLOWED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			f, srv := newFake(t)
			closeBy := driftServer(t, f)
			queueDriftAct(t, st, tc.method, tc.named)
			switch tc.first {
			case "lost":
				f.set(func() { f.lose = func(n int) bool { return n == 1 } })
				if _, err := Push(context.Background(), st, client(srv), ob); !errors.Is(err, api.ErrOffline) {
					t.Fatalf("lost answer: %v", err)
				}
				f.set(func() { f.lose = nil })
			case "down":
				f.set(func() { f.fail = func(n int) int { return 503 } })
				if _, err := Push(context.Background(), st, client(srv), ob); !errors.Is(err, api.ErrOffline) {
					t.Fatalf("down: %v", err)
				}
				f.set(func() { f.fail = nil; closeBy(protocol.DriftStatusResolved, "someone else's note", nil) })
			case "closed":
				f.set(func() { closeBy(protocol.DriftStatusResolved, "someone else's note", nil) })
			}
			_, err := Push(context.Background(), st, client(srv), ob)
			must(t, err)
			if len(spooled(t, st)) != 0 {
				t.Fatal("still queued")
			}
			ns := notices(t, st, "s1")
			sent, rej := ofTypeAbout(ns, protocol.NoticeTypeSignalSent), ofType(ns, protocol.NoticeTypeSignalRejected)
			if tc.reached != (len(sent) == 1 && len(rej) == 0) || !tc.reached && (len(rej) != 1 || !strings.Contains(rej[0].Text, tc.reason)) {
				t.Fatalf("sent %v rejected %+v", sent, rej)
			}
			f.mu.Lock()
			polled := len(f.polls) > 0
			f.mu.Unlock()
			if tc.polled != polled {
				t.Fatalf("polled: %v", polled)
			}
		})
	}
}

// A server failing on everything for more than a day (every call INTERNAL_ERROR, then whole requests 500) drops
// nothing: a request whose every call fails says nothing about any one item, and the head failing alone while the
// server answers nothing else is an outage too. Sad paths: per-call errors on every item of a batch, the head
// alone failing with member.me failing too, whole-request 500s.
func TestLongOutageDropsNothing(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	notes(t, st, 0, 3)
	spool(t, st, "sg_v", protocol.SignalKindApprove, "ok")
	f.set(func() { f.broken = &protocol.RpcError{Code: protocol.CodeInternalError, Message: "down"} })
	for i := range 30 {
		at(t, t0.Add(time.Duration(i)*time.Hour))
		if i == 15 {
			f.set(func() { f.broken = nil; f.fail = func(int) int { return 500 } })
		}
		_, _ = Push(context.Background(), st, client(srv), ob)
	}
	if got := spooled(t, st); len(got) != 4 {
		t.Fatalf("an outage dropped items: left %v", got)
	}
	if len(ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalRejected)) != 0 {
		t.Fatal("told of a drop")
	}
	shapes := f.shapes()
	if shapes[0] != "3" || shapes[1] != "1" {
		t.Fatalf("shapes %v: after a request where every call failed, the head goes alone", shapes)
	}
	// Back: everything goes, in order.
	f.set(func() { f.fail = nil })
	at(t, t0.Add(31*time.Hour))
	n, err := Push(context.Background(), st, client(srv), ob)
	must(t, err)
	if n != 4 {
		t.Fatalf("sent %d", n)
	}
}

// One bad item among good ones: it fails while others in its request succeed (its own failure), so it is counted
// and, past the limit, given up; the items after it go in order. When every call of its first request fails, it
// is sent alone next, and counted there.
func TestOneBadItemIsIsolatedAndGivenUp(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	ids := notes(t, st, 0, 2) // sg_000 good, sg_001 bad
	spool(t, st, "sg_v", protocol.SignalKindApprove, "ok")
	notes(t, st, 2, 1)
	bad := ids[1]
	all := true
	f.set(func() {
		f.callErr = func(id string) *protocol.RpcError {
			if id == bad || all {
				return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "boom"}
			}
			return nil
		}
	})
	at(t, t0)
	_, _ = Push(context.Background(), st, client(srv), ob) // both fail: nothing counted, the head marked alone
	if it := item(t, st, ids[0]); it.Fails != 0 || !it.Alone {
		t.Fatalf("after a request where every call failed: %+v", it)
	}
	f.set(func() { all = false })
	for i := range GiveUpFails + 1 {
		at(t, t0.Add(time.Duration(i+1)*3*time.Hour))
		_, _ = Push(context.Background(), st, client(srv), ob)
	}
	shapes := f.shapes()
	if shapes[1] != "1" {
		t.Fatalf("shapes %v: the head went alone after the all-failed request", shapes)
	}
	if got := spooled(t, st); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	rej := ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalRejected)
	if len(rej) != 1 || rej[0].About != bad {
		t.Fatalf("rejections %+v", rej)
	}
	order := strings.Join(f.got(), ",")
	if !strings.HasSuffix(order, bad+",sg_v,sg_002") {
		t.Fatalf("order %s", order)
	}
}

// A human act is never given up by the client, however long the server fails on it alone: only the person drops it
// (`teamflow outbox drop`). Its failures are still counted, for the person to see.
func TestHumanActIsNeverGivenUp(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	spool(t, st, "sg_v", protocol.SignalKindApprove, "ok")
	f.set(func() {
		f.callErr = func(string) *protocol.RpcError {
			return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "boom"}
		}
	})
	for i := range 3 * GiveUpFails {
		at(t, t0.Add(time.Duration(i)*4*time.Hour))
		_, _ = Push(context.Background(), st, client(srv), ob)
	}
	if it := item(t, st, "sg_v"); it.Fails != 3*GiveUpFails {
		t.Fatalf("the verdict: %+v", it)
	}
	if len(notices(t, st, "s1")) != 0 {
		t.Fatal("told of a drop")
	}
}

// Only the store is down (writes fail; everything else answers): every signal.emit fails, alone too, for 80 hours,
// first as an internal error per call, then as an HTTP 500. The head is counted past the limit, but the item
// behind it fails the same way, so nothing is dropped; a lone queued signal (nothing behind it) is never dropped.
// Sad paths: a lone item, six items, per-call errors, whole-request 500s on signal.emit only.
func TestStoreOnlyOutageDropsNothing(t *testing.T) {
	for _, n := range []int{1, 6} {
		t.Run(fmt.Sprint(n, " queued"), func(t *testing.T) {
			st := openStore(t)
			f, srv := newFake(t)
			ids := notes(t, st, 0, n)
			f.set(func() {
				f.callErr = func(string) *protocol.RpcError {
					return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "D1 down"}
				}
			})
			for i := range 80 {
				at(t, t0.Add(time.Duration(i)*time.Hour))
				if i == 40 {
					f.set(func() { f.callErr = nil; f.failEmit = 500 })
				}
				_, _ = Push(context.Background(), st, client(srv), ob)
			}
			if got := spooled(t, st); len(got) != n {
				t.Fatalf("a store outage dropped items: left %v", got)
			}
			if len(notices(t, st, "s1")) != 0 {
				t.Fatal("told of a drop")
			}
			if it := item(t, st, ids[0]); it.Fails < GiveUpFails {
				t.Fatalf("the head was not counted: %+v", it)
			}
			f.set(func() { f.failEmit = 0 })
			at(t, t0.Add(81*time.Hour))
			sent, err := Push(context.Background(), st, client(srv), ob)
			must(t, err)
			if sent != n {
				t.Fatalf("back: sent %d", sent)
			}
		})
	}
}
