package claudecode

// Claude Code's hooks: the Python adapter's golden fixtures (protocol/hooks/) replayed, then the sad paths.
//
// Replayed with two documented differences (both by design, see the design's "Revision: agent-agnostic"):
//   - the full context no longer starts with the Python client's protocol text: the one instruction text is in
//     CLAUDE.md (placed by install), so the expected context is the fixture's
//     from "## Teamflow: shared project" on;
//   - the approval request's closing line is the agent-neutral one (verdicts through `teamflow
//     approve`, on the person's explicit words), replacing the Python line ("/teamf approve … Never run those
//     yourself").
//
// Dropped: the 44 PreToolUse.guard-* fixtures. The guard is gone by design; the Teamflow home is kept from the
// agent by Claude Code's own deny rules (permissions.deny, see install), and verdicts are the person's whichever way
// they are given.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

const (
	P     = "pj_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	Lan   = "mb_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	Huy   = "mb_7ZZZZZZZZZZZZZZZZZZZZZZZZ2"
	Plan  = "tp_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	Round = "tp_7ZZZZZZZZZZZZZZZZZZZZZZZZ2"
	Curr  = "tp_7ZZZZZZZZZZZZZZZZZZZZZZZZ3"
	email = "huy@acme.test"
)

// ------------------------------------------------------------------ the fixtures and their comparison

