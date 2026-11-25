package core

// Signals leave in order (Task 8c-2b), at the core layer with a real store and a fake server: one ordered outbox
// per project, human acts flushing what is queued before them and then going alone, one sender at a time across
// processes, order through errors, and writes that are not queued never overtaking queued ones.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/sendlock"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// outboxServer is a Teamflow server for the outbox: signal.emit keeps the first result per id (as the real one),
// drift acts and plain writes answer, sync.poll answers nothing new. It records every outbox call in the order it
// read them, each request's shape, and the most requests it ever had in flight at once.
type outboxServer struct {
	t          *testing.T
	mu         sync.Mutex
	down       bool          // answer 503
	delay      time.Duration // per request
	callErr    func(id string) *protocol.RpcError
	block      func(n int)     // runs before outbox request n (1-based) is answered, outside the lock
	order      []string        // ids (signal, "drift.x dr_", or a write's method) in arrival order
	shapes     []string        // per outbox request: the batch length, or "alone"
	results    map[string]bool // signal ids with a stored result
	auths      []string        // the Authorization of each outbox request
	inFlight   atomic.Int32
	keyRefused bool   // key.fetch answers NOT_A_MEMBER
	forgot     string // a project key the server no longer knows (a restore, a member re-added): refused for the key
	whole      bool   // with forgot: a batch under that key is refused whole (one error, not an array)
	accepted   []string
	slowKey    string // a project key whose every request waits slowFor
	slowFor    time.Duration
	slowHits   int
	keyFetchN  int
	maxIn      atomic.Int32
	outboxN    int
}

func newOutboxServer(t *testing.T) (*outboxServer, *httptest.Server) {
	s := &outboxServer{t: t, results: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, srv
}

var outboxMethods = map[string]bool{"signal.emit": true, "drift.resolve": true, "drift.dismiss": true,
	"topic.create": true, "artifact.archive": true}

func (s *outboxServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	batch := strings.HasPrefix(strings.TrimSpace(string(body)), "[")
	var reqs []protocol.Request
	if batch {
		_ = json.Unmarshal(body, &reqs)
	} else {
		var one protocol.Request
		_ = json.Unmarshal(body, &one)
		reqs = []protocol.Request{one}
	}
	outbox := outboxMethods[reqs[0].Method]
	s.mu.Lock()
	if s.slowKey != "" && r.Header.Get("Authorization") == "Bearer "+s.slowKey {
		s.slowHits++
		wait := s.slowFor
		s.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
		}
		s.mu.Lock()
	}
	if s.down {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	n := 0
	if outbox {
		s.outboxN++
		n = s.outboxN
		shape := "alone"
		if batch {
			shape = fmt.Sprint(len(reqs))
		}
		s.shapes = append(s.shapes, shape)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
	}
	block, delay := s.block, s.delay
	s.mu.Unlock()
	if outbox {
		if in := s.inFlight.Add(1); in > s.maxIn.Load() {
			s.maxIn.Store(in)
		}
		defer s.inFlight.Add(-1)
		if block != nil {
			block(n)
		}
		time.Sleep(delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	forgotten := s.forgot != "" && r.Header.Get("Authorization") == "Bearer "+s.forgot
	keyRefusal := &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: api.KeyRefusalMessage,
		Data: map[string]any{"reason": "NOT_ALLOWED"}}
	if forgotten && batch && s.whole {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.Response{JSONRPC: "2.0", Error: keyRefusal})
		return
	}
	answer := func(q protocol.Request) protocol.Response {
		out := protocol.Response{JSONRPC: "2.0", ID: q.ID}
		if forgotten && q.Method != "key.fetch" {
			if q.Method == "signal.emit" { // the router's _call: a result, refused for the key
				var p struct {
					Signal struct {
						ID string `json:"id"`
					} `json:"signal"`
				}
				_ = json.Unmarshal(q.Params, &p)
				s.order = append(s.order, p.Signal.ID)
				out.Result = json.RawMessage(fmt.Sprintf(`{"signal": %q, "accepted": false, "reason": "NOT_ALLOWED", "message": %q}`,
					p.Signal.ID, api.KeyRefusalMessage))
				return out
			}
			out.Error = keyRefusal
			return out
		}
		var p struct {
			Drift  string `json:"drift"`
			Signal struct {
				ID string `json:"id"`
			} `json:"signal"`
		}
		_ = json.Unmarshal(q.Params, &p)
		id := ""
		switch q.Method {
		case "signal.emit":
			id = p.Signal.ID
			s.order = append(s.order, id)
		case "drift.resolve", "drift.dismiss":
			id = p.Drift
			s.order = append(s.order, q.Method+" "+id)
		case "topic.create", "artifact.archive":
			s.order = append(s.order, q.Method)
		}
		if s.callErr != nil && id != "" {
			if e := s.callErr(id); e != nil {
				out.Error = e
				return out
			}
		}
		switch q.Method {
		case "signal.emit":
			if !s.results[id] {
				s.accepted = append(s.accepted, id)
			}
			s.results[id] = true
			out.Result = json.RawMessage(fmt.Sprintf(`{"signal": %q, "accepted": true, "message": "", "state": "IN_FORCE"}`, id))
		case "drift.resolve", "drift.dismiss":
			out.Result = json.RawMessage(`{"drifts": []}`)
		case "sync.poll":
			out.Result = json.RawMessage(`{"events": [], "cursors": {}, "reset": []}`)
		case "key.fetch":
			s.keyFetchN++
			if s.keyRefused {
				out.Error = &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "not a member", Data: map[string]any{"reason": "NOT_A_MEMBER"}}
				return out
			}
			out.Result = json.RawMessage(`{"project": "` + P + `", "key": "tfk_minted", "member": "` + tM1 + `", "org": "og_x", "name": "Pricing"}`)
		case "member.list":
			out.Result = json.RawMessage(`{"members": []}`)
		case "topic.create":
			out.Result = json.RawMessage(`{}`)
		case "artifact.archive":
			out.Result = json.RawMessage(`{}`)
		default:
			out.Error = &protocol.RpcError{Code: protocol.CodeMethodNotFound, Message: "no such method " + q.Method}
		}
		return out
	}
	w.Header().Set("Content-Type", "application/json")
	if !batch {
		_ = json.NewEncoder(w).Encode(answer(reqs[0]))
		return
	}
	outs := []protocol.Response{}
	for _, q := range reqs {
		outs = append(outs, answer(q))
	}
	_ = json.NewEncoder(w).Encode(outs)
}

