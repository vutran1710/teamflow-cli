package live

// The background sync (Task 10) against a fake server speaking the live protocol: events land in the replica with
// no hook, the protocol's rules (ping, silence, reset, the close reasons, seq holes), reconnects with no gap and no
// duplicate (a crash and restart too), the sync.poll fallback, one copy per machine, the quiet exit, no secret in
// a log or URL, the hooks' freshness rule, and the outbox sent by one sender at a time with a command beside it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"golang.org/x/sys/unix"
)

const (
	email   = "lan@acme.test"
	authKey = "tfa_authkeyAUTHSECRETVALUE"
)

func keyOf(p string) string { return "tfk_projkey" + p[len(p)-2:] + "PROJECTSECRETVALUE" }

// machine is a Teamflow home (a temp HOME too: nothing of the person's is touched) signed in on f, with a key for
// each project and a session s1 (member M1) shared on the first one.
func machine(t *testing.T, f *fake, projects ...string) *core.Core {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMFLOW_HOME", dir)
	if err := config.Save(dir, config.Config{Server: f.URL(), AuthKey: authKey, Email: email}); err != nil {
		t.Fatal(err)
	}
	c, err := core.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for _, p := range projects {
		if err := c.St.PutProjectKey(binding(c), p, keyOf(p), M1, "og_00000000000000000000000001", "Pricing", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(projects) > 0 {
		if err := c.St.Share("s1", projects[0], M1, "ref", nil); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func binding(c *core.Core) store.KeyBinding { return store.Binding(c.Cfg.Server, email) }

// logs collects what the sync logs.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", a...)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// applied records the seqs the sync applied, per project.
type applied struct {
	mu   sync.Mutex
	seqs map[string][]int64
}

func (a *applied) add(p string, seq int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seqs[p] = append(a.seqs[p], seq)
}

func (a *applied) of(p string) []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int64(nil), a.seqs[p]...)
}

// fast is the protocol's timing scaled down for tests.
func fast(l *logs, a *applied) Options {
	o := Options{
		IdleExit: time.Hour, PingEvery: 60 * time.Millisecond, Silence: 150 * time.Millisecond,
		FirstAnswer: 2 * time.Second, Backoff: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
		SocketRetry: 300 * time.Millisecond, Rescan: 20 * time.Millisecond,
	}
	o.Logf = func(string, ...any) {}
	if l != nil {
		o.Logf = l.Logf
	}
	if a != nil {
		if a.seqs == nil {
			a.seqs = map[string][]int64{}
		}
		o.Applied = a.add
	}
	return o
}

// start runs the sync until the test ends; wait gives its result.
func start(t *testing.T, c *core.Core, o Options) (wait func() error, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, o) }()
	var once sync.Once
	var res error
	wait = func() error {
		once.Do(func() {
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				res = errors.New("the sync never returned")
			}
		})
		return res
	}
	t.Cleanup(func() { cancel(); _ = wait() })
	return wait, cancel
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cursor(c *core.Core, p string) int64 { return c.St.Cursors()[p] }

func topicNames(t *testing.T, c *core.Core, p string) []string {
	t.Helper()
	ts, err := c.St.Topics(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range ts {
		out = append(out, x.Name)
	}
	slices.Sort(out)
	return out
}

func hasKey(c *core.Core, p string) bool {
	_, _, _, _, ok := c.St.ProjectKey(binding(c), p)
	return ok
}

func seqRange(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// ------------------------------------------------------------------ events

// An event committed on the server lands in the replica with no hook running; an approval request reaches the
// approver's session within seconds and the next hook shows it from the replica, with no network call. An event
// variant or live message this client does not know is skipped, never a crash; an approval for someone else tells
// this session nothing.
func TestEventLandsWithoutAHook(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2, 3)
	c := machine(t, f, P1)
	l := &logs{}
	start(t, c, fast(l, nil))
	eventually(t, "the catch-up applied", func() bool { return cursor(c, P1) == 3 && f.liveOn(P1) })
	if got := topicNames(t, c, P1); !slices.Equal(got, []string{"T1", "T2", "T3"}) {
		t.Fatalf("topics %v", got)
	}
	if at := c.St.LiveAt(P1); at.IsZero() || time.Since(at) > core.LiveFresh {
		t.Fatalf("live_at %v", at)
	}

	// Sad: a newer server's event variant and live message, and someone else's approval request.
	f.push(P1, 4, json.RawMessage(`{"project": "`+P1+`", "seq": 4, "at": "2026-01-15T09:00:00Z", "type": "future.thing", "x": 1}`))
	f.sendRaw(P1, `{"type": "future", "whatever": true}`)
	f.push(P1, 5, approvalEvent(P1, 5, "sg_00000000000000000000000009", M2))
	eventually(t, "the unknown event skipped and the cursor moved on", func() bool { return cursor(c, P1) == 5 })

	polls := f.count("sync.poll")
	sent := time.Now()
	f.push(P1, 6, approvalEvent(P1, 6, "sg_00000000000000000000000001", M1))
	eventually(t, "the approval request stored for the session", func() bool {
		ns, _ := c.St.Notices("s1", true)
		for _, n := range ns {
			if n.Type == protocol.NoticeTypeApprovalRequested && n.About == "sg_00000000000000000000000001" {
				return true
			}
		}
		return false
	})
	if d := time.Since(sent); d > 2*time.Second {
		t.Fatalf("the approval request took %v", d)
	}
	out := c.MidTurn(context.Background(), "s1")
	if !strings.Contains(out, "sg_00000000000000000000000001") || strings.Contains(out, "sg_00000000000000000000000009") {
		t.Fatalf("the hook said %q", out)
	}
	if n := f.count("sync.poll"); n != polls {
		t.Fatalf("the hook went to the network (%d polls)", n-polls)
	}
	if again := c.MidTurn(context.Background(), "s1"); strings.Contains(again, "sg_00000000000000000000000001") {
		t.Fatal("the approval request was raised twice")
	}
	if !strings.Contains(l.String(), `skipped "future" live messages`) {
		t.Fatalf("the unknown live message was not logged: %s", l)
	}
}

// ------------------------------------------------------------------ heartbeat

// Once live the client pings ("ping", exactly) every PingEvery; while pongs come the socket stays. When nothing is
// heard within Silence of a ping the socket is dead: the client closes it and reconnects with a fresh ticket from
// its cursor.
func TestPingPongAndSilenceReconnects(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2)
	c := machine(t, f, P1)
	start(t, c, fast(nil, nil))
	eventually(t, "live", func() bool { return f.liveOn(P1) })
	time.Sleep(500 * time.Millisecond)
	f.mu.Lock()
	pings, nonPing := f.pings, append([]string(nil), f.nonPing...)
	f.mu.Unlock()
	if pings < 3 {
		t.Fatalf("%d pings in 500 ms", pings)
	}
	if len(nonPing) != 0 {
		t.Fatalf("sent after the hello: %q", nonPing)
	}
	if n := len(f.hellosFor(P1)); n != 1 {
		t.Fatalf("%d hellos while pongs came", n)
	}
	f.push(P1, 3, nil)
	eventually(t, "event 3", func() bool { return cursor(c, P1) == 3 })

	f.set(func() { f.silent = true })
	eventually(t, "a reconnect after the silence", func() bool { return len(f.hellosFor(P1)) >= 2 })
	f.set(func() { f.silent = false })
	hs := f.hellosFor(P1)
	if hs[1].Ticket == hs[0].Ticket {
		t.Fatal("the reconnect reused its ticket")
	}
	if hs[1].Cursor != 3 {
		t.Fatalf("reconnected from %d, want the cursor 3", hs[1].Cursor)
	}
	eventually(t, "live again", func() bool { return f.liveOn(P1) })
	f.push(P1, 4, nil)
	eventually(t, "event 4 after the reconnect", func() bool { return cursor(c, P1) == 4 })
}

