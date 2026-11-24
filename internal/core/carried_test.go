package core

// Carried from Task 9a's re-review: Delta's reads and bookmark in one transaction, the whole mid-turn sync bounded
// by MidTurnTimeout, and a clock set back never keeping the hook off the network for good.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// acceptErr is accept without t (for a goroutine): a teammate's signal applied and the cursor moved, one commit.
func acceptErr(c *Core, id, title string, seq int64) error {
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = id, P, tT1, protocol.SignalKindDecision, title
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points = "key_points.v1", "s", []string{"p"}
	sig.Body = body
	s := protocol.NewStoredSignal()
	s.Signal, s.Author.Member, s.Author.Actor = sig, tM2, protocol.ActorAgent
	s.Seq, s.Chain, s.ChainVersion, s.State = seq, id, 1, protocol.SignalStateInForce
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal = P, seq, "signal.accepted", s
	return c.St.Tx(func(tx *store.Tx) error {
		if err := syncer.Apply(tx, ev, time.Now()); err != nil {
			return err
		}
		return tx.SetCursor(P, seq)
	})
}

// A sync committing a teammate's signal while Delta reads must never move the bookmark past a signal the session
// was not shown: the signal is in this prompt's update or the next one, exactly once.
// Sad paths: a concurrent writer (the background sync) racing the read; the bookmark never skips.
func TestDeltaNeverSkipsASignalCommittedMidRead(t *testing.T) {
	c := open(t, offlineCfg())
	seedProject(t, c)
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Share("s1", P, tM1, "ref", nil); err != nil {
		t.Fatal(err)
	}
	accept(t, c, "sg_0000000000000000000000FRST", "First", 1, protocol.SignalStateInForce, "", "")
	if err := c.St.Seen("s1", 1, 1); err != nil {
		t.Fatal(err)
	}
	const raced = "sg_0000000000000000000000RACE"
	done := make(chan error, 1)
	var fired atomic.Bool
	deltaRead = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		go func() { // the background sync: retried while the store is busy, as a sync would on its next pass
			for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(5 * time.Millisecond) {
				err := acceptErr(c, raced, "Raced in", 2)
				if err == nil || !errors.Is(err, store.ErrBusy) {
					done <- err
					return
				}
			}
			done <- errors.New("the sync never committed")
		}()
		select { // let it commit now if it can (it could, before the fix)
		case err := <-done:
			done <- err
		case <-time.After(400 * time.Millisecond):
		}
	}
	t.Cleanup(func() { deltaRead = func() {} })
	first, err := c.Delta("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second, err := c.Delta("s1")
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Count(first, raced) + strings.Count(second, raced)
	if n != 1 {
		t.Fatalf("the raced signal was told %d times (first %q, second %q)", n, first, second)
	}
}

// slowServer answers every call after delay (a slow but answering server); each sync.poll resets the project, so
// a pull keeps asking for pages.
func slowServer(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc": "2.0", "id": 1, "result": {"events": [], "cursors": {%q: 0}, "reset": [%q]}}`, P, P)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// The mid-turn sync as a whole is bounded by MidTurnTimeout, not each of its requests: a server answering every
// request in 0.7 s (pages of a pull) still lets the step go on in about 1 s, and the project is then kept off the
// network for HookOfflineFor.
// Sad paths: a slow server (each request under the timeout, the sum over it); the offline mark after it.
func TestMidTurnBoundsTheWholeSync(t *testing.T) {
	ctx := context.Background()
	srv, calls := slowServer(t, 700*time.Millisecond)
	c, now := midTurnCore(t, srv.URL)
	start := time.Now()
	c.MidTurn(ctx, "s1")
	if took := time.Since(start); took > MidTurnTimeout+400*time.Millisecond {
		t.Fatalf("the step waited %v for a slow server (%d requests)", took, calls.Load())
	}
	seen := calls.Load()
	*now = now.Add(HookSyncEvery + time.Second)
	c.MidTurn(ctx, "s1")
	if calls.Load() != seen {
		t.Fatal("a timed-out sync did not keep the step off the network")
	}
}

// A clock set back (by hand, or a VM restored) must not keep the hook off the network until it catches up: a last
// sync time later than now, or an offline mark further ahead than HookOfflineFor, are stale.
// Sad paths: the clock set back an hour after a sync; after an offline mark; an offline mark still in force.
func TestMidTurnClockSetBack(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, now := midTurnCore(t, srv.URL)
	c.MidTurn(ctx, "s1")
	seen := calls.Load()
	if seen == 0 {
		t.Fatal("never synced")
	}
	*now = now.Add(-time.Hour)
	c.MidTurn(ctx, "s1")
	if calls.Load() == seen {
		t.Fatal("a last sync time in the future kept the step off the network")
	}
	seen = calls.Load()
	*now = now.Add(time.Hour)
	if err := c.St.SetHookOffline(P, now.Add(HookOfflineFor)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(HookSyncEvery + time.Second) // past the window, the offline mark in force
	c.MidTurn(ctx, "s1")
	if calls.Load() != seen {
		t.Fatal("synced while marked offline")
	}
	*now = now.Add(-time.Hour) // set back: the mark is now more than HookOfflineFor ahead
	c.MidTurn(ctx, "s1")
	if calls.Load() == seen {
		t.Fatal("an offline mark far in the future kept the step off the network")
	}
}