func (s *outboxServer) set(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *outboxServer) fetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyFetchN
}

func (s *outboxServer) got() (order, shapes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...), append([]string(nil), s.shapes...)
}

// outboxCore is midTurnCore (signed in, a key for P, session s1 on P) with a pending signal tS1 asking tM1.
func outboxCore(t *testing.T, server string) *Core {
	t.Helper()
	c, _ := midTurnCore(t, server)
	c.Now = time.Now
	accept(t, c, tS1, "Round prices", 3, protocol.SignalStatePending, "", "2026-12-01")
	return c
}

func note(i int) map[string]any {
	return map[string]any{"kind": "NOTE", "topic": tT1, "title": fmt.Sprintf("note %d", i),
		"body": map[string]any{"type": "comment.v1", "text": "why"}}
}

// queueNotes queues n signals by hand (as an earlier offline session left them), with ids sg_q000…
func queueNotes(t *testing.T, c *Core, from, n int) []string {
	t.Helper()
	var ids []string
	for i := from; i < from+n; i++ {
		e := protocol.NewSignalEmit()
		e.ClientSession, e.Actor, e.ProjectVersion = "s1", protocol.ActorAgent, 1
		e.Signal = protocol.NewSignal()
		e.Signal.ID, e.Signal.Project, e.Signal.Topic, e.Signal.Kind, e.Signal.Title = fmt.Sprintf("sg_q%03d", i), P, tT1, protocol.SignalKindNote, "n"
		e.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "why"}
		b, _ := json.Marshal(e)
		if err := c.St.SpoolAdd(e.Signal.ID, "s1", testOutbox(c), string(b), time.Now()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.Signal.ID)
	}
	return ids
}

// testOutbox is lan's outbox of P on c's server.
func testOutbox(c *Core) store.Outbox {
	return store.OutboxOf(store.Binding(c.Cfg.Server, "lan@acme.test"), P)
}

func outbox(t *testing.T, c *Core) []string {
	t.Helper()
	items, err := c.St.Spooled(testOutbox(c))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Signal)
	}
	return out
}

func verdictID(t *testing.T, c *Core) string {
	t.Helper()
	items, err := c.St.Spooled(testOutbox(c))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if strings.Contains(it.Params, `"APPROVE"`) || strings.Contains(it.Params, `"REJECT"`) {
			return it.Signal
		}
	}
	t.Fatal("no verdict queued")
	return ""
}

// Offline: three signals queue, then the person approves. The verdict waits behind them (never sent ahead) and
// the command says how many are ahead. Back online, the next sync sends the three in one batch, then the verdict
// alone, and the session hears that it reached the team. Sad paths: offline, a human act behind a queue, restart
// of nothing lost (a fresh Core on the same home sends it).
func TestOfflineVerdictWaitsBehindQueuedSignals(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	s.set(func() { s.down = true })
	answers, err := c.Emit(ctx, "s1", []map[string]any{note(0), note(1), note(2)})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range answers {
		if q, _ := a.Get("queued"); q != true {
			t.Fatalf("offline emit answered %v", a)
		}
	}
	_, err = c.Verdict(ctx, "s1", tS1, protocol.SignalKindApprove, "")
	var q *Queued
	if !errors.As(err, &q) || q.Ahead != 3 {
		t.Fatalf("offline verdict: %v, want queued behind 3", err)
	}
	if !strings.Contains(q.Error(), "will be sent after 3 earlier signals") {
		t.Fatalf("message %q", q.Error())
	}
	if len(outbox(t, c)) != 4 {
		t.Fatalf("outbox %v", outbox(t, c))
	}
	if order, _ := s.got(); len(order) != 0 {
		t.Fatalf("sent while offline: %v", order)
	}
	// Back online, a fresh process (the same home) flushes.
	s.set(func() { s.down = false })
	c2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if !c2.Sync(ctx, DefaultTimeout, "") {
		t.Fatal("offline")
	}
	order, shapes := s.got()
	// The head goes alone first (the offline requests failed whole), then the rest in a batch, then the verdict.
	if len(order) != 4 || order[3] != q.Item || strings.Join(shapes, ",") != "1,2,alone" {
		t.Fatalf("order %v shapes %v", order, shapes)
	}
	if len(outbox(t, c)) != 0 {
		t.Fatal("the outbox is not empty")
	}
	ns, _ := c.St.Notices("s1", false)
	told := false
	for _, n := range ns {
		told = told || (n.About == q.Item && n.Type == protocol.NoticeTypeSignalSent)
	}
	if !told {
		t.Fatalf("the session never heard the verdict was sent: %+v", ns)
	}
}

