// Tests of push, pull and apply: a port of client/tf_client/sync.py.
//
// Go twins of the Python tests in tests/client that cover push, pull, apply and notices (Python → Go):
//
//	test_api_batches.py
//	  test_a_long_emit_goes_in_batches_of_at_most_max_batch        → TestPushLongSpoolGoesInBatches
//	  test_upgrade_required_says_update_teamflow (emit part)       → TestPushUpgradeRequiredKeepsTheSpool
//	  test_python_client_sends_verdicts_alone                      → TestPushSendsVerdictsAlone
//	test_sync_reset.py
//	  test_python_client_drops_project_and_pulls_from_zero_on_reset → TestPullHonoursReset
//	test_e2e.py
//	  test_supersede_with_approval_and_stale_citation              → TestApplyCitedPendingChange, TestApplyStateChanged
//	  test_offline_spool_without_duplicates                         → TestOfflinePushKeepsSpool, TestPushIsIdempotent
//	  test_a_queued_signal_is_told_sent_once_back_online           → TestPushTellsTheSessionItReachedTheTeam
//	  test_topic_delete_reaches_replicas                            → TestApplyTopicDeleted
//	  test_update_run_reaches_sessions (drift part)                 → TestApplyDriftFound, TestApplyProjectUpdated
//	test_approvals.py
//	  test_raised_at_the_next_prompt_when_no_turn_ended (notice)    → TestApplyApprovalRequested
//	test_teamf_project.py
//	  test_approvals_follow_the_topics                              → TestApplyApprovalRequested
//	test_web_approvals.py
//	  test_a_verdict_or_drift_settled_in_the_web_app_reads_as_the_persons_own (drift text) → TestApplyDriftResolved
//	test_deliverables.py
//	  test_revoke_notice_to_owner                                   → TestApplyActionRevoked
//	  test_revoke_notice_only_for_the_owner                         → TestApplyActionRevoked
//	  test_archived_replica_gives_no_news_and_no_revoke_notice      → TestApplyActionRevoked
//	  test_archived_and_deleted_drop_out                            → TestApplyDeliverablesArchived, TestApplyDeliverablesDeleted
//	  test_archive_drops_unshown_due_notices_and_restore_brings_them_back (notice part) → TestApplyDeliverablesArchived
//	  test_a_revoke_queued_offline_is_refused_when_it_arrives      → TestPushRejectedAfterItWasQueued
//	  test_result_spools_offline_then_sends                         → TestOfflinePushKeepsSpool
//
// The rest of those files test hooks, the MCP tools, the CLI or the server; they are twinned by later tasks.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const (
	P   = "pj_00000000000000000000000001"
	P2  = "pj_00000000000000000000000002"
	M1  = "mb_00000000000000000000000001"
	M2  = "mb_00000000000000000000000002"
	M3  = "mb_00000000000000000000000003"
	T1  = "tp_00000000000000000000000001"
	T2  = "tp_00000000000000000000000002"
	S1  = "sg_00000000000000000000000001"
	S2  = "sg_00000000000000000000000002"
	S3  = "sg_00000000000000000000000003"
	D1  = "dr_00000000000000000000000001"
	A1  = "ai_00000000000000000000000001"
	A2  = "ai_00000000000000000000000002"
	key = "tfp_0123456789abcdefghijklmnopqrstuv"
)

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------------ a fake server

