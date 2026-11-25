// Package bench measures what an agent waits for: a hook that needs no network, process start included.
package bench

// The prompt hook (Claude Code's UserPromptSubmit) on a warm store, run as the agent runs it: a fresh `teamflow`
// process per prompt, its stdin the hook input. Its p95 over 200 runs must stay under 30 ms (the hook budget in the
// design), in both cases the prompt hook meets:
//
//   - the background sync already runs (its lock held): StartSync only looks, and the hook answers from the
//     replica the sync keeps fresh; nothing goes to the network;
//   - the background sync is not running (a reboot, its quiet exit): the hook starts it (fork and exec, detached)
//     and answers from the replica all the same. Each started copy is stopped before the next run, outside the time.
//
// The store is warm: signed in, a project with its key, topic, plan, people and 50 signals, the session shared and
// caught up, and the project's live socket heard from just now (so the hook does not pull).

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/live"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

const (
	runs   = 200
	budget = 30 * time.Millisecond
	email  = "lan@acme.test"
	pkey   = "tfk_0123456789abcdefghijklmnopqrstuv"
	proj   = "pj_01BENCH0000000000000000000"
	topic  = "tp_01BENCH0000000000000000000"
	lan    = "mb_01BENCH00000000000000LAN0"
	huy    = "mb_01BENCH00000000000000HUY0"
	sid    = "bench-session"
)