// Nothing queued: the human act is sent at once, alone, and its answer is the command's. Then a queue of 57
// (left by an earlier offline time): a drift act flushes them first, in batches, then goes alone.
func TestHumanActSentAtOnceOrAfterTheQueue(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	r, err := c.Verdict(ctx, "s1", tS1, protocol.SignalKindReject, "too risky")
	if err != nil || !r.Accepted {
		t.Fatalf("verdict: %+v %v", r, err)
	}
	if _, shapes := s.got(); strings.Join(shapes, ",") != "alone" {
		t.Fatalf("shapes %v", shapes)
	}
	ids := queueNotes(t, c, 0, protocol.MaxBatch+7)
	if _, err := c.DriftAct(ctx, P, "s1", "drift.resolve", "dr_1", "fixed in the plan", nil); err != nil {
		t.Fatal(err)
	}
	order, shapes := s.got()
	if strings.Join(shapes, ",") != fmt.Sprintf("alone,%d,7,alone", protocol.MaxBatch) {
		t.Fatalf("shapes %v", shapes)
	}
	if strings.Join(order[1:], ",") != strings.Join(append(ids, "drift.resolve dr_1"), ",") {
		t.Fatalf("order %v", order)
	}
	if len(outbox(t, c)) != 0 {
		t.Fatal("left queued")
	}
}

// Order through errors: a refusal mid-queue drops that item and the act still goes after the rest; a transient
// error mid-queue stops the outbox, so the act waits behind the stuck item (queued, never sent ahead), and goes
// alone, last, when the stuck one clears. Sad paths: INVALID_PARAMS mid-queue, a call the server could not answer.
func TestHumanActThroughErrorsInTheQueue(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 5)
	s.set(func() {
		s.callErr = func(id string) *protocol.RpcError {
			switch id {
			case ids[1]:
				return &protocol.RpcError{Code: protocol.CodeInvalidParams, Message: "title too long"}
			case ids[3]:
				return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "try again"}
			}
			return nil
		}
	})
	_, err := c.DriftAct(ctx, P, "s1", "drift.dismiss", "dr_1", "not a drift", nil)
	var q *Queued
	if !errors.As(err, &q) || q.Ahead != 1 {
		t.Fatalf("behind a stuck item: %v", err)
	}
	if got := outbox(t, c); len(got) != 2 || got[0] != ids[3] || got[1] != q.Item {
		t.Fatalf("outbox %v", got)
	}
	ns, _ := c.St.Notices("s1", false)
	refused := false
	for _, n := range ns {
		refused = refused || (n.About == ids[1] && n.Type == protocol.NoticeTypeSignalRejected)
	}
	if !refused {
		t.Fatalf("no notice of the refusal: %+v", ns)
	}
	s.set(func() { s.callErr = nil })
	c.Sync(ctx, DefaultTimeout, "")
	order, shapes := s.got()
	if order[len(order)-1] != "drift.dismiss dr_1" || order[len(order)-2] != ids[3] || shapes[len(shapes)-1] != "alone" {
		t.Fatalf("order %v shapes %v", order, shapes)
	}
}

