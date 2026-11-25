package core

// The hook before each tool (PreToolUse) runs at every step of the agent's turn: it raises new approval requests
// from the replica and touches the network at most once per HookSyncEvery per project, with a short timeout, never
// while marked offline after a failure, and never while the background sync is live.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// midTurnCore is a signed-in client with a project key for P on server, session s1 shared with P, and a clock.
func midTurnCore(t *testing.T, server string) (*Core, *time.Time) {
	t.Helper()
	c := open(t, &config.Config{Server: server, AuthKey: "tfa_example", Email: "lan@acme.test"})
	if err := c.St.PutProjectKey(store.Binding(server, "lan@acme.test"), P, "tfk_example", tM1, "og_x", "Pricing", time.Now()); err != nil {
		t.Fatal(err)
	}
	seedProject(t, c)
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Share("s1", P, tM1, "ref", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.Now = func() time.Time { return now }
	return c, &now
}

// countingServer answers every sync with nothing and counts the calls.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "result": {"events": [], "cursors": {}, "reset": []}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// 100 tool steps in a row: at most one network sync per HookSyncEvery; the next window syncs once more.
func TestMidTurnSyncsAtMostOncePerWindow(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, now := midTurnCore(t, srv.URL)
	for range 100 {
		c.MidTurn(ctx, "s1")
	}
	first := calls.Load()
	if first == 0 || first > 2 { // one sync: a poll (and a push when the spool has signals)
		t.Fatalf("100 steps made %d network calls", first)
	}
	*now = now.Add(HookSyncEvery / 2)
	c.MidTurn(ctx, "s1")
	if calls.Load() != first {
		t.Fatal("synced again inside the window")
	}
	*now = now.Add(HookSyncEvery)
	c.MidTurn(ctx, "s1")
	if calls.Load() == first {
		t.Fatal("never synced again after the window")
	}
	// Another project's window is its own: the claim is per project.
	if ok, err := c.St.ClaimHookSync("pj_other", *now, HookSyncEvery, HookOfflineFor); err != nil || !ok {
		t.Fatalf("other project: %v %v", ok, err)
	}
}

// A black-holed server (accepts, never answers): one wait of about MidTurnTimeout, then no network at all until
// HookOfflineFor has passed; the offline path stays fast (p95 under 30 ms) and still raises what the replica has.
func TestMidTurnBlackHoledServer(t *testing.T) {
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			defer conn.Close() // held open, never answered
		}
	}()
	t.Cleanup(func() { ln.Close() })
	c, now := midTurnCore(t, "http://"+ln.Addr().String())
	start := time.Now()
	c.MidTurn(ctx, "s1")
	if took := time.Since(start); took > MidTurnTimeout+time.Second {
		t.Fatalf("the first step waited %v", took)
	}
	seen := accepted.Load()
	if seen == 0 {
		t.Fatal("never tried the network")
	}
	*now = now.Add(HookSyncEvery + time.Second) // past the window, still inside the offline time
	seedApprovalFor(t, c, "s1")
	var took []time.Duration
	var raised string
	for range 100 {
		s := time.Now()
		if got := c.MidTurn(ctx, "s1"); got != "" {
			raised = got
		}
		took = append(took, time.Since(s))
	}
	if accepted.Load() != seen {
		t.Fatal("tried the network while marked offline")
	}
	if !strings.Contains(raised, tS1) {
		t.Fatalf("the replica's request was not raised offline: %q", raised)
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	if p95 := took[94]; p95 > 30*time.Millisecond {
		t.Fatalf("offline path p95 %v", p95)
	}
	*now = now.Add(HookOfflineFor)
	done := make(chan struct{})
	go func() { c.MidTurn(ctx, "s1"); close(done) }()
	select {
	case <-done:
	case <-time.After(MidTurnTimeout + 2*time.Second):
		t.Fatal("the retry after the offline time hung")
	}
	if accepted.Load() == seen {
		t.Fatal("never tried again after the offline time")
	}
}