// build compiles the teamflow binary (without -race: the agent runs the release build).
func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "teamflow")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/teamflow")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// server answers nothing useful and counts what reaches it.
func server(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// warm makes a signed-in Teamflow home with a project worth answering from and a shared session caught up on it.
func warm(t *testing.T, bin, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(dir, config.Config{Server: url, AuthKey: "tfa_0123456789abcdefghijklmnopqrstuv", Email: email}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	p := protocol.NewProject()
	p.ID, p.Name, p.Initiator, p.Members, p.Version = proj, "Pricing", lan, []string{lan, huy}, 1
	must(st.PutProject(p))
	must(st.PutTopic(protocol.Topic{ID: topic, Project: proj, Name: "Rounding", Description: "How prices round"}))
	state := protocol.NewProjectState()
	state.Project, state.Version, state.Plan = proj, 1, "Round prices; show them with two decimals."
	must(st.PutState(state))
	must(st.PutPeople(map[string]store.Person{lan: {Name: "Lan", Alias: "Lanny"}, huy: {Name: "Huy", Title: "Dev"}}))
	must(st.PutProjectKey(store.Binding(url, email), proj, pkey, lan, "og_01BENCH", "Pricing", now))
	for i := 1; i <= 50; i++ {
		sig := protocol.NewSignal()
		sig.ID = fmt.Sprintf("sg_01BENCH%017d", i)
		sig.Project, sig.Topic, sig.Kind, sig.Title = proj, topic, protocol.SignalKindDecision, fmt.Sprintf("Decision %d", i)
		body := protocol.NewKeyPointsBody()
		body.Type, body.Summary, body.Points = "key_points.v1", "A decision the team relies on", []string{"a point"}
		sig.Body = body
		s := protocol.NewStoredSignal()
		s.Signal, s.Author.Member, s.Author.Actor = sig, huy, protocol.ActorAgent
		s.Seq, s.Chain, s.ChainVersion, s.State = int64(i), sig.ID, 1, protocol.SignalStateInForce
		ev := protocol.NewSignalAccepted()
		ev.Project, ev.Seq, ev.Type, ev.Signal = proj, int64(i), "signal.accepted", s
		must(st.Tx(func(tx *store.Tx) error {
			if err := syncer.Apply(tx, ev, now); err != nil {
				return err
			}
			return tx.SetCursor(proj, int64(i))
		}))
	}
	_, err = st.UpsertSession(sid, "claude-code", 0, dir, now)
	must(err)
	must(st.Share(sid, proj, lan, api.KeyRef(pkey), nil))
	must(st.SetLiveAt(proj, now))
	st.Close()
	// The first prompt gives the full context and moves the session's bookmark: from then on, each is a delta.
	if out, err := hook(t, bin, dir); err != nil || !strings.Contains(out, "Decision 50") {
		t.Fatalf("the first prompt hook did not give the context (%v):\n%s", err, out)
	}
	// That hook started the background sync (signed in, none running): stop it, so each test sets up its own case.
	if !started(dir, 5*time.Second) {
		t.Fatal("the first prompt hook did not start the background sync")
	}
	if stopped, err := live.Stop(dir, 10*time.Second); err != nil || !stopped {
		t.Fatalf("stopping the background sync: stopped %v, %v", stopped, err)
	}
	return dir
}

func hook(t *testing.T, bin, dir string) (string, error) {
	cmd := exec.Command(bin, "hook", "claude-code", "UserPromptSubmit")
	cmd.Env = append(env(dir), "TEAMFLOW_HOME="+dir)
	cmd.Stdin = strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + dir + `","prompt":"Carry on."}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// env is this process's environment without an agent's markers or another Teamflow home.
func env(string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TEAMFLOW_HOME" || k == "CLAUDE_PID" || k == "CLAUDECODE" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// fresh marks the project's live socket as heard from now (the background sync keeps it so).
func fresh(t *testing.T, dir string) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetLiveAt(proj, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// holdSyncLock holds the background sync's lock as a running copy does (flock, its pid in the file).
func holdSyncLock(t *testing.T, dir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, live.LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() })
}

func p95(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[int(math.Ceil(0.95*float64(len(s))))-1]
}

func report(t *testing.T, what string, ds []time.Duration) {
	t.Helper()
	s := slices.Clone(ds)
	slices.Sort(s)
	got := p95(ds)
	t.Logf("%s: p50 %v, p95 %v, max %v over %d runs", what, s[len(s)/2].Round(10*time.Microsecond),
		got.Round(10*time.Microsecond), s[len(s)-1].Round(10*time.Microsecond), len(s))
	if got >= budget {
		t.Errorf("%s: p95 %v is over the %v budget", what, got, budget)
	}
}

func TestPromptHookP95WithTheSyncRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("200 hook processes")
	}
	bin := build(t)
	srv, calls := server(t)
	dir := warm(t, bin, srv.URL)
	holdSyncLock(t, dir)
	calls.Store(0)
	ds := make([]time.Duration, 0, runs)
	for range runs {
		fresh(t, dir)
		t0 := time.Now()
		if _, err := hook(t, bin, dir); err != nil {
			t.Fatalf("the hook failed: %v", err)
		}
		ds = append(ds, time.Since(t0))
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the hook went to the network %d times; a live project is answered from the replica", n)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "hook.log")); err == nil && len(b) > 0 {
		t.Errorf("the hook logged trouble:\n%s", b)
	}
	report(t, "prompt hook, background sync running", ds)
}

func TestPromptHookP95StartingTheSync(t *testing.T) {
	if testing.Short() {
		t.Skip("200 hook processes, each starting the background sync")
	}
	bin := build(t)
	srv, _ := server(t)
	dir := warm(t, bin, srv.URL)
	t.Cleanup(func() { _, _ = live.Stop(dir, 10*time.Second) })
	ds := make([]time.Duration, 0, runs)
	for i := range runs {
		if _, running := live.Status(dir); running {
			t.Fatalf("run %d: a background sync is still running before the hook", i)
		}
		fresh(t, dir)
		t0 := time.Now()
		if _, err := hook(t, bin, dir); err != nil {
			t.Fatalf("the hook failed: %v", err)
		}
		ds = append(ds, time.Since(t0))
		if !started(dir, 5*time.Second) {
			t.Fatalf("run %d: the prompt hook did not start the background sync", i)
		}
		if stopped, err := live.Stop(dir, 10*time.Second); err != nil || !stopped {
			t.Fatalf("run %d: stopping the background sync: stopped %v, %v", i, stopped, err)
		}
	}
	report(t, "prompt hook, starting the background sync", ds)
}

// started waits until the background sync holds its lock.
func started(dir string, wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for ctx.Err() == nil {
		if _, ok := live.Status(dir); ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// A hook whose home is unreadable or whose input is not JSON adds nothing and never fails the agent's step, and
// is as quick: the budget holds on the sad paths too.
func TestPromptHookSadPathsStayQuiet(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := build(t)
	srv, calls := server(t)
	dir := warm(t, bin, srv.URL)
	holdSyncLock(t, dir)
	calls.Store(0)
	for _, tc := range []struct{ name, stdin string }{
		{"not JSON", "not json"},
		{"no session id", `{"cwd":"/","prompt":"x"}`},
		{"an unknown session", `{"session_id":"nobody","cwd":"/","prompt":"x"}`},
	} {
		cmd := exec.Command(bin, "hook", "claude-code", "UserPromptSubmit")
		cmd.Env = append(env(dir), "TEAMFLOW_HOME="+dir)
		cmd.Stdin = strings.NewReader(tc.stdin)
		t0 := time.Now()
		out, err := cmd.Output()
		took := time.Since(t0)
		if err != nil || len(bytes.TrimSpace(out)) != 0 {
			t.Errorf("%s: exit %v, output %q; want exit 0 and nothing", tc.name, err, out)
		}
		if took >= 10*budget {
			t.Errorf("%s: took %v", tc.name, took)
		}
	}
	// A broken store (the file is not a database) is answered with nothing, at once.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "store.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "hook", "claude-code", "UserPromptSubmit")
	cmd.Env = append(env(broken), "TEAMFLOW_HOME="+broken)
	cmd.Stdin = strings.NewReader(`{"session_id":"` + sid + `","cwd":"/","prompt":"x"}`)
	if out, err := cmd.Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("a broken store: exit %v, output %q; want exit 0 and nothing", err, out)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("sad-path hooks went to the network %d times", n)
	}
}