// Writes that are not queued (topic changes, artifact archive, …) never overtake queued signals: they flush the
// outbox first, and while it cannot be emptied they are not sent and say why. Sad paths: a stuck queue (not sent,
// with the count), offline (not sent), then a clear queue (sent after the queue).
func TestWritesNeverOvertakeQueuedSignals(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 3)
	s.set(func() {
		s.callErr = func(id string) *protocol.RpcError {
			if id == ids[0] {
				return &protocol.RpcError{Code: protocol.CodeInternalError, Message: "try again"}
			}
			return nil
		}
	})
	a, err := c.API(ctx, P, DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Call(ctx, "topic.create", map[string]any{}, nil)
	var ns *NotSent
	if !errors.As(err, &ns) || ns.Waiting != 1 || !strings.Contains(err.Error(), "1 earlier signal") {
		t.Fatalf("stuck queue: %v", err)
	}
	s.set(func() { s.down = true; s.callErr = nil })
	if err := a.Call(ctx, "artifact.archive", map[string]any{}, nil); !errors.As(err, &ns) || !errors.Is(err, api.ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	order, _ := s.got()
	for _, o := range order {
		if o == "topic.create" || o == "artifact.archive" {
			t.Fatalf("a write overtook the queue: %v", order)
		}
	}
	s.set(func() { s.down = false })
	if err := a.Call(ctx, "topic.create", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	order, _ = s.got()
	if order[len(order)-1] != "topic.create" || order[len(order)-2] != ids[0] {
		t.Fatalf("order %v", order)
	}
	// A read never waits on the outbox.
	queueNotes(t, c, 10, 1)
	s.set(func() { s.down = true })
	if err := a.Call(ctx, "project.get", map[string]any{}, nil); !errors.Is(err, api.ErrOffline) || errors.As(err, &ns) {
		t.Fatalf("a read: %v", err)
	}
}

// Every server method is classified: a write to the project's shared record (it flushes the outbox first, or is
// queued) or not. A new method fails this until it is decided.
func TestEveryMethodIsClassified(t *testing.T) {
	for name := range protocol.Methods {
		if SharedWrites[name] == NotSharedWrites[name] {
			t.Errorf("%s: classify it in SharedWrites or NotSharedWrites", name)
		}
	}
	for name := range SharedWrites {
		if _, ok := protocol.Methods[name]; !ok {
			t.Errorf("SharedWrites names %s, which the server does not have", name)
		}
	}
}

// Two clients on one machine (separate store handles, as two processes) send at once: commands emitting, human
// acts, syncs. The server never has two outbox requests in flight, nothing is sent twice, and each client's own
// items arrive in the order it made them. Run with -race.
func TestTwoSendersNeverInterleave(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	s.delay = 5 * time.Millisecond
	c := outboxCore(t, srv.URL)
	queueNotes(t, c, 0, 30)
	c2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var wg sync.WaitGroup
	made := [2][]string{}
	for w, cl := range []*Core{c, c2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 6 {
				answers, err := cl.Emit(ctx, "s1", []map[string]any{note(100*w + i)})
				if err != nil {
					t.Error(err)
					return
				}
				id, _ := answers[0].Get("signal")
				made[w] = append(made[w], fmt.Sprint(id))
				if i == 3 {
					_, err := cl.DriftAct(ctx, P, "s1", "drift.resolve", fmt.Sprintf("dr_%d", w), "done", nil)
					var q *Queued
					if err != nil && !errors.As(err, &q) {
						t.Error(err)
					}
					made[w] = append(made[w], fmt.Sprintf("drift.resolve dr_%d", w))
				}
				cl.Sync(ctx, DefaultTimeout, "")
			}
		}()
	}
	wg.Wait()
	c.Sync(ctx, DefaultTimeout, "") // whatever a busy lock left queued
	if s.maxIn.Load() != 1 {
		t.Fatalf("%d outbox requests in flight at once", s.maxIn.Load())
	}
	order, _ := s.got()
	seen := map[string]int{}
	pos := map[string]int{}
	for i, o := range order {
		seen[o]++
		pos[o] = i
	}
	for o, n := range seen {
		if n != 1 {
			t.Fatalf("%s sent %d times", o, n)
		}
	}
	if len(order) != 30+2*7 {
		t.Fatalf("%d sent, want %d: %v", len(order), 30+2*7, order)
	}
	for w := range made {
		for i := 1; i < len(made[w]); i++ {
			if pos[made[w][i-1]] > pos[made[w][i]] {
				t.Fatalf("client %d: %s sent before %s", w, made[w][i], made[w][i-1])
			}
		}
	}
	if len(outbox(t, c)) != 0 {
		t.Fatal("left queued")
	}
}

// ------------------------------------------------------------------ real processes

// The helper process: TEST_OUTBOX_MODE=sync syncs once (sending the outbox); hold takes the sender lock of the test
// key, says so and sleeps until killed.
func TestMain(m *testing.M) {
	switch os.Getenv("TEST_OUTBOX_MODE") {
	case "sync":
		c, err := Open()
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println("syncing")
		c.Sync(context.Background(), DefaultTimeout, "")
		os.Exit(0)
	case "hold":
		c, err := Open()
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		release, err := sendlock.Acquire(context.Background(), os.Getenv("TEAMFLOW_HOME"), testOutbox(c).Ref(), 0)
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		defer release()
		fmt.Println("held")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(t *testing.T, mode string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TEST_OUTBOX_MODE="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, bufio.NewReader(out)
}

// Two real processes flush one outbox at the same time: one sender at a time (never two requests in flight), every
// signal sent once, in order.
func TestTwoProcessesFlushingAtOnce(t *testing.T) {
	s, srv := newOutboxServer(t)
	s.delay = 20 * time.Millisecond
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 2*protocol.MaxBatch+10)
	a, ra := helper(t, "sync")
	b, rb := helper(t, "sync")
	_, _ = ra.ReadString('\n')
	_, _ = rb.ReadString('\n')
	_ = a.Wait()
	_ = b.Wait()
	c.Sync(context.Background(), DefaultTimeout, "")
	order, _ := s.got()
	if s.maxIn.Load() != 1 {
		t.Fatalf("%d outbox requests in flight at once", s.maxIn.Load())
	}
	if strings.Join(order, ",") != strings.Join(ids, ",") {
		t.Fatalf("sent %d (want %d once each, in order)", len(order), len(ids))
	}
}

// A process killed (SIGKILL) while its second batch is on the wire: the first batch was settled before it went, so
// a restart resends none of it; the killed batch and the rest go again, in order, and the server keeps one result
// per signal.
func TestCrashBetweenBatchesResumesInOrder(t *testing.T) {
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 2*protocol.MaxBatch+5)
	reached := make(chan struct{})
	release := make(chan struct{})
	s.set(func() {
		s.block = func(n int) {
			if n == 2 {
				close(reached)
				<-release
			}
		}
	})
	cmd, r := helper(t, "sync")
	_, _ = r.ReadString('\n')
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the second batch never came")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	s.set(func() { s.block = nil })
	close(release)
	if got := outbox(t, c); len(got) != len(ids)-protocol.MaxBatch || got[0] != ids[protocol.MaxBatch] {
		t.Fatalf("after the crash the outbox holds %d from %v", len(got), got[:1])
	}
	c2, err := Open() // the restart
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.Sync(context.Background(), DefaultTimeout, "")
	order, _ := s.got()
	count := map[string]int{}
	for _, o := range order {
		count[o]++
	}
	for i, id := range ids {
		want := 1
		if i >= protocol.MaxBatch && i < 2*protocol.MaxBatch {
			want = 2 // the killed batch: read by the server, never answered, sent again
		}
		if count[id] != want {
			t.Fatalf("%s sent %d times, want %d", id, count[id], want)
		}
	}
	if len(s.results) != len(ids) || len(outbox(t, c)) != 0 {
		t.Fatalf("%d results, outbox %d", len(s.results), len(outbox(t, c)))
	}
}

// The sender lock held by another process: a human act waits briefly, then is queued (never sent around the
// holder), and the command says so. The holder dies (SIGKILL): its lock goes with it, and the next sync sends the
// act.
func TestLockHeldByADeadProcess(t *testing.T) {
	old := SendLockWait
	SendLockWait = 200 * time.Millisecond
	t.Cleanup(func() { SendLockWait = old })
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	cmd, r := helper(t, "hold")
	if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != "held" {
		t.Fatalf("helper said %q", line)
	}
	_, err := c.DriftAct(ctx, P, "s1", "drift.resolve", "dr_1", "done", nil)
	var q *Queued
	if !errors.As(err, &q) || q.Ahead != 0 {
		t.Fatalf("lock held: %v", err)
	}
	if order, _ := s.got(); len(order) != 0 {
		t.Fatalf("sent around the holder: %v", order)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	c.Sync(ctx, DefaultTimeout, "")
	if order, _ := s.got(); strings.Join(order, ",") != "drift.resolve dr_1" {
		t.Fatalf("after the holder died: %v", order)
	}
}

// ------------------------------------------------------------------ the outbox belongs to a sign-in and a project

func signInAs(t *testing.T, c *Core, server, email, auth string) {
	t.Helper()
	cfg := config.Config{Server: server, AuthKey: auth, Email: email}
	if err := config.Save(c.Dir, cfg); err != nil {
		t.Fatal(err)
	}
	c.Cfg = &cfg
}

// Offline, two signals and a verdict queue. The person logs out (keys dropped) and logs in again as the same
// account: the next sync fetches a new key for the project and sends the queue under it, the verdict last and
// alone. Sad paths: logout with a queue, the server down at logout, a key minted anew.
func TestQueuedVerdictSurvivesLogoutAndLogin(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	s.set(func() { s.down = true })
	if _, err := c.Emit(ctx, "s1", []map[string]any{note(0), note(1)}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Verdict(ctx, "s1", tS1, protocol.SignalKindApprove, "")
	var q *Queued
	if !errors.As(err, &q) || q.Ahead != 2 {
		t.Fatalf("offline verdict: %v", err)
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := c.KeyProjects(); len(ps) != 0 {
		t.Fatalf("keys kept after logout: %v", ps)
	}
	if len(outbox(t, c)) != 3 {
		t.Fatal("the outbox was lost at logout")
	}
	signInAs(t, c, srv.URL, "lan@acme.test", "tfa_again")
	s.set(func() { s.down = false })
	if !c.Sync(ctx, DefaultTimeout, "") {
		t.Fatal("offline")
	}
	order, shapes := s.got()
	if len(order) != 3 || order[2] != q.Item || strings.Join(shapes, ",") != "1,1,alone" { // the head alone first
		t.Fatalf("order %v shapes %v", order, shapes)
	}
	s.mu.Lock()
	auths := append([]string(nil), s.auths...)
	s.mu.Unlock()
	for _, a := range auths {
		if a != "Bearer tfk_minted" {
			t.Fatalf("sent with %q, not the new key", a)
		}
	}
	if len(outbox(t, c)) != 0 {
		t.Fatal("left queued")
	}
}

// The project key is refused and dropped (NOT_A_MEMBER after a re-add, say) while items wait: the next sync
// fetches the key the server mints now and sends them, in order, the verdict alone and last.
func TestQueueGoesUnderAReMintedKey(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 2)
	s.set(func() { s.down = true })
	_, err := c.DriftAct(ctx, P, "s1", "drift.dismiss", "dr_1", "not a drift", nil)
	var q *Queued
	if !errors.As(err, &q) || q.Ahead != 2 {
		t.Fatalf("offline: %v", err)
	}
	if err := c.St.DropProjectKey(mustBinding(t, c), P); err != nil {
		t.Fatal(err)
	}
	s.set(func() { s.down = false })
	c.Sync(ctx, DefaultTimeout, "")
	order, shapes := s.got()
	if strings.Join(order, ",") != strings.Join(append(ids, "drift.dismiss dr_1"), ",") || strings.Join(shapes, ",") != "1,1,alone" {
		t.Fatalf("order %v shapes %v", order, shapes)
	}
}

// Another account signs in on this machine: the first account's queued items are kept, never sent under the new
// account's key, and shown (status, the outbox list); the new account's own verdict is not held behind them.
// Logging back in as the first account sends them.
func TestAnotherAccountNeverSendsTheQueue(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 2)
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	signInAs(t, c, srv.URL, "huy@acme.test", "tfa_huy")
	if err := c.St.PutProjectKey(store.Binding(srv.URL, "huy@acme.test"), P, "tfk_huy", tM2, "og_x", "Pricing", time.Now()); err != nil {
		t.Fatal(err)
	}
	c.Sync(ctx, DefaultTimeout, "")
	if order, _ := s.got(); len(order) != 0 {
		t.Fatalf("another account's queue was sent: %v", order)
	}
	items, err := c.Outbox()
	if err != nil || len(items) != 2 || items[0].Mine || items[0].Account != "lan@acme.test" {
		t.Fatalf("outbox %+v %v", items, err)
	}
	status := strings.Join(c.Status(ctx, "", func(p string) string { return p }), "\n")
	if !strings.Contains(status, "outbox: 0 waiting to be sent") ||
		!strings.Contains(status, "outbox: 2 waiting from lan@acme.test on "+srv.URL+": never sent by this sign-in") {
		t.Fatalf("status:\n%s", status)
	}
	if _, err := c.DriftAct(ctx, P, "", "drift.resolve", "dr_1", "done", nil); err != nil {
		t.Fatalf("huy's own act: %v", err)
	}
	if order, _ := s.got(); strings.Join(order, ",") != "drift.resolve dr_1" {
		t.Fatalf("order %v", order)
	}
	// lan again: the queue goes.
	signInAs(t, c, srv.URL, "lan@acme.test", "tfa_example")
	c.Sync(ctx, DefaultTimeout, "")
	if order, _ := s.got(); strings.Join(order[1:], ",") != strings.Join(ids, ",") {
		t.Fatalf("order %v", order)
	}
}

// A hook's sync flushes within its budget: a long queue on a slow server is cut off at the deadline (the rest
// waits for the next sync), never holding the hook.
func TestHookSyncFlushIsBounded(t *testing.T) {
	s, srv := newOutboxServer(t)
	s.delay = 300 * time.Millisecond
	_ = s
	c := outboxCore(t, srv.URL)
	queueNotes(t, c, 0, 5*protocol.MaxBatch)
	start := time.Now()
	c.Sync(context.Background(), 500*time.Millisecond, P)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the sync took %v", d)
	}
	if n := len(outbox(t, c)); n == 0 {
		t.Fatalf("outbox %d after the bounded sync", n)
	}
}

// Another sender holds the lock past the wait: a write that is not queued still goes when nothing is queued
// (nothing to overtake), and is not sent while something is.
func TestGateWithABusyLock(t *testing.T) {
	old := SendLockWait
	SendLockWait = 100 * time.Millisecond
	t.Cleanup(func() { SendLockWait = old })
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	release, err := sendlock.Acquire(ctx, c.Dir, testOutbox(c).Ref(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	a, err := c.API(ctx, P, DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Call(ctx, "topic.create", map[string]any{}, nil); err != nil {
		t.Fatalf("busy lock, empty outbox: %v", err)
	}
	queueNotes(t, c, 0, 1)
	var ns *NotSent
	if err := a.Call(ctx, "topic.create", map[string]any{}, nil); !errors.As(err, &ns) || !errors.Is(err, sendlock.ErrBusy) {
		t.Fatalf("busy lock, one queued: %v", err)
	}
	if order, _ := s.got(); strings.Join(order, ",") != "topic.create" {
		t.Fatalf("order %v", order)
	}
}

// The person drops a stuck item: it is never sent, the rest goes. Sad paths: an id not queued, dropped twice.
func TestOutboxDrop(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 3)
	if ok, err := c.OutboxDrop(ctx, ids[1]); !ok || err != nil {
		t.Fatalf("drop: %v %v", ok, err)
	}
	if ok, err := c.OutboxDrop(ctx, ids[1]); ok || err != nil {
		t.Fatalf("drop again: %v %v", ok, err)
	}
	if ok, _ := c.OutboxDrop(ctx, "sg_nothing"); ok {
		t.Fatal("dropped what was not queued")
	}
	c.Sync(ctx, DefaultTimeout, "")
	if order, _ := s.got(); strings.Join(order, ",") != ids[0]+","+ids[2] {
		t.Fatalf("order %v", order)
	}
}

// Offline with no key for the project (logged out and in again, a key dropped): a human act and an agent's
// signals are still recorded (queued), not refused; back online the sync fetches the key and sends them in order.
// Sad paths: no key and no server, a verdict with no stored member.
func TestOfflineActWithNoKeyIsQueued(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	if err := c.St.DropProjectKey(mustBinding(t, c), P); err != nil {
		t.Fatal(err)
	}
	s.set(func() { s.down = true })
	answers, err := c.Emit(ctx, "s1", []map[string]any{note(0)})
	if err != nil {
		t.Fatalf("emit with no key, offline: %v", err)
	}
	if q, _ := answers[0].Get("queued"); q != true {
		t.Fatalf("emit answer %v", answers[0])
	}
	_, err = c.Verdict(ctx, "s1", tS1, protocol.SignalKindApprove, "")
	var q *Queued
	if !errors.As(err, &q) || q.Ahead != 1 || q.Hard() {
		t.Fatalf("verdict with no key, offline: %v", err)
	}
	_, err = c.DriftAct(ctx, P, "s1", "drift.dismiss", "dr_1", "not a drift", nil)
	if !errors.As(err, &q) || q.Ahead != 2 {
		t.Fatalf("drift act with no key, offline: %v", err)
	}
	s.set(func() { s.down = false })
	c.Sync(ctx, DefaultTimeout, "")
	order, shapes := s.got()
	if len(order) != 3 || order[2] != "drift.dismiss dr_1" || strings.Join(shapes, ",") != "1,alone,alone" {
		t.Fatalf("order %v shapes %v", order, shapes)
	}
}

// Items wait for a project whose membership the server refuses (key.fetch: NOT_A_MEMBER): the key is not asked for
// again on every command or hook, only once the wait (KeyRetryAfter) is over.
func TestKeyFetchForARefusedMembershipBacksOff(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	queueNotes(t, c, 0, 1)
	if err := c.St.DropProjectKey(mustBinding(t, c), P); err != nil {
		t.Fatal(err)
	}
	s.set(func() { s.keyRefused = true })
	now := time.Now()
	c.Now = func() time.Time { return now }
	for range 5 {
		c.Sync(ctx, DefaultTimeout, "")
	}
	if n := s.fetches(); n != 1 {
		t.Fatalf("key.fetch %d times within the wait", n)
	}
	now = now.Add(KeyRetryAfter + time.Minute)
	c.Sync(ctx, DefaultTimeout, "")
	if n := s.fetches(); n != 2 {
		t.Fatalf("key.fetch %d times after the wait", n)
	}
	if len(outbox(t, c)) != 1 {
		t.Fatal("the item was lost")
	}
}

// rejectedNotices are the "rejected" notices session s1 holds.
func rejectedNotices(t *testing.T, c *Core) []store.Notice {
	t.Helper()
	ns, err := c.St.Notices("s1", false)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Notice
	for _, n := range ns {
		if n.Type == protocol.NoticeTypeSignalRejected {
			out = append(out, n)
		}
	}
	return out
}

func (s *outboxServer) acceptedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.accepted...)
}

// The server forgets this machine's project key (restored from a backup, the member re-added) while two signals
// wait: signal.emit answers accepted:false NOT_ALLOWED "use a project key for this project", or the whole batch is
// refused for the key. Both are kept (never a "rejected" notice), the key is dropped, a new one fetched with the
// auth key, and both are sent under it once, in order. Sad paths: a stale key (restore), the per-call refusal and
// the whole-batch refusal, no notice for a refusal that is about the key, a fresh process after the refusal.
func TestForgottenKeyKeepsTheQueue(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(fmt.Sprintf("whole=%v", whole), func(t *testing.T) {
			ctx := context.Background()
			s, srv := newOutboxServer(t)
			c := outboxCore(t, srv.URL)
			ids := queueNotes(t, c, 0, 2)
			s.set(func() { s.forgot, s.whole = "tfk_example", whole })
			c.Sync(ctx, DefaultTimeout, "")
			if got := s.acceptedIDs(); strings.Join(got, ",") != strings.Join(ids, ",") {
				t.Fatalf("accepted %v, want %v once each, in order", got, ids)
			}
			if n := len(outbox(t, c)); n != 0 {
				t.Fatalf("outbox %v", outbox(t, c))
			}
			if ns := rejectedNotices(t, c); len(ns) != 0 {
				t.Fatalf("a key refusal told the session its signals were rejected: %+v", ns)
			}
			if key, _, _, _, _ := c.St.ProjectKey(mustBinding(t, c), P); key != "tfk_minted" {
				t.Fatalf("key %q, want the new one", key)
			}
			if n := s.fetches(); n != 1 {
				t.Fatalf("key.fetch %d times", n)
			}
		})
	}
}