type fixture struct {
	Event, Case, Layout, Stdin, Stdout string
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

var (
	idRe = regexp.MustCompile(`\b(pj|tp|sg|mb|tk|dr|ac|og|ms|ai)_[0-9A-HJKMNP-TV-Z]{26}\b`)
	tsRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
)

// normalise renumbers ids and timestamps as protocol/hooks/README.md says (stdin first, then stdout).
func normalise(stdin, stdout string) string {
	nums, counts := map[string]string{}, map[string]int{}
	for _, s := range []string{stdin, stdout} {
		for _, id := range idRe.FindAllString(s, -1) {
			if _, ok := nums[id]; !ok {
				p := id[:2]
				counts[p]++
				nums[id] = fmt.Sprintf("%s_%026d", p, counts[p])
			}
		}
	}
	var stamps []time.Time
	seen := map[string]time.Time{}
	for _, s := range []string{stdin, stdout} {
		for _, ts := range tsRe.FindAllString(s, -1) {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				seen[ts] = t
				stamps = append(stamps, t)
			}
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	var distinct []time.Time
	for _, s := range stamps {
		if len(distinct) == 0 || !distinct[len(distinct)-1].Equal(s) {
			distinct = append(distinct, s)
		}
	}
	base := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	out := idRe.ReplaceAllStringFunc(stdout, func(id string) string { return nums[id] })
	return tsRe.ReplaceAllStringFunc(out, func(ts string) string {
		for i, d := range distinct {
			if d.Equal(seen[ts]) {
				return base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			}
		}
		return ts
	})
}

// The Python closing line of an approval request, and what the agent-neutral text says instead.
const pythonAsk = "Ask the person about each, one at a time, in your own words: Approve, Reject or Later. Their choice " +
	"counts only when they make it: /teamf approve <id>, or /teamf reject <id> <why> (or in the Teamflow web app). " +
	"Never run those yourself. Later leaves it waiting; /teamf approvals brings the queue back."

// expected is the fixture's stdout with the documented differences applied.
func expected(t *testing.T, f fixture) any {
	t.Helper()
	if f.Stdout == "" {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(f.Stdout), &v); err != nil {
		t.Fatal(err)
	}
	fix := func(s string) string {
		if i := strings.Index(s, "## Teamflow: shared project"); i > 0 && strings.HasPrefix(s, "# Teamflow protocol") {
			s = s[i:]
		}
		return strings.ReplaceAll(s, pythonAsk, neutralAsk(t))
	}
	if r, ok := v["reason"].(string); ok {
		v["reason"] = fix(r)
	}
	if h, ok := v["hookSpecificOutput"].(map[string]any); ok {
		h["additionalContext"] = fix(h["additionalContext"].(string))
	}
	return v
}

// neutralAsk is the closing line core's approval text ends with today.
func neutralAsk(t *testing.T) string {
	t.Helper()
	if !strings.Contains(core.ApprovalsAsk, "explicit words") {
		t.Fatalf("the approval text's closing line: %q", core.ApprovalsAsk)
	}
	return core.ApprovalsAsk
}

// replay runs the fixture's event on the machine (with its stdin, the ids of this machine's state in it) and
// compares json-normalised.
func (m *machine) replay(t *testing.T, name string) {
	t.Helper()
	f := loadFixture(t, name)
	out := m.hook(f.Event, f.Stdin)
	if f.Stdout == "" {
		if len(out) != 0 {
			t.Fatalf("%s: want nothing, got %s", name, out)
		}
		return
	}
	if len(out) == 0 {
		t.Fatalf("%s: answered nothing; hook.log:\n%s", name, m.hookLog())
	}
	var got any
	if err := json.Unmarshal([]byte(normalise(f.Stdin, string(out))), &got); err != nil {
		t.Fatalf("%s: not JSON: %v\n%s", name, err, out)
	}
	want := expected(t, f)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("%s:\n--- got\n%s\n--- want\n%s", name, g, w)
	}
}

// ------------------------------------------------------------------ a machine

type fakeServer struct {
	mu      sync.Mutex
	srv     *httptest.Server
	answers map[string]string // method -> a whole answer {"result": …} / {"error": …}
	down    bool
	calls   []string
}

func newServer(t *testing.T) *fakeServer {
	f := &fakeServer{answers: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var c struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &c)
		f.calls = append(f.calls, c.Method)
		ans, ok := f.answers[c.Method]
		switch {
		case ok:
		case c.Method == "sync.poll":
			ans = `{"result": {"events": [], "cursors": {}, "reset": []}}`
		default:
			ans = `{"error": {"code": -32601, "message": "no answer for ` + c.Method + `"}}`
		}
		var resp map[string]json.RawMessage
		_ = json.Unmarshal([]byte(ans), &resp)
		resp["jsonrpc"], resp["id"] = json.RawMessage(`"2.0"`), c.ID
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) set(method, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = answer
}

func (f *fakeServer) offline(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

type machine struct {
	t   *testing.T
	env *env
	srv *fakeServer
	seq int64
}

// newMachine is a temp HOME and Claude Code config with a Teamflow home; signed in (on a fake server) when
// signedIn, with Pricing (Plan, Rounding, Currency; plan "Ship pricing v1") in the replica.
func newMachine(t *testing.T, signedIn bool) *machine {
	t.Helper()
	m := &machine{t: t, env: newEnv(t, true), srv: newServer(t)}
	if !signedIn {
		return m
	}
	must(t, config.Save(m.env.tfHome, config.Config{Server: m.srv.srv.URL, AuthKey: "tfa_example", Email: email}))
	m.store(func(tx *store.Tx) error {
		if err := tx.PutProjectKey(store.Binding(m.srv.srv.URL, email), P, "tfk_example", Huy, "og_7ZZZZZZZZZZZZZZZZZZZZZZZZ1", "Pricing", time.Now()); err != nil {
			return err
		}
		p := protocol.NewProject()
		p.ID, p.Org, p.Name, p.Initiator, p.Members, p.Version = P, "og_7ZZZZZZZZZZZZZZZZZZZZZZZZ1", "Pricing", Lan, []string{Lan, Huy}, 1
		p.Titles = map[string]string{}
		if err := tx.PutProject(p); err != nil {
			return err
		}
		for _, tp := range []protocol.Topic{
			{ID: Plan, Project: P, Name: "Plan", Description: "Comments on the plan, the instruction and drifts", System: true},
			{ID: Round, Project: P, Name: "Rounding", Description: "How prices round"},
			{ID: Curr, Project: P, Name: "Currency", Description: "Which currencies"},
		} {
			if err := tx.PutTopic(tp); err != nil {
				return err
			}
		}
		st := protocol.NewProjectState()
		st.Project, st.Version, st.Plan, st.Instruction = P, 1, "Ship pricing v1", "Keep prices simple"
		if err := tx.PutState(st); err != nil {
			return err
		}
		return tx.PutNames(map[string]string{Lan: "lan", Huy: "huy"})
	})
	return m
}

func (m *machine) store(fn func(tx *store.Tx) error) {
	m.t.Helper()
	st, err := store.Open(m.env.tfHome)
	must(m.t, err)
	defer st.Close()
	must(m.t, st.Tx(fn))
}

// share records an agent session shared with Pricing as member.
func (m *machine) share(sid, member string) {
	m.store(func(tx *store.Tx) error {
		if _, err := tx.UpsertSession(sid, Name, 0, "", time.Now()); err != nil {
			return err
		}
		return tx.Share(sid, P, member, "ref", nil)
	})
}

// emit applies a teammate's signal as the sync would (the cursor moves with it).
func (m *machine) emit(id, by, topic, title, summary string, points []string, why string, approver string) {
	m.seq++
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = id, P, topic, protocol.SignalKindDecision, title
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points, body.Why = "key_points.v1", summary, points, why
	sig.Body = body
	state := protocol.SignalStateInForce
	if approver != "" {
		sig.RequestForApproval = &protocol.ApprovalRequest{Approvers: []string{approver}, Required: 1}
		state = protocol.SignalStatePending
	}
	s := protocol.NewStoredSignal()
	s.Signal, s.Author.Member, s.Author.Actor = sig, by, protocol.ActorAgent
	s.Seq, s.Chain, s.ChainVersion, s.State = m.seq, id, 1, state
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal = P, m.seq, "signal.accepted", s
	m.store(func(tx *store.Tx) error {
		if err := syncer.Apply(tx, ev, time.Now()); err != nil {
			return err
		}
		return tx.SetCursor(P, m.seq)
	})
}

// hook runs one hook as `teamflow hook claude-code <event>` would: a fresh client from disk each time.
func (m *machine) hook(event, stdin string) string {
	m.t.Helper()
	return string(adapters.RunHook(New(), event, []byte(stdin), core.Open))
}

func (m *machine) hookLog() string {
	b, _ := os.ReadFile(filepath.Join(m.env.tfHome, "hook.log"))
	return string(b)
}

// ------------------------------------------------------------------ the replay

const (
	S1 = "sg_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	S2 = "sg_7ZZZZZZZZZZZZZZZZZZZZZZZZ2"
	S3 = "sg_7ZZZZZZZZZZZZZZZZZZZZZZZZ3"
)

// The shared scenario, in the order the fixtures were recorded: Huy's first prompt, nothing new, a teammate's
// decision, an approval request at the turn's end (once), another mid-turn at a tool call (once), Lan's /clear,
// Huy's new unshared session, Huy's session end.
func TestReplaySharedFixtures(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	m.share("lan-1", Lan)
	m.replay(t, "UserPromptSubmit.first-shared-prompt")
	m.replay(t, "UserPromptSubmit.nothing-new")
	m.emit(S1, Lan, Round, "Round prices to 0.05", "All prices round to the nearest 0.05", []string{"Applies to every currency"},
		"Cash registers cannot give 0.01 change", "")
	m.replay(t, "UserPromptSubmit.new-signal")
	m.emit(S2, Lan, Round, "Round half up to 0.05", "Ties go up", []string{"0.025 becomes 0.05"}, "", Huy)
	m.replay(t, "Stop.approval-requested")
	m.replay(t, "Stop.already-told")
	m.replay(t, "Stop.stop-hook-active")
	m.emit(S3, Lan, Round, "Round half down to 0.05", "Ties go up", []string{"0.025 becomes 0.05"}, "", Huy)
	m.replay(t, "PreToolUse.approval-requested")
	m.replay(t, "PreToolUse.already-told")
	m.replay(t, "SessionStart.clear-shared")
	m.replay(t, "SessionStart.new-session")
	m.replay(t, "SessionEnd.shared")
}

// Signed out (no config.json), every hook answers nothing; stdin that is not JSON answers nothing and is logged.
func TestReplaySignedOutAndMalformed(t *testing.T) {
	m := newMachine(t, false)
	for _, name := range []string{"SessionStart.signed-out", "UserPromptSubmit.signed-out", "PreToolUse.signed-out",
		"Stop.signed-out", "SessionEnd.signed-out", "UserPromptSubmit.malformed-stdin"} {
		m.replay(t, name)
	}
	if !strings.Contains(m.hookLog(), "UserPromptSubmit") {
		t.Fatalf("malformed stdin not logged: %q", m.hookLog())
	}
	if strings.Contains(m.hookLog(), "not json") {
		t.Fatal("the agent's input was logged")
	}
}

// ------------------------------------------------------------------ sad paths

// Bad input: no session id, a session id that is not a string, an empty object, an unknown event: nothing, never
// a failure; SessionID says why.
func TestHookBadInput(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	for _, in := range []string{`{}`, `{"session_id": 7}`, `{"session_id": ""}`, `[]`, ``} {
		for _, ev := range Events {
			if out := m.hook(ev, in); out != "" {
				t.Fatalf("%s %s answered %q", ev, in, out)
			}
		}
		if _, err := New().SessionID([]byte(in)); err == nil {
			t.Fatalf("SessionID(%q) took it", in)
		}
	}
	if out := m.hook("Notification", `{"session_id": "huy-1"}`); out != "" {
		t.Fatalf("an unknown event answered %q", out)
	}
	if sid, err := New().SessionID([]byte(`{"session_id": "huy-1", "cwd": "/x"}`)); err != nil || sid != "huy-1" {
		t.Fatalf("SessionID %q %v", sid, err)
	}
}

// Offline: the prompt hook answers from the replica and says the server is unreachable; nothing fails.
func TestHookOffline(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "hi"}`) // the full context, once
	m.srv.offline(true)
	m.emit(S1, Lan, Round, "Round prices to 0.05", "s", []string{"p"}, "", "")
	out := m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "go"}`)
	if !strings.Contains(out, S1) || !strings.Contains(out, "unreachable") {
		t.Fatalf("offline prompt: %s", out)
	}
	// An approval request reached the replica while offline: the turn's end still raises it from the replica.
	m.emit(S2, Lan, Round, "Needs Huy", "s", []string{"p"}, "", Huy)
	if out := m.hook("Stop", `{"session_id": "huy-1", "stop_hook_active": false}`); !strings.Contains(out, S2) {
		t.Fatalf("offline Stop: %s", out)
	}
}