// fake is a Teamflow server: signal.emit keeps the first result per signal id (as the real one does), sync.poll
// answers from pages.
type fake struct {
	t        *testing.T
	mu       sync.Mutex
	bodies   []json.RawMessage                                                // every request body, in order
	results  map[string]json.RawMessage                                       // signal id -> the first result
	answer   func(protocol.SignalEmit, json.RawMessage) protocol.SignalResult // nil: accept, IN_FORCE
	callErr  func(sig string) *protocol.RpcError                              // a per-call error
	poll     func(protocol.SyncPoll) any                                      // sync.poll's answer
	polls    []protocol.SyncPoll
	fail     func(n int) int  // HTTP status to answer request n (1-based) with, 0: answer as usual
	lose     func(n int) bool // handle request n, then drop the connection (the answer is lost)
	requests int
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{t: t, results: map[string]json.RawMessage{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	n := f.requests
	f.bodies = append(f.bodies, json.RawMessage(body))
	if f.fail != nil {
		if code := f.fail(n); code != 0 {
			w.WriteHeader(code)
			return
		}
	}
	one := func(req protocol.Request) protocol.Response {
		out := protocol.Response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "signal.emit":
			var raw struct {
				Signal struct {
					ID string `json:"id"`
				} `json:"signal"`
			}
			if err := json.Unmarshal(req.Params, &raw); err != nil {
				f.t.Errorf("emit params: %v", err)
			}
			if f.callErr != nil {
				if e := f.callErr(raw.Signal.ID); e != nil {
					out.Error = e
					return out
				}
			}
			if prev, ok := f.results[raw.Signal.ID]; ok { // a retry: the stored first result
				out.Result = prev
				return out
			}
			var emit protocol.SignalEmit
			if err := json.Unmarshal(req.Params, &emit); err != nil {
				f.t.Errorf("emit params: %v", err)
			}
			res := protocol.SignalResult{Signal: raw.Signal.ID, Accepted: true, State: ptr(protocol.SignalStateInForce)}
			if f.answer != nil {
				res = f.answer(emit, req.Params)
			}
			b, _ := json.Marshal(res)
			f.results[raw.Signal.ID] = b
			out.Result = b
		case "sync.poll":
			var p protocol.SyncPoll
			if err := json.Unmarshal(req.Params, &p); err != nil {
				f.t.Errorf("poll params: %v", err)
			}
			f.polls = append(f.polls, p)
			b, _ := json.Marshal(f.poll(p))
			out.Result = b
		default:
			out.Error = &protocol.RpcError{Code: protocol.CodeMethodNotFound, Message: "no such method"}
		}
		return out
	}
	var payload any
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var reqs []protocol.Request
		if err := json.Unmarshal(body, &reqs); err != nil {
			f.t.Errorf("bad batch: %v", err)
		}
		outs := []protocol.Response{}
		for _, q := range reqs {
			outs = append(outs, one(q))
		}
		payload = outs
	} else {
		var req protocol.Request
		if err := json.Unmarshal(body, &req); err != nil {
			f.t.Errorf("bad request: %v", err)
		}
		payload = one(req)
	}
	if f.lose != nil && f.lose(n) {
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// shapes is each request's shape: the batch length, or "alone".
func (f *fake) shapes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.bodies {
		if strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
			var calls []json.RawMessage
			_ = json.Unmarshal(b, &calls)
			out = append(out, fmt.Sprint(len(calls)))
			continue
		}
		out = append(out, "alone")
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func client(srv *httptest.Server) *api.Client { return api.New(srv.URL, key, 5*time.Second) }

// spool queues a signal of kind for session s1 with the test key.
func spool(t *testing.T, st *store.Store, id string, kind protocol.SignalKind, title string) {
	t.Helper()
	spoolWith(t, st, id, kind, title, api.KeyRef(key))
}

func spoolWith(t *testing.T, st *store.Store, id string, kind protocol.SignalKind, title, ref string) {
	t.Helper()
	e := protocol.NewSignalEmit()
	e.ClientSession, e.Actor, e.ProjectVersion = "s1", protocol.ActorAgent, 1
	e.Signal = protocol.NewSignal()
	e.Signal.ID, e.Signal.Project, e.Signal.Topic, e.Signal.Kind, e.Signal.Title = id, P, T1, kind, title
	e.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "why"}
	if kind == protocol.SignalKindApprove || kind == protocol.SignalKindReject {
		e.Signal.Target = ptr(S1)
	}
	b, err := json.Marshal(e)
	must(t, err)
	must(t, st.SpoolAdd(id, "s1", ref, string(b), t0))
}

func spooled(t *testing.T, st *store.Store) []string {
	t.Helper()
	items, err := st.Spooled(api.KeyRef(key))
	must(t, err)
	var out []string
	for _, it := range items {
		out = append(out, it.Signal)
	}
	return out
}

func notices(t *testing.T, st *store.Store, session string) []store.Notice {
	t.Helper()
	ns, err := st.Notices(session, false)
	must(t, err)
	return ns
}

func ofType(ns []store.Notice, typ protocol.NoticeType) []store.Notice {
	var out []store.Notice
	for _, n := range ns {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	return out
}

// ------------------------------------------------------------------ push

func TestPushTellsTheSessionItReachedTheTeam(t *testing.T) {
	st := openStore(t)
	_, srv := newFake(t)
	spool(t, st, S1, protocol.SignalKindDecision, "Round prices to 0.05")
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 1 {
		t.Fatalf("pushed %d, want 1", n)
	}
	if got := spooled(t, st); len(got) != 0 {
		t.Fatalf("spool still holds %v", got)
	}
	ns := notices(t, st, "s1")
	if len(ns) != 1 || ns[0].Type != protocol.NoticeTypeSignalSent || ns[0].About != S1 {
		t.Fatalf("notices %+v", ns)
	}
	want := `Your queued signal ` + S1 + ` "Round prices to 0.05" reached the team once the server was back: accepted (IN_FORCE).`
	if ns[0].Text != want {
		t.Fatalf("text %q\nwant %q", ns[0].Text, want)
	}
	if n, err := Push(context.Background(), st, client(srv)); err != nil || n != 0 { // told once; nothing left
		t.Fatalf("second push: %d, %v", n, err)
	}
	if len(notices(t, st, "s1")) != 1 {
		t.Fatal("told twice")
	}
}

func TestPushRejectedAfterItWasQueued(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	f.answer = func(e protocol.SignalEmit, _ json.RawMessage) protocol.SignalResult {
		return protocol.SignalResult{Signal: e.Signal.ID, Reason: ptr(protocol.RejectReasonUnknownAction),
			Message: "the action is not done: nothing to revoke"}
	}
	spool(t, st, S1, protocol.SignalKindActionRevoke, "Revoke")
	_, err := Push(context.Background(), st, client(srv))
	must(t, err)
	ns := notices(t, st, "s1")
	want := "Your signal " + S1 + " was rejected after it was queued: UNKNOWN_ACTION. the action is not done: nothing to revoke"
	if len(ns) != 1 || ns[0].Type != protocol.NoticeTypeSignalRejected || ns[0].Text != want {
		t.Fatalf("notices %+v", ns)
	}
	if len(spooled(t, st)) != 0 {
		t.Fatal("a rejected signal stays queued")
	}
}

func TestPushRejectedWithoutAReason(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	f.answer = func(e protocol.SignalEmit, _ json.RawMessage) protocol.SignalResult {
		return protocol.SignalResult{Signal: e.Signal.ID}
	}
	spool(t, st, S1, protocol.SignalKindNote, "n")
	_, err := Push(context.Background(), st, client(srv))
	must(t, err)
	ns := notices(t, st, "s1")
	if len(ns) != 1 || ns[0].Text != "Your signal "+S1+" was rejected after it was queued: rejected." {
		t.Fatalf("notices %+v", ns)
	}
}

func TestPushIsIdempotent(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	// The server takes the batch, then the answer is lost on the way back: as far as the client knows, it is offline.
	f.lose = func(n int) bool { return n == 1 }
	spool(t, st, S1, protocol.SignalKindDecision, "One")
	spool(t, st, S2, protocol.SignalKindDecision, "Two")
	if _, err := Push(context.Background(), st, client(srv)); !errors.Is(err, api.ErrOffline) {
		t.Fatalf("lost answer: %v, want ErrOffline", err)
	}
	if got := spooled(t, st); strings.Join(got, ",") != S1+","+S2 {
		t.Fatalf("after a lost answer the spool holds %v", got)
	}
	if len(notices(t, st, "s1")) != 0 {
		t.Fatal("told before the answer arrived")
	}
	// The flush again: the same ids, the stored first results.
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 2 || len(spooled(t, st)) != 0 {
		t.Fatalf("flush: %d, spool %v", n, spooled(t, st))
	}
	if len(f.results) != 2 {
		t.Fatalf("server holds %d results, want 2", len(f.results))
	}
	// Queued again (a retry from the agent): one result, and the session is told once.
	spool(t, st, S1, protocol.SignalKindDecision, "One")
	spool(t, st, S1, protocol.SignalKindDecision, "One") // the spool keeps one
	if got := spooled(t, st); len(got) != 1 {
		t.Fatalf("spool %v", got)
	}
	n, err = Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 1 || len(f.results) != 2 {
		t.Fatalf("retry: %d pushed, %d results on the server", n, len(f.results))
	}
	sent := ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalSent)
	if len(sent) != 2 {
		t.Fatalf("SIGNAL_SENT notices %+v, want one per signal", sent)
	}
}

func TestOfflinePushKeepsSpool(t *testing.T) {
	st := openStore(t)
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	down := "http://" + l.Addr().String()
	must(t, l.Close())
	ids := []string{S1, S2, S3}
	for i, id := range ids {
		spool(t, st, id, protocol.SignalKindNote, fmt.Sprintf("n%d", i))
	}
	spool(t, st, "sg_verdict", protocol.SignalKindApprove, "ok") // a verdict waits too
	for range 2 {
		n, err := Push(context.Background(), st, api.New(down, key, time.Second))
		if !errors.Is(err, api.ErrOffline) || n != 0 {
			t.Fatalf("offline push: %d, %v", n, err)
		}
	}
	if got := strings.Join(spooled(t, st), ","); got != strings.Join(append(ids, "sg_verdict"), ",") {
		t.Fatalf("spool %v", got)
	}
	if len(notices(t, st, "s1")) != 0 {
		t.Fatal("notices while offline")
	}
	// Back online: everything goes out once.
	f, srv := newFake(t)
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 4 || len(spooled(t, st)) != 0 || len(f.results) != 4 {
		t.Fatalf("flush: %d, spool %v, results %d", n, spooled(t, st), len(f.results))
	}
}

func TestPushOfflineMidwayKeepsTheRest(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	f.fail = func(n int) int {
		if n == 2 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	for i := range 120 {
		spool(t, st, fmt.Sprintf("sg_%03d", i), protocol.SignalKindNote, "n")
	}
	n, err := Push(context.Background(), st, client(srv))
	if !errors.Is(err, api.ErrOffline) || n != protocol.MaxBatch {
		t.Fatalf("push: %d, %v", n, err)
	}
	left := spooled(t, st)
	if len(left) != 120-protocol.MaxBatch || left[0] != fmt.Sprintf("sg_%03d", protocol.MaxBatch) {
		t.Fatalf("left %d, first %v", len(left), left[:1])
	}
	f.fail = nil
	n, err = Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 70 || len(spooled(t, st)) != 0 || len(f.results) != 120 {
		t.Fatalf("flush: %d, results %d", n, len(f.results))
	}
}

func TestPushLongSpoolGoesInBatches(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	total := 2*protocol.MaxBatch + 7
	for i := range total {
		spool(t, st, fmt.Sprintf("sg_%03d", i), protocol.SignalKindNote, "n")
	}
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != total {
		t.Fatalf("pushed %d", n)
	}
	if got := strings.Join(f.shapes(), ","); got != fmt.Sprintf("%d,%d,7", protocol.MaxBatch, protocol.MaxBatch) {
		t.Fatalf("requests %s", got)
	}
}

func TestPushSendsVerdictsAlone(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	kinds := []protocol.SignalKind{protocol.SignalKindNote, protocol.SignalKindNote, protocol.SignalKindApprove,
		protocol.SignalKindNote, protocol.SignalKindReject}
	for i, k := range kinds {
		spool(t, st, fmt.Sprintf("sg_%d", i), k, "t")
	}
	var order []string
	f.answer = func(e protocol.SignalEmit, _ json.RawMessage) protocol.SignalResult {
		order = append(order, e.Signal.ID)
		return protocol.SignalResult{Signal: e.Signal.ID, Accepted: true}
	}
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 5 {
		t.Fatalf("pushed %d", n)
	}
	if got := strings.Join(f.shapes(), ","); got != "2,alone,1,alone" {
		t.Fatalf("requests %s, want 2,alone,1,alone", got)
	}
	if strings.Join(order, ",") != "sg_0,sg_1,sg_2,sg_3,sg_4" {
		t.Fatalf("sent out of order: %v", order)
	}
	for _, b := range f.bodies {
		if strings.HasPrefix(string(b), "[") && (strings.Contains(string(b), `"APPROVE"`) || strings.Contains(string(b), `"REJECT"`)) {
			t.Fatalf("a verdict in a batch: %s", b)
		}
	}
}

func TestPushOnlyThisKeysSpool(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	spool(t, st, S1, protocol.SignalKindNote, "mine")
	spoolWith(t, st, S2, protocol.SignalKindNote, "another key's", api.KeyRef("tfp_another"))
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 1 || len(f.results) != 1 {
		t.Fatalf("pushed %d", n)
	}
	other, err := st.Spooled(api.KeyRef("tfp_another"))
	must(t, err)
	if len(other) != 1 || other[0].Signal != S2 {
		t.Fatalf("another key's spool %+v", other)
	}
}

func TestPushEmptySpoolSendsNothing(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	n, err := Push(context.Background(), st, client(srv))
	if err != nil || n != 0 || f.requests != 0 {
		t.Fatalf("%d, %v, %d requests", n, err, f.requests)
	}
}

func TestPushUpgradeRequiredKeepsTheSpool(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	f.callErr = func(string) *protocol.RpcError {
		return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "update teamflow: this server needs client protocol ≥ 2",
			Data: map[string]any{"reason": "UPGRADE_REQUIRED"}}
	}
	spool(t, st, S1, protocol.SignalKindNote, "n")
	spool(t, st, S2, protocol.SignalKindApprove, "ok")
	_, err := Push(context.Background(), st, client(srv))
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err %v, want ErrUpgradeRequired", err)
	}
	if !strings.HasPrefix(err.Error(), "update teamflow") {
		t.Fatalf("message %q", err)
	}
	if len(spooled(t, st)) != 2 || len(notices(t, st, "s1")) != 0 {
		t.Fatal("UPGRADE_REQUIRED is not a rejection: the spool waits for the new client")
	}
}

