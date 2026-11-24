// Package adaptertest is what the agent adapters' tests share: a temp HOME (never the person's own agent configs
// or Teamflow home), a fake Teamflow server, a replica seeded with a project, and the install suite every adapter
// passes (fresh, twice, over the person's own config byte for byte, malformed and read-only refused without
// writing, uninstall restoring). Used only by tests.
package adaptertest

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// Ids of the seeded project.
const (
	P     = "pj_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	Lan   = "mb_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	Huy   = "mb_7ZZZZZZZZZZZZZZZZZZZZZZZZ2"
	Plan  = "tp_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	Round = "tp_7ZZZZZZZZZZZZZZZZZZZZZZZZ2"
	S1    = "sg_7ZZZZZZZZZZZZZZZZZZZZZZZZ1"
	S2    = "sg_7ZZZZZZZZZZZZZZZZZZZZZZZZ2"
	S3    = "sg_7ZZZZZZZZZZZZZZZZZZZZZZZZ3"
	Email = "huy@acme.test"
)

// Must fails the test on an error.
func Must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Env is a temp HOME with a Teamflow home in it, and the binary's path (with a space: it must be quoted).
type Env struct {
	Home, TFHome, Bin string
}

// NewEnv sets HOME and TEAMFLOW_HOME to temp folders and clears every agent's config override, so no test can
// reach the person's own ~/.claude, ~/.copilot, ~/.cursor, ~/.gemini or ~/.teamflow.
func NewEnv(t testing.TB) *Env {
	t.Helper()
	root := t.TempDir()
	e := &Env{Home: filepath.Join(root, "home"), Bin: filepath.Join(root, "my bin", "teamflow")}
	Must(t, os.MkdirAll(e.Home, 0o755))
	t.Setenv("HOME", e.Home)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "COPILOT_HOME", "TEAMFLOW_SHARE"} {
		t.Setenv(k, "")
	}
	e.TFHome = filepath.Join(e.Home, ".teamflow")
	Must(t, os.Mkdir(e.TFHome, 0o700))
	t.Setenv("TEAMFLOW_HOME", e.TFHome)
	return e
}

// Path is a path under HOME.
func (e *Env) Path(rel string) string { return filepath.Join(e.Home, rel) }

// Write writes content at HOME/rel (folders made).
func (e *Env) Write(t testing.TB, rel, content string, mode os.FileMode) {
	t.Helper()
	Must(t, os.MkdirAll(filepath.Dir(e.Path(rel)), 0o755))
	Must(t, os.WriteFile(e.Path(rel), []byte(content), mode))
	Must(t, os.Chmod(e.Path(rel), mode))
}

// Read is HOME/rel's content ("" when missing).
func (e *Env) Read(rel string) string {
	b, _ := os.ReadFile(e.Path(rel))
	return string(b)
}

// Snapshot is every file under HOME but the Teamflow home: path → mode and content.
func (e *Env) Snapshot(t testing.TB) map[string]string {
	t.Helper()
	out := map[string]string{}
	Must(t, filepath.WalkDir(e.Home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == e.TFHome {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if p != e.Home {
				out[p+"/"] = "dir"
			}
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		b, _ := os.ReadFile(p)
		out[p] = fmt.Sprintf("%v\n%s", fi.Mode(), b)
		return nil
	}))
	return out
}

// SameSnapshot fails when two snapshots differ, naming the files.
func SameSnapshot(t testing.TB, what string, want, got map[string]string) {
	t.Helper()
	var diffs []string
	for p, w := range want {
		if g, ok := got[p]; !ok {
			diffs = append(diffs, "gone: "+p)
		} else if g != w {
			diffs = append(diffs, fmt.Sprintf("changed: %s\n--- want\n%s\n--- got\n%s", p, w, g))
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, "new: "+p)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("%s:\n%s", what, strings.Join(diffs, "\n"))
	}
}

// ------------------------------------------------------------------ the install suite

// Spec is one adapter's install, for the suite.
type Spec struct {
	New func() adapters.Adapter
	// Setup runs after NewEnv (an agent's own env overrides).
	Setup func(t *testing.T, e *Env)
	// Own is the person's own config before Teamflow (HOME-relative path → content): every file install edits,
	// with entries of their own (including an MCP server and a deny rule of theirs).
	Own map[string]string
	// JSONFiles are the HOME-relative JSON files install edits (each refused when malformed or read-only).
	JSONFiles []string
	// Wrong are HOME-relative files with content of the wrong shape install must refuse (path → content).
	Wrong map[string]string
	// Check runs after a fresh install: every artefact is there.
	Check func(t *testing.T, e *Env)
}