// A newer server (UPGRADE_REQUIRED) or a revoked membership (NOT_A_MEMBER) during the hook's sync: the hook still
// answers from the replica, never fails the agent; a revoked key is dropped.
func TestHookOldClientAndRevoked(t *testing.T) {
	for _, reason := range []string{"UPGRADE_REQUIRED", "NOT_A_MEMBER"} {
		t.Run(reason, func(t *testing.T) {
			m := newMachine(t, true)
			m.share("huy-1", Huy)
			m.srv.set("sync.poll", `{"error": {"code": -32000, "message": "refused", "data": {"reason": "`+reason+`"}}}`)
			out := m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "hi"}`)
			if !strings.Contains(out, "## Teamflow: shared project") {
				t.Fatalf("no context from the replica: %q\n%s", out, m.hookLog())
			}
		})
	}
}

// The store held by another process (the background sync) past the busy timeout: the hook answers nothing within
// budget, never fails, and the notice it could not claim is still there for the next hook.
func TestHookStoreBusy(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "hi"}`)
	m.emit(S2, Lan, Round, "Needs Huy", "s", []string{"p"}, "", Huy)
	db, err := sql.Open("sqlite", filepath.Join(m.env.tfHome, store.FileName)+"?_pragma=busy_timeout(0)")
	must(t, err)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	must(t, err)
	_, err = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE")
	must(t, err)
	start := time.Now()
	out := m.hook("Stop", `{"session_id": "huy-1", "stop_hook_active": false}`)
	took := time.Since(start)
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	conn.Close()
	if out != "" {
		t.Fatalf("answered while busy: %q", out)
	}
	if took > 3*time.Second {
		t.Fatalf("a busy store held the hook %v", took)
	}
	if out := m.hook("Stop", `{"session_id": "huy-1", "stop_hook_active": false}`); !strings.Contains(out, S2) {
		t.Fatalf("the request was lost to a busy store: %q", out)
	}
}

// Restart: after a reboot the agent resumes its session (same id, a new agent process): the record is found by
// the id, moves to the new process (so the MCP server finds it by ancestry), the resumed start adds nothing (the
// conversation has the context), and the next prompt brings what is new. The background sync is started again.
func TestHookResumeAfterRestart(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "hi"}`)
	m.store(func(tx *store.Tx) error {
		_, err := tx.UpsertSession("huy-1", Name, 999999, "", time.Now())
		return err
	})
	started := 0
	old := adapters.StartSync
	adapters.StartSync = func() error { started++; return fmt.Errorf("no background sync in this build") }
	defer func() { adapters.StartSync = old }()
	if out := m.hook("SessionStart", `{"session_id": "huy-1", "source": "resume", "cwd": ""}`); out != "" {
		t.Fatalf("resume added %q", out)
	}
	if started != 1 {
		t.Fatalf("background sync started %d times", started)
	}
	m.store(func(tx *store.Tx) error {
		s, ok, err := tx.Session("huy-1")
		if err != nil || !ok || s.Project != P || s.AgentPID == 999999 {
			return fmt.Errorf("resumed session %+v %v %v", s, ok, err)
		}
		return nil
	})
	m.emit(S1, Lan, Round, "After the reboot", "s", []string{"p"}, "", "")
	if out := m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "go"}`); !strings.Contains(out, "After the reboot") {
		t.Fatalf("after resume: %q", out)
	}
	if started != 2 { // a prompt starts it too when it is not running; its failure never fails the hook
		t.Fatalf("background sync started %d times", started)
	}
}