// The same refusal while the agent emits (its own signal behind a queued one) and while the person acts (a drift
// act, whose refusal is a RULE_FAILED error with the same message): nothing is dropped, the agent's signal and the
// act are answered by the server under the new key, after what was queued before them.
func TestForgottenKeyOnEmitAndHumanAct(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 1)
	s.set(func() { s.forgot = "tfk_example" })
	answers, err := c.Emit(ctx, "s1", []map[string]any{note(1)})
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := answers[0].Get("accepted"); a != true {
		t.Fatalf("emit answered %v", answers[0])
	}
	got := s.acceptedIDs()
	if len(got) != 2 || got[0] != ids[0] {
		t.Fatalf("accepted %v", got)
	}
	// The minted key is forgotten in turn: a drift act behind a queued signal.
	more := queueNotes(t, c, 5, 1)
	if err := c.St.PutProjectKey(mustBinding(t, c), P, "tfk_minted", tM1, "og_x", "Pricing", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.set(func() { s.forgot = "tfk_minted" })
	// key.fetch mints tfk_minted again: forget it only for the first round, then take it.
	n := 0
	s.set(func() {
		s.block = func(int) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if n++; n > 1 {
				s.forgot = ""
			}
		}
	})
	if _, err := c.DriftAct(ctx, P, "s1", "drift.dismiss", "dr_1", "not a drift", nil); err != nil {
		t.Fatalf("drift act: %v", err)
	}
	order, _ := s.got()
	if last := order[len(order)-1]; last != "drift.dismiss dr_1" || !slices.Contains(s.acceptedIDs(), more[0]) {
		t.Fatalf("order %v accepted %v", order, s.acceptedIDs())
	}
	if ns := rejectedNotices(t, c); len(ns) != 0 {
		t.Fatalf("rejected notices %+v", ns)
	}
}

