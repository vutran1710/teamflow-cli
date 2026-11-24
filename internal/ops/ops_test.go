package ops

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

// Human acts (a verdict, a drift resolved or dismissed) each go alone: one plain request, never an array, and a
// verdict is the person's own (actor MEMBER).
func TestHumanActsAreSentAlone(t *testing.T) {
	m := newMachine(t, true)
	m.withStore(func(st *store.Store) error { return seedApproval(st) })
	m.srv.on("member.me", memberMe(t))
	m.srv.on("signal.emit", resultHas(t, "signal.emit", `"accepted": true`))
	r := m.call("verdict", map[string]any{"signal": S1, "verdict": "APPROVE", "note": "fine"})
	if o := r.obj(t); o["accepted"] != true {
		t.Fatalf("verdict: %v", o)
	}
	emits := m.srv.sent("signal.emit")
	if len(emits) != 1 || emits[0].Batch {
		t.Fatalf("a verdict goes alone: %+v", emits)
	}
	p := m.params("signal.emit")
	sig := p["signal"].(map[string]any)
	if p["actor"] != "MEMBER" || p["client_session"] != "s1" || sig["kind"] != "APPROVE" || sig["target"] != S1 ||
		emits[0].Auth != "Bearer "+projKey {
		t.Fatalf("verdict params %v (auth %s)", p, emits[0].Auth)
	}

	for _, tc := range []struct{ tool, method string }{{"drift_resolve", "drift.resolve"}, {"drift_dismiss", "drift.dismiss"}} {
		m.srv.on(tc.method, ok(t, tc.method))
		r := m.call(tc.tool, map[string]any{"drift": D1, "note": "settled in the meeting", "project": P})
		if o := r.obj(t); o["id"] != D1 {
			t.Fatalf("%s: %v", tc.tool, o)
		}
		got := m.srv.sent(tc.method)
		if len(got) != 1 || got[0].Batch {
			t.Fatalf("%s goes alone: %+v", tc.method, got)
		}
		if p := m.params(tc.method); p["drift"] != D1 || p["note"] != "settled in the meeting" {
			t.Fatalf("%s params %v", tc.method, p)
		}
	}
	if p := m.params("drift.resolve"); !reflect.DeepEqual(p["resolved_by"], []any{}) {
		t.Fatalf("resolved_by %v", p["resolved_by"])
	}
}

// seedApproval puts S1 "Needs Huy", PENDING, asking M1 (this machine's member) to approve.
func seedApproval(st *store.Store) error {
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = S1, P, T1, protocol.SignalKindDecision, "Needs Huy"
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points = "key_points.v1", "Round half up", []string{"half up"}
	sig.Body = body
	sig.RequestForApproval = &protocol.ApprovalRequest{Approvers: []string{M1}, Required: 1}
	s := protocol.NewStoredSignal()
	s.Signal, s.Author.Member, s.Author.Actor = sig, M2, protocol.ActorAgent
	s.Seq, s.Chain, s.ChainVersion, s.State = 6, S1, 1, protocol.SignalStatePending
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal = P, 6, "signal.accepted", s
	return st.Tx(func(tx *store.Tx) error { return applyEvent(tx, ev) })
}

// The agent cannot slip a verdict in among routine signals: signal_emit takes only the agent's kinds.
func TestSignalEmitTakesNoVerdict(t *testing.T) {
	m := newMachine(t, true)
	r := m.call("signal_emit", map[string]any{"signals": []any{map[string]any{"kind": "APPROVE", "topic": "Rounding",
		"title": "Approved", "summary": "s", "points": []any{"p"}}}})
	a := r.objs(t)
	if len(a) != 1 || a[0]["accepted"] != false || a[0]["reason"] != "INVALID" || !strings.Contains(a[0]["message"].(string), "verdict") {
		t.Fatalf("a verdict among signals: %v", a)
	}
	m.notSent("signal.emit")
}