func TestPushPerCallRefusalDropsOnlyThatSignal(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	f.callErr = func(sig string) *protocol.RpcError {
		switch sig {
		case S1: // refused for good
			return &protocol.RpcError{Code: protocol.CodeInvalidParams, Message: "signal.title: too long"}
		case S2: // the key is no longer a member's: keep it, the caller drops the key
			return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "not a member", Data: map[string]any{"reason": "NOT_A_MEMBER"}}
		}
		return nil
	}
	spool(t, st, S1, protocol.SignalKindNote, "bad")
	spool(t, st, S2, protocol.SignalKindNote, "keyless")
	spool(t, st, S3, protocol.SignalKindNote, "fine")
	n, err := Push(context.Background(), st, client(srv))
	var ae *api.APIError
	if !errors.As(err, &ae) || ae.Reason != "NOT_A_MEMBER" {
		t.Fatalf("err %v, want the NOT_A_MEMBER refusal", err)
	}
	if n != 2 {
		t.Fatalf("settled %d, want 2", n)
	}
	if got := spooled(t, st); len(got) != 1 || got[0] != S2 {
		t.Fatalf("spool %v", got)
	}
	ns := notices(t, st, "s1")
	rej := ofType(ns, protocol.NoticeTypeSignalRejected)
	if len(rej) != 1 || rej[0].About != S1 || !strings.Contains(rej[0].Text, "too long") {
		t.Fatalf("notices %+v", ns)
	}
	if len(ofType(ns, protocol.NoticeTypeSignalSent)) != 1 {
		t.Fatalf("notices %+v", ns)
	}
}