// The member really is gone: the key is refused and key.fetch is refused too (NOT_A_MEMBER). Every item stays
// (never a "rejected" notice), the key is dropped, and the flush asks for a key once, never looping. A new key
// refused again in the same flush is not asked for a second time. Sad paths: revoked membership, a renewed key
// also refused, no loop, nothing lost.
func TestForgottenKeyWithTheMembershipGone(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 2)
	s.set(func() { s.forgot, s.keyRefused = "tfk_example", true })
	c.Sync(ctx, DefaultTimeout, "")
	if got := outbox(t, c); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("outbox %v, want both kept", got)
	}
	if ns := rejectedNotices(t, c); len(ns) != 0 {
		t.Fatalf("rejected notices %+v", ns)
	}
	if _, _, _, _, ok := c.St.ProjectKey(mustBinding(t, c), P); ok {
		t.Fatal("the refused key was kept")
	}
	if n := s.fetches(); n != 1 {
		t.Fatalf("key.fetch %d times", n)
	}
	// A renewed key refused as well: one renewal per flush, the items kept.
	if err := c.St.PutProjectKey(mustBinding(t, c), P, "tfk_example", tM1, "og_x", "Pricing", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.set(func() { s.forgot, s.keyRefused, s.keyFetchN = "", false, 0 })
	s.set(func() {
		s.callErr = func(string) *protocol.RpcError {
			return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: api.KeyRefusalMessage, Data: map[string]any{"reason": "NOT_ALLOWED"}}
		}
	})
	c.Sync(ctx, DefaultTimeout, "")
	if got := outbox(t, c); len(got) != 2 {
		t.Fatalf("outbox %v", got)
	}
	if n := s.fetches(); n != 1 {
		t.Fatalf("key.fetch %d times in one flush", n)
	}
	if ns := rejectedNotices(t, c); len(ns) != 0 {
		t.Fatalf("rejected notices %+v", ns)
	}
}