// ------------------------------------------------------------------ reset

// A hello whose cursor is beyond the server's head (a restored server) is answered with reset: the client drops
// what it holds of the project, its cursor goes back to 0, and it catches up from 0; sessions keep their place but
// never beyond the head. Another project is untouched.
func TestResetDropsAndRefetches(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2, 3, 4)
	f.add(P2, 1)
	c := machine(t, f, P1, P2)
	must(t, c.St.Tx(func(tx *store.Tx) error {
		if err := tx.PutTopic(protocol.Topic{ID: "tp_00000000000000000000000099", Project: P1, Name: "Old"}); err != nil {
			return err
		}
		if err := tx.SetCursor(P1, 10); err != nil {
			return err
		}
		return tx.Seen("s1", 10, 1)
	}))
	start(t, c, fast(nil, nil))
	eventually(t, "caught up again from 0", func() bool { return cursor(c, P1) == 4 && cursor(c, P2) == 1 })
	hs := f.hellosFor(P1)
	if len(hs) != 2 || hs[0].Cursor != 10 || hs[1].Cursor != 0 {
		t.Fatalf("hellos %+v: want cursor 10, then 0", hs)
	}
	if got := topicNames(t, c, P1); !slices.Equal(got, []string{"T1", "T2", "T3", "T4"}) {
		t.Fatalf("topics after the reset %v", got)
	}
	if s, _, _ := c.St.Session("s1"); s.LastSeq > 4 {
		t.Fatalf("the session's place %d is beyond the head", s.LastSeq)
	}
	if hs := f.hellosFor(P2); len(hs) != 1 {
		t.Fatalf("the other project reconnected: %+v", hs)
	}
}

// ------------------------------------------------------------------ close reasons

// REPLACED: a newer socket of this member took the place: stop, never reconnect (the key stays). The other project
// stays live and its events still land.
func TestReplacedStopsNoReconnect(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	f.add(P2, 1)
	f.set(func() { f.closeWith[P1] = []string{"REPLACED"} })
	c := machine(t, f, P1, P2)
	l := &logs{}
	start(t, c, fast(l, nil))
	eventually(t, "the other project live", func() bool { return f.liveOn(P2) && len(f.hellosFor(P1)) == 1 })
	time.Sleep(400 * time.Millisecond)
	if n := len(f.hellosFor(P1)); n != 1 {
		t.Fatalf("reconnected %d times after REPLACED", n-1)
	}
	if !hasKey(c, P1) {
		t.Fatal("REPLACED dropped the key")
	}
	if c.St.LiveAt(P1).After(time.Now().Add(-core.LiveFresh)) {
		t.Fatal("a replaced project still looks live to hooks")
	}
	f.push(P2, 2, nil)
	eventually(t, "the other project's event", func() bool { return cursor(c, P2) == 2 })

	// Replaced while live: the same.
	f.closeLive(P2, "REPLACED")
	time.Sleep(300 * time.Millisecond)
	if n := len(f.hellosFor(P2)); n != 1 {
		t.Fatalf("reconnected after a live REPLACED: %d hellos", n)
	}
	if !strings.Contains(l.String(), "REPLACED") {
		t.Fatalf("not logged: %s", l)
	}
}