func TestPushUnreadableSpoolItemIsDropped(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	must(t, st.SpoolAdd(S1, "s1", api.KeyRef(key), "{not json", t0))
	spool(t, st, S2, protocol.SignalKindNote, "fine")
	n, err := Push(context.Background(), st, client(srv))
	must(t, err)
	if n != 2 || len(spooled(t, st)) != 0 || len(f.results) != 1 {
		t.Fatalf("%d pushed, spool %v, %d sent", n, spooled(t, st), len(f.results))
	}
	rej := ofType(notices(t, st, "s1"), protocol.NoticeTypeSignalRejected)
	if len(rej) != 1 || rej[0].About != S1 {
		t.Fatalf("notices %+v", rej)
	}
}

func TestPushOnAClosedStoreFails(t *testing.T) {
	st, err := store.Open(t.TempDir())
	must(t, err)
	f, srv := newFake(t)
	st.Close()
	if _, err := Push(context.Background(), st, client(srv)); err == nil {
		t.Fatal("no error from a closed store")
	}
	if f.requests != 0 {
		t.Fatal("sent with no spool read")
	}
}

// ------------------------------------------------------------------ pull

func golden(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "protocol", "golden", name+".json"))
	must(t, err)
	var g struct {
		Event json.RawMessage `json:"event"`
	}
	must(t, json.Unmarshal(b, &g))
	return g.Event
}

