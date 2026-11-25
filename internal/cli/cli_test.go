// Tests of the teamflow commands, run in-process against an httptest server that answers with the exported golden
// answers (protocol/golden). Each test asserts what was sent (method, params, key), what was printed and the exit
// code, and the sad paths: not signed in, a non-member, an unknown project, offline, and UPGRADE_REQUIRED.
//
// Python twins (cli/tf_cli/main.py and the e2e checks that drive it):
//
//	e2e/smoke.py "teamflow login refuses a wrong password"           → TestLogin
//	e2e/smoke.py "teamflow login signs in, prints no key, keeps no password" → TestLogin, TestPasswordNeverInArgv
//	e2e/smoke.py "teamflow projects lists them by org"               → TestProjects
//	e2e/smoke.py artifacts (write, too large, link, pull, archive)   → TestArtifact*
//	e2e/smoke.py "the CLI has no drift command"                      → TestUnknownCommand
//	e2e/smoke.py org members, add-member, remove-member              → TestOrg
//	e2e/smoke.py "teamflow logout revokes the auth key"              → TestLogout
//	tests/client/test_teamf_project.py (relay, project menu, join)   → TestTeam*
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

const (
	P       = "pj_00000000000000000000000001"
	M1      = "mb_00000000000000000000000001"
	M2      = "mb_00000000000000000000000002"
	T1      = "tp_00000000000000000000000001"
	T2      = "tp_00000000000000000000000002"
	S1      = "sg_00000000000000000000000001"
	S9      = "sg_00000000000000000000000009"
	auth    = "tfa_example"
	projKey = "tfk_example"

	updateText = "update teamflow (`teamflow self-update`, or download the new release)"
)

// ------------------------------------------------------------------ the fake server

// goldenDir holds the exported golden answers (absolute: some tests change the working folder).
var goldenDir, _ = filepath.Abs(filepath.Join("..", "..", "protocol", "golden"))

type rec struct {
	Method string
	Params json.RawMessage
	Auth   string // the Authorization header
	Batch  bool   // the body was a JSON array
}

type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []rec
	answers map[string][]string // method -> answers in turn (the last one repeats)
	down    bool                // unreachable: every request answers 502
	srv     *httptest.Server
}

// offline makes the server unreachable (a gateway's 502), or reachable again.
func (f *fakeServer) offline(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func newServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, answers: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) URL() string { return f.srv.URL }

// on sets a method's answers, in turn: a golden selector (see goldenPath), or a raw {"result": …} / {"error": …}.
func (f *fakeServer) on(method string, answers ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = answers
}

func (f *fakeServer) sent(method string) []rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rec
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

var defaults = map[string]string{
	"sync.poll":   `{"result": {"events": [], "cursors": {}, "reset": []}}`,
	"member.list": "member.list",
	"auth.logout": "auth.logout",
}

func (f *fakeServer) answer(method string) json.RawMessage {
	f.mu.Lock()
	queue := f.answers[method]
	var a string
	switch {
	case len(queue) > 1:
		a, f.answers[method] = queue[0], queue[1:]
	case len(queue) == 1:
		a = queue[0]
	default:
		a = defaults[method]
	}
	f.mu.Unlock()
	if a == "" {
		f.t.Errorf("unexpected call %s", method)
		return json.RawMessage(`{"error": {"code": -32601, "message": "no answer in the test"}}`)
	}
	if strings.HasPrefix(a, "{") {
		return json.RawMessage(a)
	}
	var g struct {
		Response json.RawMessage `json:"response"`
	}
	b, err := os.ReadFile(goldenPath(f.t, a))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(b, &g); err != nil {
		f.t.Fatal(err)
	}
	return g.Response
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	body, _ := io.ReadAll(r.Body)
	type call struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	var calls []call
	batch := bytes.HasPrefix(bytes.TrimSpace(body), []byte("["))
	if batch {
		_ = json.Unmarshal(body, &calls)
	} else {
		var c call
		_ = json.Unmarshal(body, &c)
		calls = []call{c}
	}
	var out []json.RawMessage
	for _, c := range calls {
		f.mu.Lock()
		f.calls = append(f.calls, rec{Method: c.Method, Params: c.Params, Auth: r.Header.Get("Authorization"), Batch: batch})
		f.mu.Unlock()
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(f.answer(c.Method), &resp); err != nil {
			f.t.Fatal(err)
		}
		resp["jsonrpc"], resp["id"] = json.RawMessage(`"2.0"`), c.ID
		b, _ := json.Marshal(resp)
		out = append(out, b)
	}
	w.Header().Set("Content-Type", "application/json")
	if batch {
		b, _ := json.Marshal(out)
		_, _ = w.Write(b)
		return
	}
	_, _ = w.Write(out[0])
}