// Every answer carries what is new for the session, once: approval requests (as the Python tools did) and the
// other notices not shown yet.
func TestRefreshingCarriesNotices(t *testing.T) {
	m := newMachine(t, true)
	m.withStore(func(st *store.Store) error {
		if err := seedApproval(st); err != nil {
			return err
		}
		return st.Notice("s1", protocol.NoticeTypeDriftFound, D1, "Drift found (PLAN_CONFLICT): x [dr_…]", now())
	})
	m.srv.on("account.me", lanMe(t))
	m.srv.on("member.me", memberMe(t))
	r := m.call("whoami", nil)
	o := r.obj(t)
	news, _ := o["approval_requests"].(string)
	if !strings.Contains(news, "a new approval request arrived") || !strings.Contains(news, S1) ||
		!strings.Contains(news, "«Needs Huy»") || !strings.Contains(news, "verdict tool") {
		t.Fatalf("approval_requests %q", news)
	}
	if n, _ := o["notices"].([]any); len(n) != 1 || !strings.Contains(n[0].(string), "Drift found") {
		t.Fatalf("notices %v", o["notices"])
	}
	if o["account"] != "lan" || o["member"] != M1 {
		t.Fatalf("whoami %v", o)
	}
	again := m.call("whoami", nil).obj(t)
	if _, ok := again["approval_requests"]; ok {
		t.Fatal("an approval request was raised twice")
	}
	if _, ok := again["notices"]; ok {
		t.Fatal("a notice was shown twice")
	}
	// The queue still holds it (Later): approvals lists it.
	wantText(t, m.call("approvals", nil), S1, "1 approval request(s) wait for you")

	// A list result gains the news under "result".
	m2 := newMachine(t, true)
	m2.withStore(func(st *store.Store) error {
		return st.Notice("s1", protocol.NoticeTypeTopicDeleted, T2, "Topic \"Currency\" was deleted", now())
	})
	m2.srv.on("account.projects", ok(t, "account.projects"))
	o = m2.call("project_list", nil).obj(t)
	if _, ok := o["result"].([]any); !ok || o["notices"] == nil {
		t.Fatalf("a list with news: %v", o)
	}
	// No session found: nothing to add, and the tool still answers.
	m3 := newMachine(t, true)
	m3.find.id = ""
	m3.srv.on("account.projects", ok(t, "account.projects"))
	if blocks := m3.call("project_list", nil).objs(t); len(blocks) == 0 || blocks[0]["id"] != P {
		t.Fatalf("project_list without a session: %v", blocks)
	}
}

// Malformed input is refused before anything is sent, with what is wrong; an unknown action is refused.
func TestMalformedArgumentsAndUnknownTools(t *testing.T) {
	m := newMachine(t, true)
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"a wrong type", "signal_get", map[string]any{"signal": 7}, "signal"},
		{"an unknown enum value", "drift_list", map[string]any{"status": "MAYBE"}, "status"},
		{"an extra field in a signal", "signal_emit", map[string]any{"signals": []any{map[string]any{"kind": "NOTE",
			"topic": "Rounding", "title": "t", "summary": "s", "points": []any{"p"}, "secret": "x"}}}, "secret"},
		{"a verdict that is neither", "verdict", map[string]any{"signal": S1, "verdict": "MAYBE"}, "verdict"},
		{"a note too long", "project_request_update", map[string]any{"note": strings.Repeat("x", 241)}, "note"},
		{"an unknown argument to a new tool", "profile_update", map[string]any{"avatar": "x.png"}, "avatar"},
		{"a drift note too long", "drift_resolve", map[string]any{"drift": D1, "note": strings.Repeat("x", 241)}, "note"},
		{"an archive note too long", "artifact_archive", map[string]any{"artifact": S1, "note": strings.Repeat("x", 241)}, "note"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.srv.reset()
			wantErr(t, m.call(tc.tool, tc.args), tc.want)
			for _, c := range m.srv.methods() { // the news may sync; nothing else is sent
				if c != "sync.poll" && c != "member.list" {
					t.Fatalf("a malformed call reached the server: %v", m.srv.methods())
				}
			}
		})
	}
	t.Run("an unknown tool", func(t *testing.T) {
		wantErr(t, m.call("approve_everything", nil), "approve_everything")
	})
}

// Commands run at once (an agent runs several in parallel, each process with its own store): what is new reaches
// exactly one result, an error result included.
func TestNewsReachesOneResultAcrossParallelCalls(t *testing.T) {
	m := newMachine(t, true)
	m.withStore(func(st *store.Store) error {
		if err := seedApproval(st); err != nil {
			return err
		}
		if err := st.Notice("s1", protocol.NoticeTypeDriftFound, D1, "notice-one", now()); err != nil {
			return err
		}
		return st.Notice("s1", protocol.NoticeTypeTopicDeleted, T2, "notice-two", now())
	})
	m.srv.on("account.me", refused(t, "account.me", "NOT_ALLOWED")) // whoami fails: an error result
	m.srv.on("account.projects", ok(t, "account.projects"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []string
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "project_list"
			if i%2 == 0 {
				name = "whoami"
			}
			r := m.call(name, nil)
			mu.Lock()
			all = append(all, r.text())
			mu.Unlock()
		}()
	}
	wg.Wait()
	joined := strings.Join(all, "\n")
	for _, want := range []string{"a new approval request arrived", `"notice-one"`, `"notice-two"`} {
		if n := strings.Count(joined, want); n != 1 {
			t.Errorf("%s reached %d results, want 1:\n%s", want, n, joined)
		}
	}
}

// A failing tool still tells the session what is new.
func TestErrorResultsCarryNews(t *testing.T) {
	m := newMachine(t, true)
	m.withStore(func(st *store.Store) error { return seedApproval(st) })
	m.srv.on("account.me", refused(t, "account.me", "NOT_ALLOWED"))
	r := m.call("whoami", nil)
	wantErr(t, r, "NOT_ALLOWED")
	if !strings.Contains(r.text(), `"approval_requests"`) || !strings.Contains(r.text(), S1) {
		t.Fatalf("an error result without the news: %s", r)
	}
	if again := m.call("whoami", nil); strings.Contains(again.text(), "approval_requests") {
		t.Fatalf("raised twice: %s", again)
	}
}