// NOT_A_MEMBER, from the socket or from live.ticket: the key is dropped and the socket is never opened again. The
// outbox items queued for the project stay; the key fetch for them is refused once and not asked again on every
// round (no reconnect storm), and the sync keeps running.
func TestNotAMemberDropsKey(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	f.add(P2, 1)
	f.set(func() {
		f.closeWith[P1] = []string{"NOT_A_MEMBER"}
		f.ticketErr[P2] = "NOT_A_MEMBER"
		f.keyFetch = "NOT_A_MEMBER"
	})
	c := machine(t, f, P1, P2)
	wait, _ := start(t, c, fast(nil, nil))
	eventually(t, "both keys dropped", func() bool { return !hasKey(c, P1) && !hasKey(c, P2) })
	queue(t, c, P1, 0, 2) // queued after: there is no key to send them with
	time.Sleep(400 * time.Millisecond)
	if n := len(f.hellosFor(P1)); n != 1 {
		t.Fatalf("%d hellos after NOT_A_MEMBER", n)
	}
	if n := f.count("live.ticket"); n != 2 {
		t.Fatalf("%d tickets asked: a reconnect storm", n)
	}
	if n := f.count("key.fetch"); n > 1 {
		t.Fatalf("the refused key was asked for %d times in 400 ms", n)
	}
	if got := queued(t, c, P1); len(got) != 2 {
		t.Fatalf("the outbox items were lost: %v", got)
	}
	select {
	case err := <-waitCh(wait):
		t.Fatalf("the sync stopped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitCh(wait func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- wait() }()
	return ch
}

// TOO_SLOW: reconnect at once (with backoff set to an hour, only an immediate reconnect can happen), from the cursor.
func TestTooSlowReconnectsAtOnce(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2)
	c := machine(t, f, P1)
	o := fast(nil, nil)
	o.Backoff = []time.Duration{time.Hour}
	start(t, c, o)
	eventually(t, "live", func() bool { return f.liveOn(P1) && cursor(c, P1) == 2 })
	f.closeLive(P1, "TOO_SLOW")
	eventually(t, "reconnected", func() bool { return len(f.hellosFor(P1)) == 2 && f.liveOn(P1) })
	if hs := f.hellosFor(P1); hs[1].Cursor != 2 || hs[1].At.Sub(hs[0].At) > 2*time.Second {
		t.Fatalf("reconnect %+v", hs)
	}
	f.push(P1, 3, nil)
	eventually(t, "event 3", func() bool { return cursor(c, P1) == 3 })
}

// Every close reason of live.py, and a socket dropped with no LiveClosed: which reconnect, how soon, and which drop
// the key. NOT_ALLOWED (an expired ticket, a disabled account), SCHEMA_VIOLATION and a reason this client does not
// know are a failed connect: a new ticket after the backoff.
func TestCloseReasons(t *testing.T) {
	cases := []struct {
		reason    string
		reconnect bool
		atOnce    bool
		dropKey   bool
	}{
		{"NOT_ALLOWED", true, false, false},
		{"SCHEMA_VIOLATION", true, false, false},
		{"FUTURE_REASON", true, false, false},
		{"drop", true, false, false},
		{"TOO_SLOW", true, true, false},
		{"REPLACED", false, false, false},
		{"NOT_A_MEMBER", false, false, true},
	}
	backoff := 300 * time.Millisecond
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			f := newFake(t)
			f.add(P1, 1)
			f.set(func() { f.closeWith[P1] = []string{tc.reason} })
			c := machine(t, f, P1)
			o := fast(nil, nil)
			o.Backoff = []time.Duration{backoff}
			start(t, c, o)
			if !tc.reconnect {
				eventually(t, "the hello", func() bool { return len(f.hellosFor(P1)) == 1 })
				time.Sleep(backoff + 200*time.Millisecond)
				if n := len(f.hellosFor(P1)); n != 1 {
					t.Fatalf("%d hellos", n)
				}
				if hasKey(c, P1) == tc.dropKey {
					t.Fatalf("key kept %v, want dropped %v", hasKey(c, P1), tc.dropKey)
				}
				return
			}
			eventually(t, "reconnected and live", func() bool { return len(f.hellosFor(P1)) == 2 && f.liveOn(P1) })
			hs := f.hellosFor(P1)
			gap := hs[1].At.Sub(hs[0].At)
			if tc.atOnce && gap >= backoff {
				t.Fatalf("waited %v", gap)
			}
			if !tc.atOnce && gap < backoff {
				t.Fatalf("reconnected after %v, before the backoff", gap)
			}
			if hs[1].Ticket == hs[0].Ticket || !hasKey(c, P1) {
				t.Fatal("no fresh ticket, or the key went")
			}
			eventually(t, "caught up", func() bool { return cursor(c, P1) == 1 })
		})
	}
}

// ------------------------------------------------------------------ seqs

// Seqs may have holes (a deleted topic deletes its events): every event is applied, the cursor is the last seq,
// nothing reconnects or waits for the missing ones.
func TestHolesInSeqs(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2, 5, 9)
	c := machine(t, f, P1)
	a := &applied{}
	start(t, c, fast(nil, a))
	eventually(t, "cursor 9", func() bool { return cursor(c, P1) == 9 })
	f.push(P1, 12, nil)
	eventually(t, "cursor 12", func() bool { return cursor(c, P1) == 12 })
	if got := a.of(P1); !slices.Equal(got, []int64{1, 2, 5, 9, 12}) {
		t.Fatalf("applied %v", got)
	}
	if n := len(f.hellosFor(P1)); n != 1 {
		t.Fatalf("%d hellos over holes", n)
	}
	f.kick(P1)
	eventually(t, "reconnected", func() bool { return len(f.hellosFor(P1)) == 2 })
	if hs := f.hellosFor(P1); hs[1].Cursor != 12 {
		t.Fatalf("reconnected from %d", hs[1].Cursor)
	}
}

// ------------------------------------------------------------------ reconnects