// ------------------------------------------------------------------ a machine: a home, a config, the commands

type machine struct {
	t   *testing.T
	dir string
	srv *fakeServer
}

// newMachine is a fresh Teamflow home, signed in (an auth key in config.json) on the fake server when signedIn.
func newMachine(t *testing.T, signedIn bool) *machine {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMFLOW_HOME", dir)
	// Never the person's own agent configs: every agent's config lives under a temp HOME.
	t.Setenv("HOME", filepath.Join(filepath.Dir(dir), "user"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(filepath.Dir(dir), "user", ".claude"))
	t.Setenv("COPILOT_HOME", "")
	m := &machine{t: t, dir: dir, srv: newServer(t)}
	if signedIn {
		if err := config.Save(dir, config.Config{Server: m.srv.URL(), AuthKey: auth, Email: "lan@acme.test"}); err != nil {
			t.Fatal(err)
		}
	}
	terminal(t, false, "")
	return m
}

// terminal makes stdin a terminal (or not) for one test; password is what the no-echo prompt reads.
func terminal(t *testing.T, yes bool, password string) {
	t.Helper()
	oldT, oldP := isTerminal, readPassword
	isTerminal = func() bool { return yes }
	readPassword = func() (string, error) { return password, nil }
	t.Cleanup(func() { isTerminal, readPassword = oldT, oldP })
}

type result struct {
	code     int
	out, err string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout: %q\nstderr: %q", r.code, r.out, r.err)
}

func (m *machine) run(stdin string, args ...string) result {
	m.t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, strings.NewReader(stdin), &out, &errb)
	return result{code, out.String(), errb.String()}
}

func (m *machine) store() *store.Store {
	m.t.Helper()
	st, err := store.Open(m.dir)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { st.Close() })
	return st
}

// binding is the sign-in newMachine makes: the fake server, lan@acme.test.
func (m *machine) binding() store.KeyBinding { return store.Binding(m.srv.URL(), "lan@acme.test") }

// key gives the machine its project key for P (as key.fetch would have).
func (m *machine) key() {
	m.t.Helper()
	st := m.store()
	if err := st.PutProjectKey(m.binding(), P, projKey, M1, "og_00000000000000000000000001", "huy", time.Now()); err != nil {
		m.t.Fatal(err)
	}
}

// holds says whether s holds every ~-separated term of text.
func holds(s, text string) bool {
	for _, term := range strings.Split(text, "~") {
		if !strings.Contains(s, term) {
			return false
		}
	}
	return true
}

// lanMe is the golden account.me of Lan, in Acme; huyMember the golden member.me naming M1 "huy".
const (
	lanMe     = `account.me~"email":"lan@acme.test"~"orgs":[{`
	huyMember = `member.me~"name":"huy"`
)

// pricingGet is the golden project.get of Pricing with its two members, Lan (the initiator) and Huy.
const pricingGet = `project.get~"names":{"mb_00000000000000000000000001":"lan","mb_00000000000000000000000002":"huy"}`