func event(t *testing.T, name string) protocol.SyncEvent {
	t.Helper()
	ev, err := protocol.DecodeSyncEvent(golden(t, name))
	must(t, err)
	return ev
}

// page is a sync.poll answer with raw events.
type page struct {
	Events  []json.RawMessage `json:"events"`
	Cursors map[string]int64  `json:"cursors"`
	Reset   []string          `json:"reset"`
}

func TestPullAppliesEventsAndMovesCursors(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	evs := []json.RawMessage{golden(t, "event.project.updated.1"), golden(t, "event.topic.updated.1"), golden(t, "event.signal.accepted.1")}
	f.poll = func(p protocol.SyncPoll) any {
		if p.Cursors[P] == 0 {
			return page{Events: evs, Cursors: map[string]int64{P: 5}, Reset: []string{}}
		}
		return page{Events: []json.RawMessage{}, Cursors: map[string]int64{P: 5}, Reset: []string{}}
	}
	n, err := Pull(context.Background(), st, client(srv), time.Second)
	must(t, err)
	if n != 3 {
		t.Fatalf("pulled %d", n)
	}
	if c := st.Cursors(); c[P] != 5 {
		t.Fatalf("cursors %v", c)
	}
	if _, ok, _ := st.Signal(S1); !ok {
		t.Fatal("signal not in the replica")
	}
	if len(f.polls) != 2 || f.polls[0].Wait != 0 || f.polls[1].Cursors[P] != 5 {
		t.Fatalf("polls %+v", f.polls)
	}
	// nothing new
	n, err = Pull(context.Background(), st, client(srv), time.Second)
	if err != nil || n != 0 {
		t.Fatalf("again: %d, %v", n, err)
	}
}