// Network drops mid-stream: each reconnect says the last seq applied, so the server resends from there; every
// event is applied exactly once and in order. An event the server sends again (seq at or below the cursor) is not
// applied twice, nor is one a pull of a command brought in first.
func TestReconnectNoGapsNoDuplicates(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2, 3, 4, 5)
	c := machine(t, f, P1)
	a := &applied{}
	start(t, c, fast(nil, a))
	eventually(t, "cursor 5", func() bool { return cursor(c, P1) == 5 && f.liveOn(P1) })
	for seq := int64(6); seq <= 30; seq++ {
		if seq%5 == 0 {
			f.kick(P1)
			f.add(P1, seq) // committed while the socket was down
			eventually(t, "back live", func() bool { return f.liveOn(P1) })
			continue
		}
		f.push(P1, seq, nil)
	}
	eventually(t, "cursor 30", func() bool { return cursor(c, P1) == 30 })
	f.sendRaw(P1, fmt.Sprintf(`{"type": "events", "events": [%s], "cursor": 3}`, topicEvent(P1, 3))) // again
	f.add(P1, 31, 32)
	c.Sync(context.Background(), core.HookTimeout, P1) // a command pulls 31 and 32 before the socket sends them
	f.push(P1, 33, nil)
	f.sendRaw(P1, fmt.Sprintf(`{"type": "events", "events": [%s, %s], "cursor": 32}`, topicEvent(P1, 31), topicEvent(P1, 32)))
	eventually(t, "cursor 33", func() bool { return cursor(c, P1) == 33 })
	time.Sleep(50 * time.Millisecond)
	got := a.of(P1)
	want := append(seqRange(1, 30), 33)
	if !slices.Equal(got, want) {
		t.Fatalf("applied %v\nwant    %v", got, want)
	}
	hs := f.hellosFor(P1)
	for i := 1; i < len(hs); i++ {
		if hs[i].Cursor < hs[i-1].Cursor {
			t.Fatalf("a reconnect went back: %+v", hs)
		}
	}
}

// A crash (SIGKILL) and a restart: the new process reconnects from the stored cursor; nothing is missed, nothing
// applied twice.
func TestCrashAndRestartResumesFromTheCursor(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2, 3, 4, 5)
	c := machine(t, f, P1)
	cmd, _ := helper(t, "run")
	eventually(t, "the first process caught up", func() bool { return cursor(c, P1) == 5 && f.liveOn(P1) })
	f.push(P1, 6, nil)
	eventually(t, "event 6", func() bool { return cursor(c, P1) == 6 })
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	f.add(P1, 7, 8) // committed while nothing ran
	c2, err := core.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	a := &applied{}
	start(t, c2, fast(nil, a))
	eventually(t, "the restart caught up", func() bool { return cursor(c2, P1) == 8 })
	f.push(P1, 9, nil)
	eventually(t, "event 9", func() bool { return cursor(c2, P1) == 9 })
	if got := a.of(P1); !slices.Equal(got, []int64{7, 8, 9}) {
		t.Fatalf("the restart applied %v", got)
	}
	if hs := f.hellosFor(P1); hs[len(hs)-1].Cursor != 6 {
		t.Fatalf("the restart said cursor %d", hs[len(hs)-1].Cursor)
	}
	if got := topicNames(t, c2, P1); len(got) != 9 {
		t.Fatalf("topics %v", got)
	}
}

// ------------------------------------------------------------------ fallback

// The WebSocket refused (a proxy, a server with live sync off): after 3 failed connects the sync falls back to
// sync.poll with wait 25 from its cursor, and tries the socket again every SocketRetry; once it opens, polling
// stops. A reset on the poll path drops the project and pulls it from 0.
func TestFallsBackToPoll(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2)
	f.set(func() { f.refuseWS = true })
	c := machine(t, f, P1)
	start(t, c, fast(nil, nil))
	eventually(t, "a poll", func() bool { return f.count("sync.poll") > 0 })
	f.mu.Lock()
	tries, first := f.urlsAtFirstPoll, f.polls[0]
	f.mu.Unlock()
	if tries != 3 {
		t.Fatalf("polled after %d socket tries, want 3", tries)
	}
	if first.Wait != 25 || first.Cursors[P1] != 0 {
		t.Fatalf("poll %+v", first)
	}
	eventually(t, "events by poll", func() bool { return cursor(c, P1) == 2 })
	f.push(P1, 3, nil)
	eventually(t, "a new event by poll", func() bool { return cursor(c, P1) == 3 })
	time.Sleep(400 * time.Millisecond)
	f.mu.Lock()
	retries := len(f.urls)
	f.mu.Unlock()
	if retries <= tries {
		t.Fatal("the socket was not tried again")
	}

	f.set(func() { f.refuseWS = false })
	eventually(t, "live again", func() bool { return f.liveOn(P1) })
	if hs := f.hellosFor(P1); hs[0].Cursor != 3 {
		t.Fatalf("the socket opened from %d", hs[0].Cursor)
	}
	time.Sleep(100 * time.Millisecond)
	polls := f.count("sync.poll")
	time.Sleep(300 * time.Millisecond)
	if n := f.count("sync.poll"); n != polls {
		t.Fatalf("still polling while live: %d more", n-polls)
	}
	f.push(P1, 4, nil)
	eventually(t, "event 4 live", func() bool { return cursor(c, P1) == 4 })
}

func TestPollResetDropsAndRefetches(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2)
	f.set(func() { f.refuseWS = true })
	c := machine(t, f, P1)
	must(t, c.St.Tx(func(tx *store.Tx) error {
		if err := tx.PutTopic(protocol.Topic{ID: "tp_00000000000000000000000099", Project: P1, Name: "Old"}); err != nil {
			return err
		}
		return tx.SetCursor(P1, 10)
	}))
	start(t, c, fast(nil, nil))
	eventually(t, "pulled again from 0", func() bool { return cursor(c, P1) == 2 })
	if got := topicNames(t, c, P1); !slices.Equal(got, []string{"T1", "T2"}) {
		t.Fatalf("topics %v", got)
	}
}