// goldenPath is the golden file a selector names, picked by what it holds, never by its number (a new server test
// that records earlier renumbers them): "method" is its first answer that is a result (an event: its first golden),
// "method!REASON" its first refusal for REASON, "method~TEXT[~TEXT…]" its first result (or event) holding every TEXT, written without whitespace (`"archived":false`).
func goldenPath(t *testing.T, sel string) string {
	t.Helper()
	method, reason, text := sel, "", ""
	if i := strings.IndexAny(sel, "!~"); i >= 0 {
		method = sel[:i]
		if sel[i] == '!' {
			reason = sel[i+1:]
		} else {
			text = sel[i+1:]
		}
	}
	if last := method[strings.LastIndex(method, ".")+1:]; last != "" && strings.Trim(last, "0123456789") == "" {
		t.Fatalf("golden %q is named by its number: pick it by what it holds", sel)
	}
	files, _ := filepath.Glob(filepath.Join(goldenDir, method+".*.json"))
	num := func(f string) int {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), method+"."), ".json"))
		return n
	}
	sort.Slice(files, func(i, j int) bool { return num(files[i]) < num(files[j]) })
	for _, f := range files {
		if num(f) == 0 {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var g struct {
			Event    json.RawMessage `json:"event"`
			Response *struct {
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Data struct {
						Reason string `json:"reason"`
					} `json:"data"`
				} `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		compact := func(raw json.RawMessage) string { // matched without whitespace: "a":1
			var b bytes.Buffer
			_ = json.Compact(&b, raw)
			return b.String()
		}
		switch {
		case g.Event != nil:
			if holds(compact(g.Event), text) {
				return f
			}
		case g.Response == nil:
		case reason != "":
			if g.Response.Error != nil && g.Response.Error.Data.Reason == reason {
				return f
			}
		case g.Response.Error == nil && holds(compact(g.Response.Result), text):
			return f
		}
	}
	t.Fatalf("no golden fits %q: re-export the protocol, or the server tests changed", sel)
	return ""
}

func goldenEvent(t *testing.T, name string) protocol.SyncEvent {
	t.Helper()
	b, err := os.ReadFile(goldenPath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	ev, err := protocol.DecodeSyncEvent(g.Event)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// pollEvents are the events of a golden sync.poll answer.
func pollEvents(t *testing.T, name string) []protocol.SyncEvent {
	t.Helper()
	b, err := os.ReadFile(goldenPath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Response struct {
			Result protocol.SyncBatch `json:"result"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g.Response.Result.Events
}

// seed applies events to the replica, as a pull would have.
func (m *machine) seed(evs ...protocol.SyncEvent) {
	m.t.Helper()
	st := m.store()
	err := st.Tx(func(tx *store.Tx) error {
		for _, ev := range evs {
			if err := syncer.Apply(tx, ev, time.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// replica is project P with its topics (Rounding, Currency, Plan) as the golden project.get has them, and three
// artifacts (sg_…1 and its second version sg_…2, and sg_…3) from the golden sync.poll that holds them.
func (m *machine) replica() {
	m.t.Helper()
	m.project()
	var evs []protocol.SyncEvent
	for _, ev := range pollEvents(m.t, `sync.poll~"ARTIFACT"`) {
		switch ev.(type) {
		case protocol.SignalAccepted, protocol.SignalStateChanged, protocol.ArtifactArchivedEvent:
			evs = append(evs, ev)
		}
	}
	m.seed(evs...)
}

// project puts project P and its topics in the replica, as the golden project.get has them.
func (m *machine) project() {
	m.t.Helper()
	b, err := os.ReadFile(goldenPath(m.t, pricingGet))
	if err != nil {
		m.t.Fatal(err)
	}
	var g struct {
		Response struct {
			Result protocol.ProjectView `json:"result"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		m.t.Fatal(err)
	}
	st := m.store()
	err = st.Tx(func(tx *store.Tx) error {
		if err := tx.PutProject(g.Response.Result.Project); err != nil {
			return err
		}
		for _, t := range g.Response.Result.Topics {
			if err := tx.PutTopic(t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// names records member names, as a share would have.
func (m *machine) names(names map[string]string) {
	m.t.Helper()
	if err := m.store().PutNames(names); err != nil {
		m.t.Fatal(err)
	}
}

// inSession records an agent session whose agent is this process's parent: a command run here is "inside" it,
// found by process ancestry as a command run from the agent's shell would be.
func (m *machine) inSession(id, project string, topics []string) {
	m.t.Helper()
	st := m.store()
	if _, err := st.UpsertSession(id, "claude-code", os.Getppid(), m.dir, time.Now()); err != nil {
		m.t.Fatal(err)
	}
	if project != "" {
		if err := st.Share(id, project, M1, "ref", topics); err != nil {
			m.t.Fatal(err)
		}
	}
}

// staleSession records a session against this process's parent as an agent that last ran long before that
// process started: the pid was reused, so it is not this process's agent.
func (m *machine) staleSession(id, project string) {
	m.t.Helper()
	st := m.store()
	if _, err := st.UpsertSession(id, "claude-code", os.Getppid(), m.dir, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		m.t.Fatal(err)
	}
	if err := st.Share(id, project, M1, "ref", nil); err != nil {
		m.t.Fatal(err)
	}
}

func params(t *testing.T, r rec) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(r.Params, &p); err != nil {
		t.Fatalf("params of %s: %v", r.Method, err)
	}
	return p
}

func onlyCall(t *testing.T, s *fakeServer, method string) rec {
	t.Helper()
	got := s.sent(method)
	if len(got) != 1 {
		t.Fatalf("%s sent %d times; calls: %v", method, len(got), s.methods())
	}
	return got[0]
}

func noCall(t *testing.T, s *fakeServer, method string) {
	t.Helper()
	if got := s.sent(method); len(got) != 0 {
		t.Fatalf("%s must not be sent; calls: %v", method, s.methods())
	}
}

func wantCode(t *testing.T, r result, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("want exit %d, got %s", code, r)
	}
}

func contains(t *testing.T, where, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%s %q does not contain %q", where, got, w)
		}
	}
}

func lacks(t *testing.T, where, got string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(got, b) {
			t.Fatalf("%s %q contains %q", where, got, b)
		}
	}
}

// offline points the machine's config at a closed port.
func (m *machine) offline() {
	m.t.Helper()
	m.srv.srv.Close()
}

// ------------------------------------------------------------------ the command line itself

func TestUnknownCommand(t *testing.T) {
	m := newMachine(t, true)
	for _, args := range [][]string{{"tui"}, {"org", "stores", "Acme"},
		{"org", "add-store", "Acme", "s", "ssh://x"}, {"artifact", "delete", "x"}} {
		r := m.run("", args...)
		wantCode(t, r, 2)
		contains(t, "stderr", r.err, "No such command")
	}
	r := m.run("", "drift", "purge") // drift is a command now (resolve, dismiss): no web-only actions
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "No such command 'purge'")
}

func TestUsageHidesInternalCommands(t *testing.T) {
	m := newMachine(t, true)
	r := m.run("", "--help")
	wantCode(t, r, 0)
	contains(t, "help", r.out, "login", "logout", "projects", "status", "stop", "approve", "reject", "team", "topic",
		"artifact", "deliverables", "org", "log", "install", "uninstall")
	for _, hidden := range []string{"hook", "mcp", "debug", "tui"} {
		for _, line := range strings.Split(r.out, "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == hidden {
				t.Fatalf("%s is in the usage text:\n%s", hidden, r.out)
			}
		}
	}
	r = m.run("")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "usage: teamflow")
}

func TestUnknownOptionIsAUsageError(t *testing.T) {
	m := newMachine(t, true)
	r := m.run("", "projects", "--nope")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "No such option: --nope")
	r = m.run("", "artifact", "list", "--project")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "--project needs a value")
}

func TestHelpPerCommand(t *testing.T) {
	m := newMachine(t, true)
	for _, args := range [][]string{{"login", "--help"}, {"artifact", "write", "--help"}, {"org", "-h"}, {"team", "--help"}} {
		r := m.run("", args...)
		wantCode(t, r, 0)
		contains(t, "help", r.out, "usage: teamflow "+args[0])
	}
}

// ------------------------------------------------------------------ passwords

func TestPasswordNeverInArgv(t *testing.T) {
	m := newMachine(t, false)
	m.srv.on("auth.login", "auth.login")

	// No command takes a password as an argument.
	for _, args := range [][]string{
		{"login", "--email", "lan@acme.test", "--password", "pw"},
		{"login", "--email", "lan@acme.test", "--password=pw"},
		{"login", "--email", "lan@acme.test", "-p", "pw"},
		{"install", "-a", "claude-code", "-e", "lan@acme.test", "--password", "pw"},
	} {
		r := m.run("", args...)
		wantCode(t, r, 2)
		contains(t, "stderr", r.err, "No such option")
		lacks(t, "output", r.out+r.err, "pw\n")
	}
	for _, c := range commands {
		for _, f := range c.flagNames() {
			if strings.Contains(f, "pass") && f != "password-stdin" {
				t.Fatalf("%s has a flag --%s", c.name, f)
			}
		}
	}
	noCall(t, m.srv, "auth.login")

	// Not a terminal and no --password-stdin: refused, nothing sent (never read from a pipe silently).
	r := m.run("correct horse battery\n", "login", "-s", m.srv.URL(), "-e", "lan@acme.test")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "--password-stdin")
	noCall(t, m.srv, "auth.login")

	// A terminal: the no-echo prompt.
	terminal(t, true, "typed secret")
	r = m.run("", "login", "-s", m.srv.URL(), "-e", "lan@acme.test")
	wantCode(t, r, 0)
	contains(t, "prompt", r.err, "Password: ")
	if p := params(t, onlyCall(t, m.srv, "auth.login")); p["password"] != "typed secret" {
		t.Fatalf("sent %v", p)
	}
	lacks(t, "output", r.out+r.err, "typed secret")
}

// ------------------------------------------------------------------ login, logout, projects, status

func TestLogin(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		code    int
		out     string
		errText string
	}{
		{"signs in", "auth.login", 0, "signed in as lan <lan@acme.test> until 2026-01-15", ""},
		{"wrong password", "auth.login!NOT_ALLOWED", 1, "", "not signed in: wrong email or password"},
		{"empty password", "auth.login!SCHEMA_VIOLATION", 1, "", "not signed in: password: String should have at least 1 character"},
		{"upgrade required", "auth.login!UPGRADE_REQUIRED", 1, "", updateText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, false)
			m.srv.on("auth.login", tc.answer)
			r := m.run("correct horse battery\n", "login", "--server", m.srv.URL(), "--email", "lan@acme.test",
				"--password-stdin")
			wantCode(t, r, tc.code)
			contains(t, "stdout", r.out, tc.out)
			contains(t, "stderr", r.err, tc.errText)
			c := onlyCall(t, m.srv, "auth.login")
			if c.Auth != "" {
				t.Fatalf("login sent a key: %q", c.Auth)
			}
			if p := params(t, c); p["email"] != "lan@acme.test" || p["password"] != "correct horse battery" {
				t.Fatalf("params %v", p)
			}
			lacks(t, "output", r.out+r.err, "tfa_", "correct horse")
			cfg, _ := config.Load(m.dir)
			if tc.code != 0 {
				if cfg != nil && cfg.AuthKey != "" {
					t.Fatalf("refused, yet a key was saved: %v", cfg)
				}
				return
			}
			b, _ := os.ReadFile(filepath.Join(m.dir, config.FileName))
			fi, _ := os.Stat(filepath.Join(m.dir, config.FileName))
			if cfg == nil || cfg.AuthKey != "tfa_example" || cfg.Server != m.srv.URL() || cfg.Email != "lan@acme.test" ||
				strings.Contains(string(b), "correct horse") || fi.Mode().Perm() != 0o600 {
				t.Fatalf("config %v (mode %o): %s", cfg, fi.Mode().Perm(), b)
			}
		})
	}
}

func TestLoginUsesTheConfiguredServerElseTheDefault(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("auth.login", "auth.login")
	r := m.run("pw\n", "login", "-e", "lan@acme.test", "--password-stdin")
	wantCode(t, r, 0)
	onlyCall(t, m.srv, "auth.login")
	if serverFor("", nil) != config.DefaultServer {
		t.Fatal("no server, no config: the default server")
	}
}

func TestLoginOfflineAndMissingEmail(t *testing.T) {
	m := newMachine(t, false)
	url := m.srv.URL()
	m.offline()
	r := m.run("pw\n", "login", "-s", url, "-e", "lan@acme.test", "--password-stdin")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "could not reach")
	r = m.run("pw\n", "login", "-s", url, "--password-stdin")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "--email")
	r = m.run("pw\n", "login", "-s", "http://teamflow.example.com", "-e", "lan@acme.test", "--password-stdin")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "https")
	r = m.run("\n", "login", "-s", url, "-e", "lan@acme.test", "--password-stdin")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no password")
}

func TestLogout(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	r := m.run("", "logout")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "signed out")
	if c := onlyCall(t, m.srv, "auth.logout"); c.Auth != "Bearer "+auth {
		t.Fatalf("logout revokes the auth key: %q", c.Auth)
	}
	cfg, _ := config.Load(m.dir)
	if cfg == nil || cfg.AuthKey != "" || cfg.Server != m.srv.URL() {
		t.Fatalf("after logout: %v", cfg)
	}
	if ps, _ := m.store().KeyProjects(m.binding()); len(ps) != 0 {
		t.Fatalf("project keys kept: %v", ps)
	}
}

func TestLogoutOfflineStillForgetsLocally(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.offline()
	r := m.run("", "logout")
	wantCode(t, r, 0)
	cfg, _ := config.Load(m.dir)
	if cfg == nil || cfg.AuthKey != "" {
		t.Fatalf("after logout: %v", cfg)
	}
	if ps, _ := m.store().KeyProjects(m.binding()); len(ps) != 0 {
		t.Fatalf("project keys kept: %v", ps)
	}
	// Signed out already: still fine, nothing sent.
	m2 := newMachine(t, false)
	wantCode(t, m2.run("", "logout"), 0)
	noCall(t, m2.srv, "auth.logout")
}

func TestProjects(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.projects", "account.projects")
	r := m.run("", "projects")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "Acme\tPricing\t"+P+"\n")
	if c := onlyCall(t, m.srv, "account.projects"); c.Auth != "Bearer "+auth {
		t.Fatalf("auth %q", c.Auth)
	}
	r = m.run("", "projects", "--json")
	wantCode(t, r, 0)
	var got struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(r.out), &got); err != nil || len(got.Result) == 0 ||
		got.Result[0]["org"] != "Acme" || got.Result[0]["name"] != "Pricing" || got.Result[0]["id"] != P {
		t.Fatalf("--json: %v %s", err, r.out)
	}
}

// Every command that needs the server: not signed in, offline and UPGRADE_REQUIRED, each a clear refusal (exit 1).
func TestSadPathsEverywhere(t *testing.T) {
	type cmd struct {
		args   []string
		method string // the call to refuse with UPGRADE_REQUIRED
	}
	cmds := []cmd{
		{[]string{"projects"}, "account.projects"},
		{[]string{"org", "members", "Acme"}, "org.members"},
		{[]string{"org", "add-member", "Acme", "new@acme.test"}, "org.add_member"},
		{[]string{"org", "remove-member", "Acme", "huy@acme.test", "--yes"}, "org.members"},
		{[]string{"topic", "delete", "Rounding", "--project", P, "--yes"}, "key.fetch"},
		{[]string{"artifact", "list", "--project", P}, "key.fetch"},
		{[]string{"deliverables", "--project", P}, "key.fetch"},
		{[]string{"log", "--project", P}, "key.fetch"},
	}
	upgrade := `{"error": {"code": -32000, "message": "this server needs client protocol 2", "data": {"reason": "UPGRADE_REQUIRED"}}}`
	for _, c := range cmds {
		name := strings.Join(c.args[:min(2, len(c.args))], " ")
		t.Run("not signed in/"+name, func(t *testing.T) {
			m := newMachine(t, false)
			r := m.run("", c.args...)
			wantCode(t, r, 1)
			contains(t, "stderr", r.err, "not signed in")
			if len(m.srv.methods()) != 0 {
				t.Fatalf("sent %v while signed out", m.srv.methods())
			}
		})
		t.Run("offline/"+name, func(t *testing.T) {
			m := newMachine(t, true)
			m.offline()
			r := m.run("", c.args...)
			wantCode(t, r, 1)
			if !strings.Contains(r.err, "unreachable") && !strings.Contains(r.err, "could not reach") {
				t.Fatalf("offline: %s", r)
			}
		})
		t.Run("upgrade/"+name, func(t *testing.T) {
			m := newMachine(t, true)
			m.srv.on(c.method, upgrade)
			r := m.run("", c.args...)
			wantCode(t, r, 1)
			contains(t, "stderr", r.err, updateText)
		})
	}
}

// A non-member and an unknown project, for the commands that fetch a project key.
func TestNonMemberAndUnknownProject(t *testing.T) {
	for _, tc := range []struct{ answer, reason string }{{"key.fetch!NOT_A_MEMBER", "NOT_A_MEMBER"}, {"key.fetch!UNKNOWN_PROJECT", "UNKNOWN_PROJECT"}} {
		for _, args := range [][]string{
			{"topic", "delete", "Rounding", "--project", P, "--yes"},
			{"artifact", "list", "--project", P},
			{"deliverables", "--project", P},
			{"artifact", "write", "f.md", "--topic", "Rounding", "--summary", "x", "--project", P},
		} {
			t.Run(tc.reason+"/"+strings.Join(args[:2], " "), func(t *testing.T) {
				m := newMachine(t, true)
				t.Chdir(t.TempDir())
				_ = os.WriteFile("f.md", []byte("# x\n"), 0o644)
				m.srv.on("key.fetch", tc.answer)
				r := m.run("", args...)
				wantCode(t, r, 1)
				contains(t, "stderr", r.err, tc.reason)
				if ps, _ := m.store().KeyProjects(m.binding()); len(ps) != 0 {
					t.Fatalf("a key was kept: %v", ps)
				}
			})
		}
	}
}

func TestStatus(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.srv.on("account.me", lanMe)
	m.inSession("s1", P, nil)
	r := m.run("", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out,
		"config: "+filepath.Join(m.dir, "config.json")+" (ok)\n",
		"server: "+m.srv.URL()+"\n",
		"signed in: lan <lan@acme.test> (Acme)\n",
		"project keys on this machine: 1\n",
		"outbox: 0 waiting to be sent\n",
		"this session: s1 (shared with "+P+")\n",
		"shared session s1 -> "+P+"\n",
		"claude-code: not installed\n")
	lacks(t, "stdout", r.out, "client.sqlite", "tfa_", "tfk_")

	// A Python-era store is mentioned (it is no longer used), once.
	if err := os.WriteFile(filepath.Join(m.dir, store.LegacyFileName), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = m.run("", "status")
	if n := strings.Count(r.out, "client.sqlite"); n != 1 {
		t.Fatalf("legacy store mentioned %d times:\n%s", n, r.out)
	}
	contains(t, "stdout", r.out, "is no longer used; you may delete it")
}

func TestStatusSadPaths(t *testing.T) {
	m := newMachine(t, false)
	r := m.run("", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "(missing)")
	noCall(t, m.srv, "account.me")

	m = newMachine(t, true)
	if err := config.Save(m.dir, config.Config{Server: m.srv.URL()}); err != nil {
		t.Fatal(err)
	}
	r = m.run("", "status")
	contains(t, "stdout", r.out, "signed out: run `teamflow login`")

	m = newMachine(t, true)
	m.srv.on("account.me", "account.me!NOT_ALLOWED")
	r = m.run("", "status")
	contains(t, "stdout", r.out, "sign-in: refused (NOT_ALLOWED); run `teamflow login` again")

	m = newMachine(t, true)
	m.srv.on("account.me", `{"error": {"code": -32000, "message": "needs protocol 2", "data": {"reason": "UPGRADE_REQUIRED"}}}`)
	r = m.run("", "status")
	contains(t, "stdout", r.out, updateText)

	m = newMachine(t, true)
	m.offline()
	r = m.run("", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "server: unreachable")
}

// ------------------------------------------------------------------ hidden commands and later tasks

func TestHiddenAndLaterCommands(t *testing.T) {
	m := newMachine(t, true)
	r := m.run(`{"session_id": "s1"}`, "hook", "claude-code", "UserPromptSubmit")
	wantCode(t, r, 0) // a hook never fails the agent
	if r.out != "" || r.err != "" {
		t.Fatalf("hook said something: %s", r)
	}
	// self-update: with the placeholder release key it refuses before any network call; it takes no arguments.
	if selfupdate.PublicKey == "" {
		r = m.run("", "self-update")
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "this build has no release key")
	}
	r = m.run("", "self-update", "now")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "self-update takes no arguments")
	r = m.run("", "self-update", "--help")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "signature")
	r = m.run("", "debug", "state", "--json")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, `"sessions"`)
}

// A corrupt config.json: every command that needs the sign-in refuses (exit 1) without showing the file, and
// nothing is sent.
func TestCorruptConfigRefusesCleanly(t *testing.T) {
	m := newMachine(t, false)
	if err := os.WriteFile(filepath.Join(m.dir, config.FileName), []byte(`{"server": "`+m.srv.URL()+`", "auth_key": "tfa_secret",`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"projects"}, {"artifact", "list", "--project", P}, {"approve", S1}, {"status"}} {
		r := m.run("", args...)
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "config.json is not valid JSON")
		lacks(t, "output", r.out+r.err, "tfa_secret")
	}
	if len(m.srv.methods()) != 0 {
		t.Fatalf("sent %v", m.srv.methods())
	}
}

// An auth key the server no longer takes (expired) after a restart: status says to sign in again, the commands
// refuse with the server's reason, and nothing crashes.
func TestExpiredAuthKeySaysSignInAgain(t *testing.T) {
	m := newMachine(t, true)
	expired := `{"error": {"code": -32000, "message": "your sign-in has expired: sign in again", "data": {"reason": "NOT_ALLOWED"}}}`
	m.srv.on("account.me", expired)
	m.srv.on("account.projects", expired)
	m.srv.on("key.fetch", expired)
	r := m.run("", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "sign-in: refused (NOT_ALLOWED); run `teamflow login` again")
	for _, args := range [][]string{{"projects"}, {"artifact", "list", "--project", P}} {
		r = m.run("", args...)
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "NOT_ALLOWED: your sign-in has expired: sign in again")
		lacks(t, "stderr", r.err, "panic", "goroutine")
	}
}