// RunInstallSuite runs the install and uninstall cases every adapter passes.
func RunInstallSuite(t *testing.T, s Spec) {
	setup := func(t *testing.T) *Env {
		e := NewEnv(t)
		if s.Setup != nil {
			s.Setup(t, e)
		}
		return e
	}
	t.Run("fresh-twice-uninstall", func(t *testing.T) {
		e := setup(t)
		a := s.New()
		before := e.Snapshot(t)
		if a.Installed() {
			t.Fatal("installed before install")
		}
		lines, err := a.Install(e.Bin)
		Must(t, err)
		for _, l := range lines {
			if strings.Contains(l, e.Home) {
				t.Fatalf("a home path in the output: %q", l)
			}
		}
		s.Check(t, e)
		if !a.Installed() {
			t.Fatal("not installed after install")
		}
		once := e.Snapshot(t)
		_, err = a.Install(e.Bin)
		Must(t, err)
		SameSnapshot(t, "the second install changed files", once, e.Snapshot(t))
		_, err = a.Uninstall()
		Must(t, err)
		if a.Installed() {
			t.Fatal("installed after uninstall")
		}
		SameSnapshot(t, "uninstall did not restore the fresh machine", before, e.Snapshot(t))
		_, err = a.Uninstall()
		Must(t, err)
		SameSnapshot(t, "a second uninstall changed files", before, e.Snapshot(t))
	})
	t.Run("over-the-persons-own-config", func(t *testing.T) {
		e := setup(t)
		for p, c := range s.Own {
			e.Write(t, p, c, 0o640)
		}
		before := e.Snapshot(t)
		a := s.New()
		_, err := a.Install(e.Bin)
		Must(t, err)
		s.Check(t, e)
		for p, c := range s.Own { // the person's own entries are still there
			if slices.Contains(s.JSONFiles, p) {
				var own, now map[string]any
				Must(t, json.Unmarshal([]byte(c), &own))
				Must(t, json.Unmarshal([]byte(e.Read(p)), &now))
				for k := range own {
					if _, ok := now[k]; !ok {
						t.Fatalf("%s lost the person's %q", p, k)
					}
				}
			} else if !strings.Contains(e.Read(p), c) {
				t.Fatalf("%s lost the person's text", p)
			}
			if fi, err := os.Stat(e.Path(p)); err != nil || fi.Mode().Perm() != 0o640 {
				t.Fatalf("%s mode changed: %v %v", p, fi.Mode(), err)
			}
		}
		once := e.Snapshot(t)
		_, err = a.Install(e.Bin)
		Must(t, err)
		SameSnapshot(t, "the second install changed files", once, e.Snapshot(t))
		_, err = a.Uninstall()
		Must(t, err)
		SameSnapshot(t, "uninstall did not restore the person's config byte for byte", before, e.Snapshot(t))
	})
	refuse := func(t *testing.T, e *Env, what string) {
		t.Helper()
		before := e.Snapshot(t)
		_, err := s.New().Install(e.Bin)
		if err == nil {
			t.Fatalf("%s: install took it", what)
		}
		if strings.Contains(err.Error(), e.Home) {
			t.Fatalf("%s: a home path in the message: %v", what, err)
		}
		SameSnapshot(t, what+": install wrote", before, e.Snapshot(t))
	}
	for _, f := range s.JSONFiles {
		for _, bad := range []string{"{bad", "[1, 2]", `{"a": 1} {"b": 2}`, `{"a": 1, "a": 2}`} {
			t.Run("malformed "+f, func(t *testing.T) {
				e := setup(t)
				for p, c := range s.Own {
					e.Write(t, p, c, 0o644)
				}
				e.Write(t, f, bad, 0o644)
				refuse(t, e, f+" "+bad)
				// Uninstall refuses it too, writing nothing.
				before := e.Snapshot(t)
				if _, err := s.New().Uninstall(); err == nil {
					t.Fatal("uninstall took a malformed file")
				}
				SameSnapshot(t, "uninstall wrote", before, e.Snapshot(t))
			})
		}
		t.Run("read-only "+f, func(t *testing.T) {
			e := setup(t)
			for p, c := range s.Own {
				e.Write(t, p, c, 0o644)
			}
			if _, ok := s.Own[f]; !ok {
				e.Write(t, f, "{}\n", 0o644)
			}
			Must(t, os.Chmod(e.Path(f), 0o444))
			refuse(t, e, f+" read-only")
		})
	}
	for f, c := range s.Wrong {
		t.Run("wrong shape "+f+" "+c, func(t *testing.T) {
			e := setup(t)
			e.Write(t, f, c, 0o644)
			refuse(t, e, f+" "+c)
		})
	}
	t.Run("a link to nothing", func(t *testing.T) {
		if len(s.JSONFiles) == 0 {
			t.Skip()
		}
		e := setup(t)
		Must(t, os.MkdirAll(filepath.Dir(e.Path(s.JSONFiles[0])), 0o755))
		Must(t, os.Symlink(filepath.Join(e.Home, "nowhere.json"), e.Path(s.JSONFiles[0])))
		refuse(t, e, "a dangling link")
	})
}