// A live.ticket the server can't be reached for (or refuses for a reason that is not the membership) is a failed
// connect: backoff, then the fallback; nothing is dropped.
func TestTicketRefusedIsAFailedConnect(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	f.set(func() { f.ticketErr[P1] = "UNKNOWN_PROJECT" })
	c := machine(t, f, P1)
	start(t, c, fast(nil, nil))
	eventually(t, "the fallback", func() bool { return f.count("sync.poll") > 0 && cursor(c, P1) == 1 })
	f.mu.Lock()
	n := f.ticketsAtFirstPoll
	f.mu.Unlock()
	if n != 3 {
		t.Fatalf("%d tickets before the fallback", n)
	}
	if !hasKey(c, P1) {
		t.Fatal("the key was dropped")
	}
	f.set(func() { f.ticketErr[P1] = "" })
	eventually(t, "live", func() bool { return f.liveOn(P1) })
}

// ------------------------------------------------------------------ one copy

// One copy per machine: a second start returns ErrRunning at once and the first keeps running; a copy in another
// process holds the lock too, Status names it, Stop ends it, and then a new copy starts.
func TestTwoStartsOneRuns(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	c := machine(t, f, P1)
	_, stop := start(t, c, fast(nil, nil))
	eventually(t, "live", func() bool { return f.liveOn(P1) })
	c2, err := core.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	began := time.Now()
	if err := Run(context.Background(), c2, fast(nil, nil)); !errors.Is(err, ErrRunning) {
		t.Fatalf("the second copy: %v", err)
	}
	if time.Since(began) > time.Second {
		t.Fatal("the second copy waited")
	}
	f.push(P1, 2, nil)
	eventually(t, "the first copy still runs", func() bool { return cursor(c, P1) == 2 })
	if pid, ok := Status(c.Dir); !ok || pid != os.Getpid() {
		t.Fatalf("status %d %v", pid, ok)
	}
	stop()
	eventually(t, "the lock let go", func() bool { _, ok := Status(c.Dir); return !ok })

	cmd, r := helper(t, "run")
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "running") {
		t.Fatalf("helper: %q", line)
	}
	if err := Run(context.Background(), c2, fast(nil, nil)); !errors.Is(err, ErrRunning) {
		t.Fatalf("beside another process: %v", err)
	}
	if pid, ok := Status(c.Dir); !ok || pid != cmd.Process.Pid {
		t.Fatalf("status %d %v, want %d", pid, ok, cmd.Process.Pid)
	}
	stopped, err := Stop(c.Dir, 5*time.Second)
	if err != nil || !stopped {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	if _, ok := Status(c.Dir); ok {
		t.Fatal("still running after stop")
	}
	if stopped, err := Stop(c.Dir, time.Second); err != nil || stopped {
		t.Fatalf("stopping what is not running: %v %v", stopped, err)
	}
	_, _ = start(t, c2, fast(nil, nil))
	eventually(t, "a new copy", func() bool { pid, ok := Status(c.Dir); return ok && pid == os.Getpid() })
	if fi, err := os.Stat(filepath.Join(c.Dir, LockFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file %v %v", fi, err)
	}
}

// ------------------------------------------------------------------ exit

// The sync exits (nil, the lock released, sockets closed) after IdleExit with no active session: none seen within
// it, and no agent process of a recent session alive. Signed out while running: it exits cleanly.
func TestIdleExit(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		f := newFake(t)
		c := machine(t, f)
		must(t, c.St.Tx(func(tx *store.Tx) error {
			_, err := tx.UpsertSession("s1", "agent", 0, "", time.Now().Add(-time.Hour))
			return err
		}))
		o := fast(nil, nil)
		o.IdleExit = 200 * time.Millisecond
		wait, _ := start(t, c, o)
		if err := wait(); err != nil {
			t.Fatal(err)
		}
		if _, ok := Status(c.Dir); ok {
			t.Fatal("the lock is still held")
		}
	})
	t.Run("a session seen lately, then quiet", func(t *testing.T) {
		f := newFake(t)
		f.add(P1, 1)
		c := machine(t, f, P1)
		o := fast(nil, nil)
		o.IdleExit = 600 * time.Millisecond
		began := time.Now()
		wait, _ := start(t, c, o)
		eventually(t, "live", func() bool { return f.liveOn(P1) })
		if err := wait(); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(began); d < 500*time.Millisecond {
			t.Fatalf("exited after %v while a session was active", d)
		}
		eventually(t, "the socket closed", func() bool { return !f.liveOn(P1) })
	})
	t.Run("an agent process alive", func(t *testing.T) {
		f := newFake(t)
		c := machine(t, f)
		must(t, c.St.Tx(func(tx *store.Tx) error {
			_, err := tx.UpsertSession("s1", "agent", os.Getpid(), "", time.Now())
			return err
		}))
		o := fast(nil, nil)
		o.IdleExit = 100 * time.Millisecond
		wait, stop := start(t, c, o)
		select {
		case err := <-waitCh(wait):
			t.Fatalf("exited with the agent alive: %v", err)
		case <-time.After(600 * time.Millisecond):
		}
		stop()
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a dead agent process", func(t *testing.T) {
		f := newFake(t)
		c := machine(t, f)
		must(t, c.St.Tx(func(tx *store.Tx) error {
			_, err := tx.UpsertSession("s1", "agent", 999999, "", time.Now().Add(-time.Minute))
			return err
		}))
		o := fast(nil, nil)
		o.IdleExit = 100 * time.Millisecond
		wait, _ := start(t, c, o)
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a session seen ahead of now (clock set back)", func(t *testing.T) {
		f := newFake(t)
		c := machine(t, f)
		must(t, c.St.Tx(func(tx *store.Tx) error {
			_, err := tx.UpsertSession("s1", "agent", 0, "", time.Now().Add(time.Hour))
			return err
		}))
		o := fast(nil, nil)
		o.IdleExit = 100 * time.Millisecond
		wait, stop := start(t, c, o)
		select {
		case err := <-waitCh(wait):
			t.Fatalf("exited with a session seen ahead of now: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		stop()
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a reused agent pid", func(t *testing.T) {
		f := newFake(t)
		c := machine(t, f)
		// This process is alive, but started after the session was last seen: another process now has the pid.
		must(t, c.St.Tx(func(tx *store.Tx) error {
			_, err := tx.UpsertSession("s1", "agent", os.Getpid(), "", time.Now().Add(-23*time.Hour))
			return err
		}))
		o := fast(nil, nil)
		o.IdleExit = 100 * time.Millisecond
		wait, _ := start(t, c, o)
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("signed out while running", func(t *testing.T) {
		f := newFake(t)
		f.add(P1, 1)
		c := machine(t, f, P1)
		wait, _ := start(t, c, fast(nil, nil))
		eventually(t, "live", func() bool { return f.liveOn(P1) })
		if err := c.Logout(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := wait(); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the socket closed", func() bool { return !f.liveOn(P1) })
	})
	t.Run("not signed in", func(t *testing.T) {
		f := newFake(t)
		c := machine(t, f)
		must(t, config.Save(c.Dir, config.Config{Server: f.URL()}))
		wait, _ := start(t, c, fast(nil, nil))
		if err := wait(); err != nil {
			t.Fatal(err)
		}
	})
}

// ------------------------------------------------------------------ secrets

// The ticket goes only in the hello: never in a URL, a header or the log, even when the server echoes it in a
// LiveClosed. No key either: the socket carries no Authorization.
func TestTicketNeverLogged(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	f.set(func() { f.closeWith[P1] = []string{"SCHEMA_VIOLATION", "NOT_ALLOWED"} })
	c := machine(t, f, P1)
	l := &logs{}
	start(t, c, fast(l, nil))
	eventually(t, "live after two refusals", func() bool { return f.liveOn(P1) && len(f.hellosFor(P1)) == 3 })
	f.closeLive(P1, "TOO_SLOW")
	eventually(t, "live again", func() bool { return f.liveOn(P1) })
	f.mu.Lock()
	var secrets []string
	for tk := range f.tickets {
		secrets = append(secrets, tk, strings.TrimPrefix(tk, "tkt"))
	}
	urls, headers := append([]string(nil), f.urls...), toHeaders(f.headers)
	f.mu.Unlock()
	secrets = append(secrets, keyOf(P1), authKey, "SECRETTICKETVALUE", "PROJECTSECRETVALUE", "AUTHSECRETVALUE")
	text := l.String()
	if !strings.Contains(text, "SCHEMA_VIOLATION") || !strings.Contains(text, "NOT_ALLOWED") {
		t.Fatalf("the refusals were not logged: %s", text)
	}
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Fatalf("logged a secret: %s", text)
		}
		for _, u := range urls {
			if strings.Contains(u, s) {
				t.Fatalf("a secret in the URL %s", u)
			}
		}
	}
	for i, u := range urls {
		if !strings.HasPrefix(u, protocol.LivePath+"?") || !strings.Contains(u, "project="+P1) || !strings.Contains(u, "org=og_") {
			t.Fatalf("url %s", u)
		}
		if headers[i]["Authorization"] != "" || !strings.HasPrefix(headers[i][protocol.ClientHeader], "teamflow-go/") {
			t.Fatalf("headers %v", headers[i])
		}
	}
}

func toHeaders(hs []http.Header) []map[string]string {
	var out []map[string]string
	for _, h := range hs {
		m := map[string]string{}
		for k, v := range h {
			m[k] = strings.Join(v, ",")
		}
		out = append(out, m)
	}
	return out
}

// ------------------------------------------------------------------ hooks

// While the background sync is live on a project (heard within LiveFresh), hooks and commands answer from the
// replica: no sync.poll. The outbox still goes when something waits in it.
func TestHookSkipsNetworkWhenFresh(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.add(P1, 1)
	c := machine(t, f, P1)
	must(t, c.St.Seen("s1", 1, 1))
	must(t, c.St.SetLiveAt(P1, time.Now().Add(-5*time.Second)))
	if _, err := c.Prompt(ctx, "s1", "agent", 0, "", "hello"); err != nil {
		t.Fatal(err)
	}
	c.MidTurn(ctx, "s1")
	if online := c.Sync(ctx, core.HookTimeout, P1); !online {
		t.Fatal("offline")
	}
	c.Sync(ctx, core.HookTimeout, "")
	if n := f.count("sync.poll"); n != 0 {
		t.Fatalf("%d polls while live", n)
	}
	queue(t, c, P1, 0, 1)
	c.Sync(ctx, core.HookTimeout, P1)
	if n := f.count("signal.emit"); n != 1 || len(queued(t, c, P1)) != 0 {
		t.Fatalf("the outbox was not sent while live: %d", n)
	}
}

// When the background sync is not live (never heard from, heard more than LiveFresh ago, or a time ahead of now
// after the clock was set back), the hook pulls within HOOK_TIMEOUT.
func TestHookPullsWhenStale(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"never", time.Time{}},
		{"stale", time.Now().Add(-core.LiveFresh - time.Second)},
		{"clock set back", time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.add(P1, 1, 2)
			c := machine(t, f, P1)
			must(t, c.St.Seen("s1", 1, 1))
			if !tc.at.IsZero() {
				must(t, c.St.SetLiveAt(P1, tc.at))
			}
			if _, err := c.Prompt(ctx, "s1", "agent", 0, "", "hello"); err != nil {
				t.Fatal(err)
			}
			if n := f.count("sync.poll"); n == 0 || cursor(c, P1) != 2 {
				t.Fatalf("%d polls, cursor %d", n, cursor(c, P1))
			}
		})
	}
}