func TestPullHonoursReset(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	// The server's project P: three events (head 3). Project P2 is left alone.
	project := golden(t, "event.project.updated.1")
	topic := golden(t, "event.topic.updated.1")
	sig := golden(t, "event.signal.accepted.1")
	head := int64(3)
	var extra []json.RawMessage
	f.poll = func(p protocol.SyncPoll) any {
		cur, ok := p.Cursors[P]
		switch {
		case ok && cur > head+int64(len(extra)): // beyond the head: a restored server
			return page{Events: []json.RawMessage{}, Cursors: map[string]int64{P: head + int64(len(extra))}, Reset: []string{P}}
		case cur == 0:
			return page{Events: append([]json.RawMessage{project, topic, sig}, extra...), Cursors: map[string]int64{P: head + int64(len(extra))}, Reset: []string{}}
		case cur < head+int64(len(extra)):
			return page{Events: extra[cur-head:], Cursors: map[string]int64{P: head + int64(len(extra))}, Reset: []string{}}
		}
		return page{Events: []json.RawMessage{}, Cursors: map[string]int64{P: cur}, Reset: []string{}}
	}
	_, err := Pull(context.Background(), st, client(srv), time.Second)
	must(t, err)
	if st.Cursors()[P] != head {
		t.Fatalf("cursor %v", st.Cursors())
	}
	// The replica is ahead of a restored server, with a topic the server no longer has; P2 has its own data.
	stale := protocol.NewTopic()
	stale.ID, stale.Project, stale.Name = "tp_stale", P, "Gone after the restore"
	other := protocol.NewTopic()
	other.ID, other.Project, other.Name = "tp_other", P2, "Invoices"
	must(t, st.Tx(func(tx *store.Tx) error {
		must(t, tx.PutTopic(stale))
		must(t, tx.PutTopic(other))
		must(t, tx.SetCursor(P, head+40))
		must(t, tx.SetCursor(P2, 7))
		_, err := tx.UpsertSession("s1", "agent", 1, "", t0)
		must(t, err)
		must(t, tx.Share("s1", P, M1, "k", nil))
		return tx.Seen("s1", head+40, 1)
	}))
	f.poll = wrapPollWithout(f.poll, P2)
	n, err := Pull(context.Background(), st, client(srv), time.Second)
	must(t, err)
	if n != 3 {
		t.Fatalf("pulled %d after the reset, want 3 (everything again from 0)", n)
	}
	topics, err := st.Topics(P)
	must(t, err)
	if len(topics) != 1 || topics[0].ID != T1 {
		t.Fatalf("topics %+v: the stale one stays or the real one is missing", topics)
	}
	if _, ok, _ := st.Signal(S1); !ok {
		t.Fatal("the signal did not come back")
	}
	if _, ok, _ := st.Project(P); !ok {
		t.Fatal("the project did not come back")
	}
	if c := st.Cursors(); c[P] != head || c[P2] != 7 {
		t.Fatalf("cursors %v", c)
	}
	sess, _, err := st.Session("s1")
	must(t, err)
	if sess.LastSeq != head {
		t.Fatalf("session bookmark %d, want the head %d", sess.LastSeq, head)
	}
	t2, err := st.Topics(P2)
	must(t, err)
	if len(t2) != 1 || t2[0].ID != "tp_other" {
		t.Fatalf("P2 touched: %+v", t2)
	}
	// After the reset a new event comes as usual, and pulling again changes nothing.
	ev := json.RawMessage(strings.Replace(string(golden(t, "event.signal.accepted.1")), `"seq": 5`, `"seq": 4`, 1))
	ev = json.RawMessage(strings.ReplaceAll(string(ev), S1, S2))
	extra = append(extra, ev)
	n, err = Pull(context.Background(), st, client(srv), time.Second)
	must(t, err)
	if n != 1 || st.Cursors()[P] != head+1 {
		t.Fatalf("after the reset: %d pulled, cursors %v", n, st.Cursors())
	}
	n, err = Pull(context.Background(), st, client(srv), time.Second)
	if err != nil || n != 0 || st.Cursors()[P] != head+1 {
		t.Fatalf("again: %d, %v, %v", n, err, st.Cursors())
	}
}