// While the background sync is live (heard within LiveFresh), the step never touches the network.
func TestMidTurnTrustsALiveBackgroundSync(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, now := midTurnCore(t, srv.URL)
	if err := c.St.SetLiveAt(P, now.Add(-LiveFresh+time.Second)); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		c.MidTurn(ctx, "s1")
	}
	if calls.Load() != 0 {
		t.Fatalf("%d network calls with a live background sync", calls.Load())
	}
	*now = now.Add(2 * time.Second) // the live mark goes stale
	c.MidTurn(ctx, "s1")
	if calls.Load() == 0 {
		t.Fatal("a stale live mark kept the step off the network")
	}
}

// Signed out, unshared or unknown sessions: nothing, no network.
func TestMidTurnSadPaths(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, _ := midTurnCore(t, srv.URL)
	if got := c.MidTurn(ctx, "nobody"); got != "" {
		t.Fatal(got)
	}
	if _, err := c.St.UpsertSession("loose", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := c.MidTurn(ctx, "loose"); got != "" {
		t.Fatal(got)
	}
	if calls.Load() != 0 {
		t.Fatal("network for no shared session")
	}
	out := open(t, nil)
	if got := out.MidTurn(ctx, "s1"); got != "" {
		t.Fatal(got)
	}
}

// A command looking up a signal or artifact the replica does not have yet (just written, still on its way over the
// live socket) pulls for it even while the background sync is live; one the replica has costs no network.
func TestALookupMissPullsEvenWhileLive(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, now := midTurnCore(t, srv.URL)
	if err := c.St.SetLiveAt(P, *now); err != nil {
		t.Fatal(err)
	}
	accept(t, c, tS1, "Known", 1, protocol.SignalStateInForce, "", "")
	if _, ok, err := c.signalOf(ctx, tS1); err != nil || !ok || calls.Load() != 0 {
		t.Fatalf("a known signal: ok %v, err %v, %d network calls", ok, err, calls.Load())
	}
	missing := "sg_01MISSING0000000000000000"
	if _, ok, err := c.signalOf(ctx, missing); err != nil || ok {
		t.Fatalf("a missing signal: ok %v, err %v", ok, err)
	}
	if calls.Load() == 0 {
		t.Fatal("a lookup miss trusted the live replica and never pulled")
	}
	before := calls.Load()
	if _, err := c.ArtifactArchive(ctx, missing, true, "x"); err == nil || !strings.Contains(err.Error(), "no artifact") {
		t.Fatalf("archive of a missing artifact: %v", err)
	}
	if _, err := c.Verdict(ctx, "s1", missing, protocol.SignalKindApprove, ""); err == nil ||
		!strings.Contains(err.Error(), "no signal") {
		t.Fatalf("a verdict on a missing signal: %v", err)
	}
	if calls.Load() < before+2 {
		t.Fatalf("archive and verdict on a miss pulled %d times, want a pull each", calls.Load()-before)
	}
}

// Offline: PullNow says so (false) and changes nothing; a verdict on a target the replica lacks does not try the
// network a second time once the first sync found the server unreachable.
func TestPullNowOffline(t *testing.T) {
	ctx := context.Background()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c, now := midTurnCore(t, srv.URL)
	if err := c.St.SetLiveAt(P, *now); err != nil {
		t.Fatal(err)
	}
	if c.PullNow(ctx, time.Second, "") {
		t.Fatal("PullNow said online with the server down")
	}
	if n.Load() == 0 {
		t.Fatal("PullNow did not try the network while live")
	}
	if _, ok, err := c.St.Signal("sg_01MISSING0000000000000000"); err != nil || ok {
		t.Fatalf("offline pull changed the replica: %v %v", ok, err)
	}
	if err := c.St.ClearLiveAt(P); err != nil { // not live: the verdict's own sync tries, and fails
		t.Fatal(err)
	}
	n.Store(0)
	if _, err := c.Verdict(ctx, "s1", "sg_01MISSING0000000000000000", protocol.SignalKindApprove, ""); err == nil ||
		!strings.Contains(err.Error(), "no signal") {
		t.Fatalf("an offline verdict on a missing target: %v", err)
	}
	first := n.Load()
	if first == 0 {
		t.Fatal("the verdict never tried the server")
	}
	// one sync attempt (it may retry inside its own request policy), never a second PullNow round: compare with a
	// sync alone
	n.Store(0)
	c.Sync(ctx, DefaultTimeout, "")
	if alone := n.Load(); first > alone {
		t.Fatalf("an offline verdict made %d requests, a sync alone %d: it pulled again after finding the server down", first, alone)
	}
}