// ------------------------------------------------------------------ the outbox

// The background sync sends the outbox only through Core.Sync, under the sender lock: with a command flushing the
// same outbox at the same time (two real processes), never two requests in flight, every signal once, in order,
// and both really send. The background copy is held on its first request until the command is waiting on the
// lock (contention); its second request fails (503), so it lets go and the command sends the rest.
func TestBackgroundSyncAndACommandNeverInterleave(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	first := make(chan string, 1)
	release := make(chan struct{})
	f.set(func() {
		f.emitDelay = 20 * time.Millisecond
		f.emitHook = func(n int, client string) bool {
			switch n {
			case 1:
				first <- client
				<-release
			case 2:
				return strings.Contains(client, "/bg ")
			}
			return false
		}
	})
	c := machine(t, f, P1)
	ids := queue(t, c, P1, 0, 2*protocol.MaxBatch+10)
	bg, _ := helper(t, "run")
	select {
	case who := <-first:
		if !strings.Contains(who, "/bg ") {
			t.Fatalf("the first request came from %q", who)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the background sync never sent")
	}
	cmd, r := helper(t, "sync")
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "syncing") {
		t.Fatalf("command: %q", line)
	}
	time.Sleep(300 * time.Millisecond) // the command now waits on the lock the background copy holds
	close(release)
	_ = cmd.Wait()
	eventually(t, "the outbox sent", func() bool { return len(queued(t, c, P1)) == 0 })
	_ = bg.Process.Signal(syscall.SIGTERM)
	_ = bg.Wait()
	f.mu.Lock()
	order, clients := append([]string(nil), f.order...), append([]string(nil), f.clients...)
	f.mu.Unlock()
	if m := f.maxIn.Load(); m != 1 {
		t.Fatalf("%d outbox requests in flight at once", m)
	}
	if !slices.Equal(order, ids) {
		t.Fatalf("sent %d, want %d once each in order", len(order), len(ids))
	}
	var bgN, cmdN int
	for _, cl := range clients {
		switch {
		case strings.Contains(cl, "/bg "):
			bgN++
		case strings.Contains(cl, "/cmd "):
			cmdN++
		}
	}
	if bgN == 0 || cmdN == 0 {
		t.Fatalf("requests: background %d, command %d (%q)", bgN, cmdN, clients)
	}
}

