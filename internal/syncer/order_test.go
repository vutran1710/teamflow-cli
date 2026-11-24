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
	"net/http"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

func driftAct(t *testing.T, st *store.Store, id, method, drift, session string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"drift": drift, "note": "settled"})
	must(t, st.OutboxAdd(id, method, session, api.KeyRef(key), string(b), t0))
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
	n, err := Push(context.Background(), st, client(srv))
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
	f.callErr = func(id string) *protocol.RpcError {
		if id == stuck {
			return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "try later"}
		}
		return nil
	}
	_, err := Push(context.Background(), st, client(srv))
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
	f.callErr = nil
	f.fail = func(n int) int {
		if n == 3 {
			return http.StatusTooManyRequests
		}
		return 0
	}
	_, err = Push(context.Background(), st, client(srv))
	if !errors.Is(err, api.ErrOffline) {
		t.Fatalf("429: %v", err)
	}
	if got := strings.Join(spooled(t, st), ","); got != "da_1,"+strings.Join(tail, ",") {
		t.Fatalf("after the 429 the outbox holds %s", got)
	}
	f.fail = nil
	_, err = Push(context.Background(), st, client(srv))
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
	must(t, st.OutboxAdd("odd_1", "topic.delete", "s1", api.KeyRef(key), `{}`, t0))
	must(t, st.OutboxAdd("odd_2", "drift.resolve", "s1", api.KeyRef(key), `{not json`, t0))
	last := notes(t, st, 200, 1)
	bad := ids[7]
	f.callErr = func(id string) *protocol.RpcError {
		switch id {
		case bad:
			return &protocol.RpcError{Code: protocol.CodeInvalidParams, Message: "signal.title: too long"}
		case D1:
			return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "already settled", Data: map[string]any{"reason": "DRIFT_CLOSED"}}
		}
		return nil
	}
	n, err := Push(context.Background(), st, client(srv))
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
	other, err := store.Open(st.Dir())
	must(t, err)
	defer other.Close()
	var seen []int
	f.onReq = func(n int) {
		items, err := other.Spooled(api.KeyRef(key)) // another handle, as another process would read it
		if err != nil {
			t.Errorf("read: %v", err)
		}
		f.mu.Lock()
		seen = append(seen, len(items))
		f.mu.Unlock()
	}
	f.lose = func(n int) bool { return n == 2 }
	if _, err := Push(context.Background(), st, client(srv)); !errors.Is(err, api.ErrOffline) {
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
	f.lose = nil
	_, err = Push(context.Background(), st, client(srv))
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
	f.callErr = func(id string) *protocol.RpcError {
		if id == S2 {
			return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "no such topic", Data: map[string]any{"reason": "UNKNOWN_TOPIC"}}
		}
		return nil
	}
	got, err := Flush(context.Background(), st, client(srv), map[string]bool{S1: true, S2: true})
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
	f.fail = func(int) int { return http.StatusBadGateway }
	got, err = Flush(context.Background(), st, client(srv), map[string]bool{S3: true})
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
	other, err := store.Open(st.Dir())
	must(t, err)
	defer other.Close()
	f.onReq = func(n int) {
		if n == 1 {
			b, _ := json.Marshal(map[string]any{"drift": D1, "note": "x"})
			if err := other.OutboxAdd("da_late", "drift.resolve", "s1", api.KeyRef(key), string(b), t0); err != nil {
				t.Errorf("queue: %v", err)
			}
		}
	}
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 2 || strings.Join(f.order, ",") != "sg_000,drift.resolve "+D1 {
		t.Fatalf("sent %d, order %v", n, f.order)
	}
}