// ------------------------------------------------------------------ a fake server and a seeded replica

// Server is a fake Teamflow server: sync.poll answers nothing new, other methods what Set gave.
type Server struct {
	mu      sync.Mutex
	srv     *httptest.Server
	answers map[string]string
	down    bool
	Calls   []string
}

// NewServer starts a fake server.
func NewServer(t testing.TB) *Server {
	f := &Server{answers: map[string]string{}}
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
		f.Calls = append(f.Calls, c.Method)
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

// URL is the server's address.
func (f *Server) URL() string { return f.srv.URL }

// Set gives a method's whole answer ({"result": …} or {"error": …}).
func (f *Server) Set(method, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = answer
}

// Offline makes every call fail with 502 (down) or answer again.
func (f *Server) Offline(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// Count is how many calls of method the server had.
func (f *Server) Count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == method {
			n++
		}
	}
	return n
}

// Machine is a temp HOME with a Teamflow home signed in on a fake server, Pricing (Plan, Rounding; plan "Ship
// pricing v1") in the replica, and one adapter whose hooks run as `teamflow hook <agent> <event>` would.
type Machine struct {
	T   *testing.T
	Env *Env
	Srv *Server
	A   adapters.Adapter
	seq int64
}

// NewMachine is a Machine; signed out when !signedIn.
func NewMachine(t *testing.T, a adapters.Adapter, signedIn bool) *Machine {
	t.Helper()
	m := &Machine{T: t, Env: NewEnv(t), Srv: NewServer(t), A: a}
	if !signedIn {
		return m
	}
	Must(t, config.Save(m.Env.TFHome, config.Config{Server: m.Srv.URL(), AuthKey: "tfa_example", Email: Email}))
	m.Store(func(tx *store.Tx) error {
		if err := tx.PutProjectKey(store.Binding(m.Srv.URL(), Email), P, "tfk_example", Huy, "og_7ZZZZZZZZZZZZZZZZZZZZZZZZ1", "Pricing", time.Now()); err != nil {
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

// Store runs fn in one transaction on the machine's store (a fresh open, as another process would).
func (m *Machine) Store(fn func(tx *store.Tx) error) {
	m.T.Helper()
	st, err := store.Open(m.Env.TFHome)
	Must(m.T, err)
	defer st.Close()
	Must(m.T, st.Tx(fn))
}

// Session is the recorded session sid.
func (m *Machine) Session(sid string) (s store.Session, ok bool) {
	m.Store(func(tx *store.Tx) error {
		var err error
		s, ok, err = tx.Session(sid)
		return err
	})
	return s, ok
}

// Share records session sid shared with Pricing as member.
func (m *Machine) Share(sid, member string) {
	m.Store(func(tx *store.Tx) error {
		if _, err := tx.UpsertSession(sid, m.A.Name(), 0, "", time.Now()); err != nil {
			return err
		}
		return tx.Share(sid, P, member, "ref", nil)
	})
}

// Emit applies a teammate's decision as the sync would (the cursor moves with it); approver asks for approval.
func (m *Machine) Emit(id, by, title, approver string) {
	m.seq++
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = id, P, Round, protocol.SignalKindDecision, title
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points = "key_points.v1", "s", []string{"p"}
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
	m.Store(func(tx *store.Tx) error {
		if err := syncer.Apply(tx, ev, time.Now()); err != nil {
			return err
		}
		return tx.SetCursor(P, m.seq)
	})
}

// Hook runs one hook as `teamflow hook <agent> <event>` would: a fresh client from disk each time.
func (m *Machine) Hook(event, stdin string) string {
	m.T.Helper()
	return string(adapters.RunHook(m.A, event, []byte(stdin), core.Open))
}

// HookLog is the Teamflow home's hook.log.
func (m *Machine) HookLog() string {
	b, _ := os.ReadFile(filepath.Join(m.Env.TFHome, "hook.log"))
	return string(b)
}

// Field is a string field of a hook's JSON answer (fails when the answer is not JSON).
func Field(t testing.TB, out, key string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	s, _ := v[key].(string)
	return s
}
