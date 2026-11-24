package mcpserver

// The test harness: an httptest server answering with the exported golden answers (picked by what they hold, never
// by file number), a Teamflow home on disk, a fake session finder, and an MCP client over in-memory transports.

import (
	"bytes"
	"context"
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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
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
	D1      = "dr_00000000000000000000000001"
	auth    = "tfa_example"
	projKey = "tfk_example"
	email   = "lan@acme.test"
)

var exportDir, _ = filepath.Abs(filepath.Join("..", "..", "protocol"))

// ------------------------------------------------------------------ goldens, by content

type rpcGolden struct {
	Params   json.RawMessage `json:"params"`
	Response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Data struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"error"`
	} `json:"response"`
	raw json.RawMessage // the whole response
}

// goldens are every golden answer of a method, in file order.
func goldens(t *testing.T, method string) []rpcGolden {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(exportDir, "golden", method+".*.json"))
	num := func(f string) int {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), method+"."), ".json"))
		return n
	}
	sort.Slice(files, func(i, j int) bool { return num(files[i]) < num(files[j]) })
	var out []rpcGolden
	for _, f := range files {
		if num(f) == 0 {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var g rpcGolden
		var whole struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &whole); err != nil {
			t.Fatal(err)
		}
		g.raw = whole.Response
		out = append(out, g)
	}
	return out
}

// golden is the first golden answer of method that want accepts.
func golden(t *testing.T, method, what string, want func(g rpcGolden) bool) string {
	t.Helper()
	for _, g := range goldens(t, method) {
		if want(g) {
			return string(g.raw)
		}
	}
	t.Fatalf("no golden %s answer is %s: re-export the protocol", method, what)
	return ""
}

// ok is the first golden answer of method that is a result; refused the first refused for reason.
func ok(t *testing.T, method string) string {
	t.Helper()
	return golden(t, method, "a result", func(g rpcGolden) bool { return g.Response.Error == nil })
}

func refused(t *testing.T, method, reason string) string {
	t.Helper()
	return golden(t, method, "refused "+reason, func(g rpcGolden) bool {
		return g.Response.Error != nil && g.Response.Error.Data.Reason == reason
	})
}

// resultHas is a golden whose result holds s.
func resultHas(t *testing.T, method, s string) string {
	t.Helper()
	return golden(t, method, "holding "+s, func(g rpcGolden) bool {
		return g.Response.Error == nil && strings.Contains(string(g.Response.Result), s)
	})
}

func result(t *testing.T, answer string, out any) {
	t.Helper()
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(answer), &r); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------------ the fake server

type rec struct {
	Method string
	Params json.RawMessage
	Auth   string
	Batch  bool
}

type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []rec
	answers map[string][]string
	down    bool
	srv     *httptest.Server
}

func newServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, answers: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// on sets a method's answers, in turn (the last one repeats): a whole JSON-RPC answer {"result": …} / {"error": …}.
func (f *fakeServer) on(method string, answers ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = answers
}

func (f *fakeServer) offline(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
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

func (f *fakeServer) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeServer) answer(method string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	queue := f.answers[method]
	switch {
	case len(queue) > 1:
		f.answers[method] = queue[1:]
		return json.RawMessage(queue[0])
	case len(queue) == 1:
		return json.RawMessage(queue[0])
	case method == "sync.poll":
		return json.RawMessage(`{"result": {"events": [], "cursors": {}, "reset": []}}`)
	}
	return json.RawMessage(`{"error": {"code": -32601, "message": "no answer in the test for ` + method + `"}}`)
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
	type callT struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	var calls []callT
	batch := bytes.HasPrefix(bytes.TrimSpace(body), []byte("["))
	if batch {
		_ = json.Unmarshal(body, &calls)
	} else {
		var c callT
		_ = json.Unmarshal(body, &c)
		calls = []callT{c}
	}
	var out []json.RawMessage
	for _, c := range calls {
		f.mu.Lock()
		f.calls = append(f.calls, rec{Method: c.Method, Params: c.Params, Auth: r.Header.Get("Authorization"), Batch: batch})
		f.mu.Unlock()
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(f.answer(c.Method), &resp); err != nil {
			f.t.Errorf("bad answer for %s: %v", c.Method, err)
			resp = map[string]json.RawMessage{}
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

// ------------------------------------------------------------------ a machine: a home, a session, the server

// finder is a fake SessionFinder: the session with this id, if the store has it.
type finder struct {
	id  string
	err error
}

func (f *finder) Current(st *store.Store) (store.Session, bool, error) {
	if f.err != nil {
		return store.Session{}, false, f.err
	}
	if f.id == "" {
		return store.Session{}, false, nil
	}
	return st.Session(f.id)
}

type machine struct {
	t    *testing.T
	dir  string
	srv  *fakeServer
	find *finder
	cs   *mcp.ClientSession
}

// newMachine is a fresh Teamflow home: signed in on the fake server (when signedIn), with this machine's key for P,
// P and its topics in the replica, and the agent session "s1" shared with P as M1 on all its topics.
func newMachine(t *testing.T, signedIn bool) *machine {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMFLOW_HOME", dir)
	m := &machine{t: t, dir: dir, srv: newServer(t), find: &finder{id: "s1"}}
	m.srv.on("member.list", ok(t, "member.list"))
	if signedIn {
		if err := config.Save(dir, config.Config{Server: m.srv.srv.URL, AuthKey: auth, Email: email}); err != nil {
			t.Fatal(err)
		}
		m.withStore(func(st *store.Store) error {
			return st.PutProjectKey(store.Binding(m.srv.srv.URL, email), P, projKey, M1, "og_00000000000000000000000001", "Pricing", time.Now())
		})
	}
	m.replica()
	m.session("s1", P, nil)
	return m
}

func (m *machine) withStore(fn func(st *store.Store) error) {
	m.t.Helper()
	st, err := store.Open(m.dir)
	if err != nil {
		m.t.Fatal(err)
	}
	defer st.Close()
	if err := fn(st); err != nil {
		m.t.Fatal(err)
	}
}

// replica puts P (as the golden project.get answer has it: topics Rounding, Currency and Plan) and names.
func (m *machine) replica() {
	m.t.Helper()
	var view protocol.ProjectView
	result(m.t, ok(m.t, "project.get"), &view)
	m.withStore(func(st *store.Store) error {
		return st.Tx(func(tx *store.Tx) error {
			if err := tx.PutProject(view.Project); err != nil {
				return err
			}
			for _, tp := range view.Topics {
				if err := tx.PutTopic(tp); err != nil {
					return err
				}
			}
			return tx.PutNames(view.Names)
		})
	})
}

// session records an agent session, shared with project (when given) as M1, following topics (nil: all).
func (m *machine) session(id, project string, topics []string) {
	m.t.Helper()
	m.withStore(func(st *store.Store) error {
		if _, err := st.UpsertSession(id, "agent", os.Getpid(), m.dir, time.Now()); err != nil {
			return err
		}
		if project == "" {
			return nil
		}
		return st.Share(id, project, M1, "ref", topics)
	})
}

// connect starts the MCP server for this machine and a client on it.
func (m *machine) connect() *mcp.ClientSession {
	m.t.Helper()
	if m.cs != nil {
		return m.cs
	}
	ctx := context.Background()
	server := New(core.Open, m.find, "test")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		m.t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { cs.Close(); ss.Close() })
	m.cs = cs
	return cs
}

// reply is a tool's answer: its text blocks and whether it is an error.
type reply struct {
	blocks []string
	err    bool
}

func (r reply) text() string { return strings.Join(r.blocks, "\n") }

func (r reply) String() string { return fmt.Sprintf("error=%v %q", r.err, r.blocks) }

// obj is the answer as one JSON object.
func (r reply) obj(t *testing.T) map[string]any {
	t.Helper()
	if r.err || len(r.blocks) != 1 {
		t.Fatalf("want one JSON object, got %s", r)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.blocks[0]), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.blocks[0])
	}
	return out
}

// objs are the answer's blocks, each one JSON object (a Python list answer).
func (r reply) objs(t *testing.T) []map[string]any {
	t.Helper()
	if r.err {
		t.Fatalf("an error: %s", r)
	}
	var out []map[string]any
	for _, b := range r.blocks {
		var o map[string]any
		if err := json.Unmarshal([]byte(b), &o); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, b)
		}
		out = append(out, o)
	}
	return out
}

func (m *machine) call(name string, args map[string]any) reply {
	m.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := m.connect().CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return reply{blocks: []string{err.Error()}, err: true}
	}
	var r reply
	r.err = res.IsError
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			r.blocks = append(r.blocks, tc.Text)
		}
	}
	return r
}

func (m *machine) params(method string) map[string]any {
	m.t.Helper()
	got := m.srv.sent(method)
	if len(got) == 0 {
		m.t.Fatalf("%s not sent; calls: %v", method, m.srv.methods())
	}
	var p map[string]any
	if err := json.Unmarshal(got[len(got)-1].Params, &p); err != nil {
		m.t.Fatal(err)
	}
	return p
}

func (m *machine) notSent(method string) {
	m.t.Helper()
	if got := m.srv.sent(method); len(got) != 0 {
		m.t.Fatalf("%s must not be sent; calls: %v", method, m.srv.methods())
	}
}

func wantErr(t *testing.T, r reply, want string) {
	t.Helper()
	if !r.err || !strings.Contains(r.text(), want) {
		t.Fatalf("want an error with %q, got %s", want, r)
	}
}

func wantText(t *testing.T, r reply, want ...string) {
	t.Helper()
	if r.err {
		t.Fatalf("an error: %s", r)
	}
	for _, w := range want {
		if !strings.Contains(r.text(), w) {
			t.Fatalf("want %q in %s", w, r)
		}
	}
}

func applyEvent(tx *store.Tx, ev protocol.SyncEvent) error { return syncer.Apply(tx, ev, time.Now()) }

func now() time.Time { return time.Now() }