// Any other NOT_ALLOWED refusal is the item's own: it leaves the outbox and its session is told (the key match is
// exact, never a reason alone). Sad path: a reasoned refusal that is not about the key.
func TestOtherNotAllowedStillRejects(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	ids := queueNotes(t, c, 0, 1)
	s.set(func() {
		s.callErr = func(string) *protocol.RpcError {
			return &protocol.RpcError{Code: protocol.CodeRuleFailed, Message: "only the org's admin can do this", Data: map[string]any{"reason": "NOT_ALLOWED"}}
		}
	})
	c.Sync(ctx, DefaultTimeout, "")
	if got := outbox(t, c); len(got) != 0 {
		t.Fatalf("outbox %v", got)
	}
	if ns := rejectedNotices(t, c); len(ns) != 1 || ns[0].About != ids[0] {
		t.Fatalf("notices %+v", ns)
	}
	if n := s.fetches(); n != 0 {
		t.Fatalf("key.fetch %d times for an item's own refusal", n)
	}
}

// A verdict syncs only its target's project, before and after it is sent: another project of this machine on a
// slow server never holds the person's approve. Sad paths: a second project whose every request is slow; a
// target the replica does not know yet (every project is synced to find it, as before).
func TestVerdictSyncsOnlyItsProject(t *testing.T) {
	ctx := context.Background()
	s, srv := newOutboxServer(t)
	c := outboxCore(t, srv.URL)
	const P2 = "pj_00000000000000000000000002"
	if err := c.St.PutProjectKey(mustBinding(t, c), P2, "tfk_p2", tM1, "og_x", "Ops", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.set(func() { s.slowKey, s.slowFor = "tfk_p2", 3*time.Second })
	start := time.Now()
	r, err := c.Verdict(ctx, "s1", tS1, protocol.SignalKindApprove, "")
	if err != nil || !r.Accepted {
		t.Fatalf("verdict: %+v %v", r, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the approve took %v behind another project's slow server", d)
	}
	s.mu.Lock()
	hits := s.slowHits
	s.mu.Unlock()
	if hits != 0 {
		t.Fatalf("the other project was synced %d times for this verdict", hits)
	}
	// A target not in the replica: every project is synced to find it (and it is then refused as unknown).
	s.set(func() { s.slowFor = 50 * time.Millisecond })
	if _, err := c.Verdict(ctx, "s1", "sg_00000000000000000000000099", protocol.SignalKindApprove, ""); err == nil ||
		!strings.Contains(err.Error(), "no signal") {
		t.Fatalf("unknown target: %v", err)
	}
	s.mu.Lock()
	hits = s.slowHits
	s.mu.Unlock()
	if hits == 0 {
		t.Fatal("an unknown target did not look in every project")
	}
}
