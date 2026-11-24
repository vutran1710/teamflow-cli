package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vutran1710/teamflow-cli/internal/core"
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

// The tool list is the export's, read from the files (not the generated Go): names and descriptions word for
// word, in order; the server is "teamflow" with the one instruction text.
func TestToolListEqualsTheExport(t *testing.T) {
	var want []struct{ Name, Description string }
	readJSON(t, filepath.Join(exportDir, "agent", "tools.json"), &want)
	m := newMachine(t, true)
	cs := m.connect()
	var got []*mcp.Tool
	for tl, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, tl)
	}
	if len(got) != len(want) || len(want) != 44 {
		t.Fatalf("%d tools, the export has %d", len(got), len(want))
	}
	// The SDK lists tools by name; the export's order is the registration order.
	byName := map[string]*mcp.Tool{}
	for _, tl := range got {
		byName[tl.Name] = tl
	}
	for _, w := range want {
		tl, ok := byName[w.Name]
		if !ok {
			t.Errorf("missing tool %s", w.Name)
			continue
		}
		if tl.Description != w.Description {
			t.Errorf("%s description\n got %q\nwant %q", w.Name, tl.Description, w.Description)
		}
	}
	for i, at := range protocol.AgentTools {
		if at.Name != want[i].Name {
			t.Fatalf("tool %d is %s, the export's is %s", i, at.Name, want[i].Name)
		}
	}
	instructions, err := os.ReadFile(filepath.Join(exportDir, "agent", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	init := cs.InitializeResult()
	if init.ServerInfo.Name != "teamflow" || init.Instructions != string(instructions) {
		t.Fatalf("server %q, instructions %q", init.ServerInfo.Name, init.Instructions)
	}
	for _, name := range []string{"claude", "cursor", "copilot", "antigravity", "codex", "gemini"} {
		for _, tl := range got {
			if strings.Contains(strings.ToLower(tl.Description), name) {
				t.Errorf("%s names an agent (%s)", tl.Name, name)
			}
		}
	}
}

// Every tool has a handler, and nothing else is registered.
func TestEveryToolHasAHandler(t *testing.T) {
	var names []string
	for _, at := range protocol.AgentTools {
		names = append(names, at.Name)
		if _, ok := registry[at.Name]; !ok {
			t.Errorf("no handler for %s", at.Name)
		}
	}
	for name := range registry {
		if !slices.Contains(names, name) {
			t.Errorf("a handler for %s, a tool the export does not have", name)
		}
	}
}

// The 28 tools the Python MCP server had take its input schemas exactly; the new ones take inferred schemas
// with their enums and defaults.
func TestInputSchemas(t *testing.T) {
	var python []struct {
		Name   string         `json:"name"`
		Schema map[string]any `json:"input_schema"`
	}
	readJSON(t, filepath.Join(exportDir, "mcp_tools.json"), &python)
	m := newMachine(t, true)
	got := map[string]map[string]any{}
	for tl, err := range m.connect().Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(tl.InputSchema)
		var s map[string]any
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		got[tl.Name] = s
	}
	if len(python) != 28 {
		t.Fatalf("mcp_tools.json has %d tools", len(python))
	}
	for _, p := range python {
		if p.Name == "artifact_get" { // gains overwrite (save_to never replaces a file without it)
			p.Schema["properties"].(map[string]any)["overwrite"] = map[string]any{"type": "boolean", "default": false,
				"title": "Overwrite", "description": "Replace a file that already exists at save_to"}
		}
		if !reflect.DeepEqual(got[p.Name], p.Schema) {
			a, _ := json.Marshal(got[p.Name])
			b, _ := json.Marshal(p.Schema)
			t.Errorf("%s schema\n got %s\nwant %s", p.Name, a, b)
		}
	}
	verdict := got["verdict"]
	props := verdict["properties"].(map[string]any)
	if e := props["verdict"].(map[string]any)["enum"]; !reflect.DeepEqual(e, []any{"APPROVE", "REJECT"}) {
		t.Errorf("verdict enum %v", e)
	}
	if req := verdict["required"]; !reflect.DeepEqual(req, []any{"signal", "verdict"}) {
		t.Errorf("verdict required %v", req)
	}
	if d := got["deliverables_archive"]["properties"].(map[string]any)["archived"].(map[string]any)["default"]; d != true {
		t.Errorf("deliverables_archive archived default %v", d)
	}
	for _, name := range []string{"drift_resolve", "drift_dismiss", "deliverables_archive", "artifact_archive"} {
		if n := got[name]["properties"].(map[string]any)["note"].(map[string]any)["maxLength"]; n != float64(240) {
			t.Errorf("%s note maxLength %v", name, n)
		}
	}
	for name, s := range got {
		if s["type"] != "object" {
			t.Errorf("%s: not an object schema", name)
		}
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
	wantErr(t, r, "kind")
	m.notSent("signal.emit")
}

// Every tool result carries what is new for the session, once: approval requests (as the Python tools did) and
// the other notices not shown yet.
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

// A malformed call is refused before the tool runs, with what is wrong; an unknown tool is refused.
func TestMalformedArgumentsAndUnknownTools(t *testing.T) {
	m := newMachine(t, true)
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"a missing required argument", "signal_get", map[string]any{}, "signal"},
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
			if calls := m.srv.methods(); len(calls) != 0 {
				t.Fatalf("a malformed call reached the server: %v", calls)
			}
		})
	}
	t.Run("an unknown tool", func(t *testing.T) {
		wantErr(t, m.call("approve_everything", nil), "approve_everything")
	})
}

// The agent's calls run at once (the SDK serves tools/call concurrently, each call with its own store): what is new
// reaches exactly one result, an error result included.
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
	m.connect()
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

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("pipe broken") }

// Run ends cleanly only when the agent closes the pipe (EOF); any other failure is reported.
func TestRunEndsCleanlyOnlyAtEOF(t *testing.T) {
	newMachine(t, true)
	var out strings.Builder
	if err := Run(context.Background(), core.Open, &finder{}, "test", strings.NewReader(""), &out); err != nil {
		t.Fatalf("EOF: %v", err)
	}
	if err := Run(context.Background(), core.Open, &finder{}, "test", failingReader{}, &out); err == nil ||
		!strings.Contains(err.Error(), "pipe broken") {
		t.Fatalf("a broken pipe: %v", err)
	}
}