// wrapPollWithout answers as poll does but never about project skip (as a key for one project would).
func wrapPollWithout(poll func(protocol.SyncPoll) any, skip string) func(protocol.SyncPoll) any {
	return func(p protocol.SyncPoll) any {
		cur := map[string]int64{}
		for k, v := range p.Cursors {
			if k != skip {
				cur[k] = v
			}
		}
		return poll(protocol.SyncPoll{Cursors: cur, Wait: p.Wait})
	}
}

func TestPullStopsAfterMaxPages(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	sig := golden(t, "event.signal.accepted.1")
	f.poll = func(p protocol.SyncPoll) any { // always one more event
		return page{Events: []json.RawMessage{sig}, Cursors: map[string]int64{P: p.Cursors[P] + 1}, Reset: []string{}}
	}
	n, err := Pull(context.Background(), st, client(srv), time.Second)
	must(t, err)
	if n != MaxPages || len(f.polls) != MaxPages || MaxPages != 20 {
		t.Fatalf("pulled %d in %d polls", n, len(f.polls))
	}
}

func TestPullOfflineChangesNothing(t *testing.T) {
	st := openStore(t)
	must(t, st.SetCursor(P, 4))
	f, srv := newFake(t)
	f.fail = func(int) int { return http.StatusBadGateway }
	n, err := Pull(context.Background(), st, client(srv), time.Second)
	if !errors.Is(err, api.ErrOffline) || n != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if st.Cursors()[P] != 4 {
		t.Fatal("cursor moved")
	}
}