// Compaction re-sends the project and the session's citations; the person's own /teamf approve prompt adds
// nothing (the command records the verdict after the hook).
func TestHookCompactAndTypedVerdict(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "hi"}`)
	out := contextOf(t, m.hook("SessionStart", `{"session_id": "huy-1", "source": "compact"}`))
	if !strings.Contains(out, "re-sent after compaction") || !strings.Contains(out, "Signals this session cites:\n(none)") {
		t.Fatalf("compact: %q", out)
	}
	m.emit(S2, Lan, Round, "Needs Huy", "s", []string{"p"}, "", Huy)
	if out := m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "/teamf approve `+S2+`"}`); out != "" {
		t.Fatalf("typed verdict: %q", out)
	}
}

// A .teamflow file in the session's folder shares it at start; when sharing fails (here: the server refuses), the
// session hears why and nothing fails.
func TestHookAutoShareFailure(t *testing.T) {
	m := newMachine(t, true)
	work := filepath.Join(t.TempDir(), "repo", "sub")
	must(t, os.MkdirAll(work, 0o755))
	must(t, os.WriteFile(filepath.Join(filepath.Dir(work), ".teamflow"), []byte("Pricing\nignored\n"), 0o644))
	m.srv.set("project.get", `{"error": {"code": -32000, "message": "refused", "data": {"reason": "NOT_A_MEMBER"}}}`)
	in, _ := json.Marshal(map[string]string{"session_id": "new-1", "source": "startup", "cwd": work})
	out := m.hook("SessionStart", string(in))
	if !strings.Contains(out, "could not share this session with Pricing") {
		t.Fatalf("auto share failure: %q\n%s", out, m.hookLog())
	}
}

// The prompt hook's answer is Claude Code's shape: hookSpecificOutput with the event's name, nothing else.
func TestHookOutputShape(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	out := m.hook("UserPromptSubmit", `{"session_id": "huy-1", "prompt": "hi"}`)
	var v map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v) != 1 || v["hookSpecificOutput"]["hookEventName"] != "UserPromptSubmit" {
		t.Fatalf("shape: %s %v", out, err)
	}
	if !bytes.Contains([]byte(out), []byte("Ship pricing v1")) {
		t.Fatal("no plan")
	}
}

// contextOf is the additionalContext of a hook's answer.
func contextOf(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		H struct {
			C string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not a hook answer: %q", out)
	}
	return v.H.C
}