// The background sync alone sends what waits in the outbox (a hook or command left it queued: offline then).
func TestBackgroundSyncSendsTheOutbox(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	c := machine(t, f, P1)
	ids := queue(t, c, P1, 0, 3)
	start(t, c, fast(nil, nil))
	eventually(t, "sent", func() bool { return len(queued(t, c, P1)) == 0 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.order, ids) {
		t.Fatalf("sent %v", f.order)
	}
}

// ------------------------------------------------------------------ review fixes

// A server with live sync off refuses live.ticket NOT_ALLOWED ("use sync.poll"): every project falls back to
// sync.poll and keeps its key. Only sync.poll's own refusal of the key drops it.
func TestLiveSyncOffFallsBackToPollAndKeepsKeys(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1, 2)
	f.add(P2, 1)
	f.set(func() { f.liveOff = true })
	c := machine(t, f, P1, P2)
	l := &logs{}
	start(t, c, fast(l, nil))
	eventually(t, "both projects by poll", func() bool { return cursor(c, P1) == 2 && cursor(c, P2) == 1 })
	f.push(P2, 2, nil)
	eventually(t, "a new event by poll", func() bool { return cursor(c, P2) == 2 })
	time.Sleep(400 * time.Millisecond) // a socket retry or two
	if !hasKey(c, P1) || !hasKey(c, P2) {
		t.Fatal("a key was dropped for live sync being off")
	}
	if len(f.helloList()) != 0 {
		t.Fatal("a socket opened without a ticket")
	}
	if !strings.Contains(l.String(), "use sync.poll") {
		t.Fatalf("not logged: %s", l)
	}
	f.set(func() { f.pollErr[P2] = "NOT_ALLOWED" })
	eventually(t, "sync.poll's refusal drops the key", func() bool { return !hasKey(c, P2) })
	if !hasKey(c, P1) {
		t.Fatal("the other project's key went too")
	}
}

// A half-open socket (pings go out, nothing comes back) never looks fresh past what was last heard: live_at stops
// moving, so a hook pulls again once LiveFresh has passed since then, however long the silence limit.
func TestHalfOpenSocketGoesStale(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	c := machine(t, f, P1)
	must(t, c.St.Seen("s1", 1, 1))
	o := fast(nil, nil)
	o.Silence = time.Hour // never found dead here: only live_at protects the hooks
	start(t, c, o)
	eventually(t, "live", func() bool { return f.liveOn(P1) })
	time.Sleep(200 * time.Millisecond) // pongs heard: live_at moves
	f.set(func() { f.silent = true })
	time.Sleep(150 * time.Millisecond)
	heard := c.St.LiveAt(P1)
	time.Sleep(400 * time.Millisecond) // several pings, no answer
	f.mu.Lock()
	pings := f.pings
	f.mu.Unlock()
	if at := c.St.LiveAt(P1); !at.Equal(heard) || heard.IsZero() || pings < 5 {
		t.Fatalf("live_at moved with nothing heard: %v then %v (%d pings)", heard, at, pings)
	}
	hook, err := core.Open()
	must(t, err)
	defer hook.Close()
	hook.Now = func() time.Time { return heard.Add(core.LiveFresh + time.Second) }
	polls := f.count("sync.poll")
	if _, err := hook.Prompt(context.Background(), "s1", "agent", 0, "", "hi"); err != nil {
		t.Fatal(err)
	}
	if f.count("sync.poll") == polls {
		t.Fatal("the hook trusted a socket not heard from for LiveFresh")
	}
}