func TestPullGivesUpAfterTheTimeout(t *testing.T) {
	st := openStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	start := time.Now()
	_, err := Pull(context.Background(), st, api.New(srv.URL, key, 10*time.Second), 100*time.Millisecond)
	if !errors.Is(err, api.ErrOffline) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestPullNeverPullsFromZeroOnAStoreThatCantAnswer(t *testing.T) {
	st, err := store.Open(t.TempDir())
	must(t, err)
	f, srv := newFake(t)
	f.poll = func(protocol.SyncPoll) any {
		return page{Events: []json.RawMessage{}, Cursors: map[string]int64{}, Reset: []string{}}
	}
	st.Close()
	if _, err := Pull(context.Background(), st, client(srv), time.Second); err == nil {
		t.Fatal("no error")
	}
	if len(f.polls) != 0 {
		t.Fatal("polled with cursors the store could not read")
	}
}

func TestPullSkipsAnUnknownEventAndMovesOn(t *testing.T) {
	st := openStore(t)
	f, srv := newFake(t)
	var logged []string
	defer swapLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })()
	future := json.RawMessage(`{"project":"` + P + `","seq":1,"at":"2026-01-15T09:00:00Z","type":"future.thing","x":1}`)
	f.poll = func(p protocol.SyncPoll) any {
		if p.Cursors[P] == 0 {
			return page{Events: []json.RawMessage{future, future, golden(t, "event.topic.updated.1")}, Cursors: map[string]int64{P: 3}, Reset: []string{}}
		}
		return page{Events: []json.RawMessage{}, Cursors: map[string]int64{P: 3}, Reset: []string{}}
	}
	n, err := Pull(context.Background(), st, client(srv), time.Second)
	must(t, err)
	if n != 3 || st.Cursors()[P] != 3 {
		t.Fatalf("%d pulled, cursors %v", n, st.Cursors())
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "future.thing") {
		t.Fatalf("logged %q, want once", logged)
	}
}