// With the default timing a dead socket is found within LiveFresh: a ping every 10 s, 5 s of silence.
func TestDefaultsFindADeadSocketWithinLiveFresh(t *testing.T) {
	o := Options{}.withDefaults()
	if o.PingEvery != 10*time.Second || o.Silence != 5*time.Second || o.PingEvery+o.Silence > core.LiveFresh {
		t.Fatalf("ping %v, silence %v, LiveFresh %v", o.PingEvery, o.Silence, core.LiveFresh)
	}
}

// However a socket ends (dropped by the network, refused, replaced, gone silent), live_at is cleared at once: the
// next hook pulls.
func TestSocketEndClearsLiveAt(t *testing.T) {
	for _, end := range []string{"drop", "NOT_ALLOWED", "REPLACED", "silent"} {
		t.Run(end, func(t *testing.T) {
			f := newFake(t)
			f.add(P1, 1)
			c := machine(t, f, P1)
			o := fast(nil, nil)
			o.Backoff = []time.Duration{time.Hour} // nothing reconnects to set it again
			start(t, c, o)
			eventually(t, "live", func() bool { return f.liveOn(P1) && !c.St.LiveAt(P1).IsZero() })
			switch end {
			case "drop":
				f.kick(P1)
			case "silent":
				f.set(func() { f.silent = true; f.closeWith[P1] = []string{"drop"} }) // the reconnect fails
			default:
				f.closeLive(P1, end)
			}
			eventually(t, "live_at cleared", func() bool { return c.St.LiveAt(P1).IsZero() })
			if c.LiveFresh(P1) {
				t.Fatal("still fresh")
			}
		})
	}
}

// A --status or --stop probe holding the lock for a moment never makes a starting copy give up.
func TestStartingBesideAStatusProbe(t *testing.T) {
	f := newFake(t)
	c := machine(t, f)
	fh, err := os.OpenFile(filepath.Join(c.Dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	must(t, err)
	defer fh.Close()
	fd := int(fh.Fd())
	must(t, unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB))
	unlocked := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); _ = unix.Flock(fd, unix.LOCK_UN); close(unlocked) }()
	defer func() { <-unlocked }()
	wait, _ := start(t, c, fast(nil, nil))
	eventually(t, "running", func() bool { pid, ok := Status(c.Dir); return ok && pid == os.Getpid() })
	select {
	case err := <-waitCh(wait):
		t.Fatalf("the starting copy gave up: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// The auth key expiring while the sync runs: sockets on project keys go on; a project whose items wait without a
// key asks for one once (refused), not on every round; nothing is lost, nothing crashes.
func TestExpiredAuthKeyWhileRunning(t *testing.T) {
	f := newFake(t)
	f.add(P1, 1)
	f.set(func() { f.keyFetch = "NOT_ALLOWED" })
	c := machine(t, f, P1)
	wait, _ := start(t, c, fast(nil, nil))
	eventually(t, "live", func() bool { return f.liveOn(P1) })
	ids := queue(t, c, P2, 0, 2) // queued for a project with no key on this machine
	time.Sleep(400 * time.Millisecond)
	if n := f.count("key.fetch"); n != 1 {
		t.Fatalf("the refused key fetched %d times", n)
	}
	if got := queued(t, c, P2); !slices.Equal(got, ids) {
		t.Fatalf("queued %v", got)
	}
	f.push(P1, 2, nil)
	eventually(t, "the socket goes on", func() bool { return cursor(c, P1) == 2 })
	select {
	case err := <-waitCh(wait):
		t.Fatalf("the sync stopped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

// ------------------------------------------------------------------ helpers

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// queue puts n signals in the project's outbox, as an offline session left them (ids sg_q000…).
func queue(t *testing.T, c *core.Core, project string, from, n int) []string {
	t.Helper()
	var ids []string
	for i := from; i < from+n; i++ {
		e := protocol.NewSignalEmit()
		e.ClientSession, e.Actor, e.ProjectVersion = "s1", protocol.ActorAgent, 1
		e.Signal = protocol.NewSignal()
		e.Signal.ID, e.Signal.Project, e.Signal.Topic, e.Signal.Kind, e.Signal.Title = fmt.Sprintf("sg_q%03d", i), project,
			"tp_00000000000000000000000001", protocol.SignalKindNote, "n"
		e.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "why"}
		b, _ := json.Marshal(e)
		must(t, c.St.SpoolAdd(e.Signal.ID, "s1", store.OutboxOf(binding(c), project), string(b), time.Now()))
		ids = append(ids, e.Signal.ID)
	}
	return ids
}

func queued(t *testing.T, c *core.Core, project string) []string {
	t.Helper()
	items, err := c.St.Spooled(store.OutboxOf(binding(c), project))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Signal)
	}
	return out
}

// The helper process: TEST_LIVE_MODE=run runs the background sync (fast timing) until SIGTERM or SIGKILL; sync
// syncs once as a command does.
func TestMain(m *testing.M) {
	switch os.Getenv("TEST_LIVE_MODE") {
	case "run":
		api.Version = "bg" // the server tells the two senders apart by their Teamflow-Client
		c, err := core.Open()
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		fmt.Println("running")
		o := fast(nil, nil)
		o.Backoff = []time.Duration{300 * time.Millisecond} // an outbox left short waits: a command gets its turn
		if err := Run(ctx, c, o); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		os.Exit(0)
	case "sync":
		api.Version = "cmd"
		core.SyncLockWait = 5 * time.Second // waits for the background copy's lock, then sends
		c, err := core.Open()
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println("syncing")
		c.Sync(context.Background(), core.DefaultTimeout, "")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(t *testing.T, mode string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TEST_LIVE_MODE="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, bufio.NewReader(out)
}
