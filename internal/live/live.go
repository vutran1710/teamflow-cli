// Package live is the background sync (`teamflow sync`): one copy per machine holds a live socket per project
// this sign-in has a key for (models/tf_models/live.py), writes each event into the replica as it commits, and
// sends what waits in the outbox, so hooks and commands answer from the replica without waiting on the network.
//
// Per project: a ticket from live.ticket (the project key goes only in its Authorization header), the socket at
// wss://<server>/live?org=&project= (ids only; ws:// only on loopback), the ticket and cursor as the first
// message (LiveHello), then each LiveEvents applied in one transaction with its cursor and the project's live_at.
// The first answer means the socket is live. The live rules:
//
//   - a ping every LIVE_PING_SECONDS (10 s) once live; nothing heard within LIVE_PONG_SECONDS (5 s) of a ping: the
//     socket is dead, and a new one opens at once with a fresh ticket from the cursor (after 3 such in a row, the
//     backoff and the fallback, as for failed connects). live_at moves only when something is heard (a
//     pong or a message) and is cleared when a socket ends, so hooks trust a dead socket for at most
//     PingEvery + Silence (15 s, LiveFresh) and pull again at once after a socket closes;
//   - reset (the cursor is beyond a restored server's head): the project's replica is dropped and caught up from 0;
//   - REPLACED: stop, never reconnect; NOT_A_MEMBER (from the socket or live.ticket): the key is dropped and the
//     socket stays closed; TOO_SLOW: reconnect at once; any other refusal or a failed connect (live.ticket refused
//     NOT_ALLOWED too: a server with live sync off) reconnects after the backoff (1, 2, 3, 5, 8, 13 s); only
//     sync.poll's own refusal of the key drops it there;
//   - seq holes are allowed; an event at or below the cursor (another sync brought it first) is not applied again;
//   - after FailsBeforePoll failed connects in a row (WebSockets blocked), sync.poll with wait 25 s instead, trying
//     the socket again every SocketRetry.
//
// The outbox is sent only through core.Core.Sync, which takes the outbox's sender lock: the sync is one more
// sender, never sending beside a command or a hook. It exits after IdleExit with no active agent session, and at
// once when the machine is signed out. One copy per machine: the flock on sync.lock in the Teamflow home.
//
// Nothing secret is logged: a ticket or key in any text the sync logs (a server's message too) is masked.
package live

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/procs"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// LockFile is the one-copy lock in the Teamflow home; it holds the running copy's pid.
const LockFile = "sync.lock"

// ErrRunning is a start while another copy runs on this machine.
var ErrRunning = errors.New("the background sync is already running")

// Options tune the sync; a zero field takes the protocol's value.
type Options struct {
	IdleExit        time.Duration   // no active agent session this long: exit (10 min)
	PingEvery       time.Duration   // LIVE_PING_SECONDS (10 s)
	Silence         time.Duration   // LIVE_PONG_SECONDS (5 s): nothing heard this long after a ping, the socket is dead
	FirstAnswer     time.Duration   // a hello unanswered this long is a failed connect (LIVE_HELLO + LIVE_PONG)
	Backoff         []time.Duration // waits after failed connects: 1, 2, 3, 5, 8, 13 s (the last repeats)
	FailsBeforePoll int             // failed connects in a row before falling back to sync.poll (3)
	PollWait        time.Duration   // sync.poll's wait in the fallback (25 s)
	SocketRetry     time.Duration   // in the fallback, the socket is tried again this often (60 s)
	Healthy         time.Duration   // a socket live this long resets the failure and silence streaks (2 × (PingEvery + Silence))
	Rescan          time.Duration   // sign-in, keys, outbox and sessions are read again this often (2 s)
	Logf            func(format string, a ...any)
	Applied         func(project string, seq int64) // hears each event applied from the socket (tests)
	AutoUpdate      *AutoUpdate                     // the scheduled self-update (update.go); nil: none
}

// DefaultPingEvery and DefaultSilence: a socket that stops answering is found dead within their sum, which is
// core.LiveFresh: hooks never trust a dead socket longer than they would trust a live one.
const (
	DefaultPingEvery = protocol.LivePingSeconds * time.Second
	DefaultSilence   = protocol.LivePongSeconds * time.Second
)

// lockRetry: a start finding the lock held by status probes (shared) tries again for this long, every 2–8 ms
// (jittered, so it never keeps step with a probe polled at a fixed rate). A lock held exclusively is a running
// copy: the start gives up at once.
const lockRetry = 2 * time.Second

// writeTimeout bounds one ping's write.
const writeTimeout = 5 * time.Second

// DefaultIdleExit is how long the sync runs with no active agent session.
const DefaultIdleExit = 10 * time.Minute

// agentWindow: a session seen this recently whose agent process still lives keeps the sync running.
const agentWindow = 24 * time.Hour

// pidReuseSlack: an agent process that seems to have started after its session was last seen is another process.
const pidReuseSlack = 2 * time.Second

// readLimit bounds one live message (LIVE_MAX_EVENTS events).
const readLimit = 64 << 20

func (o Options) withDefaults() Options {
	sec := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.IdleExit, DefaultIdleExit)
	def(&o.PingEvery, DefaultPingEvery)
	def(&o.Silence, DefaultSilence)
	def(&o.FirstAnswer, sec(protocol.LiveHelloSeconds+protocol.LivePongSeconds))
	def(&o.PollWait, 25*time.Second)
	def(&o.SocketRetry, 60*time.Second)
	def(&o.Rescan, 2*time.Second)
	def(&o.Healthy, 2*(o.PingEvery+o.Silence))
	if len(o.Backoff) == 0 {
		o.Backoff = []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 8 * time.Second, 13 * time.Second}
	}
	if o.FailsBeforePoll <= 0 {
		o.FailsBeforePoll = 3
	}
	if o.Logf == nil {
		o.Logf = log.New(os.Stderr, "", log.LstdFlags).Printf
	}
	return o
}

func (o Options) backoff(i int) time.Duration {
	return o.Backoff[min(max(i, 0), len(o.Backoff)-1)]
}

// ------------------------------------------------------------------ one copy per machine

// lock takes the one-copy lock in dir (ErrRunning when another copy holds it) and writes this pid in it.
func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sync lock: %w", err)
	}
	_ = f.Chmod(0o600)
	deadline := time.Now().Add(lockRetry)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("sync lock: %w", err)
		}
		// Held, but by whom? Shared is only probes (Status, --stop): wait them out. Not even shared: a running copy.
		if serr := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); serr != nil {
			if errors.Is(serr, unix.EWOULDBLOCK) {
				_ = f.Close()
				return nil, ErrRunning
			}
		} else {
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrRunning
		}
		time.Sleep(time.Duration(2+rand.IntN(7)) * time.Millisecond)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = f.Truncate(0)
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

// Status says whether a copy runs on this machine (the lock in dir is held), and its pid (0 when unknown).
func Status(dir string) (pid int, running bool) {
	f, err := os.Open(filepath.Join(dir, LockFile))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err == nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return 0, false
	}
	for range 20 { // a copy just started writes its pid right after taking the lock
		b := make([]byte, 32)
		n, _ := f.ReadAt(b, 0)
		if p, err := strconv.Atoi(strings.TrimSpace(string(b[:n]))); err == nil && p > 0 {
			return p, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, true
}

// Stop asks the running copy to stop (SIGTERM) and waits up to wait for it to let go of the lock. False when none
// runs.
func Stop(dir string, wait time.Duration) (bool, error) {
	pid, ok := Status(dir)
	if !ok {
		return false, nil
	}
	if pid <= 1 {
		return false, errors.New("the background sync is running but its process is unknown")
	}
	if pid == os.Getpid() {
		return false, errors.New("the background sync runs in this process")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("stop the background sync: %w", err)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if _, ok := Status(dir); !ok {
			return true, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false, fmt.Errorf("the background sync (pid %d) did not stop within %v", pid, wait)
}

// ------------------------------------------------------------------ the run

// Run is the background sync on c's Teamflow home, until ctx ends, the machine is signed out, or IdleExit passes
// with no active agent session (all nil), or the scheduled self-update put a new binary in place (*Updated: the
// caller restarts the sync on it). ErrRunning when another copy runs. c's config and store are read, never
// changed: the sync reads the sign-in from disk again every Rescan.
func Run(ctx context.Context, c *core.Core, o Options) error {
	o = o.withDefaults()
	release, err := lock(c.Dir)
	if err != nil {
		return err
	}
	defer release()
	now := c.Now
	if now == nil {
		now = time.Now
	}
	r := &runner{o: o, dir: c.Dir, st: c.St, now: now, started: now(), workers: map[string]*worker{},
		stopped: map[string]string{}, flushAt: map[string]time.Time{}, flushStep: map[string]int{}, once: map[string]bool{}}
	if o.AutoUpdate != nil && o.AutoUpdate.Updater != nil {
		r.update = newUpdater(o.AutoUpdate)
	}
	defer r.stopAll()
	o.Logf("teamflow sync: started (pid %d)", os.Getpid())
	tick := time.NewTicker(o.Rescan)
	defer tick.Stop()
	for {
		if why := r.round(ctx); why != "" {
			o.Logf("teamflow sync: stopping: %s", why)
			return nil
		}
		if up := r.selfUpdate(ctx); up != nil {
			return up // a new binary is in place: the caller restarts the sync on it
		}
		select {
		case <-ctx.Done():
			o.Logf("teamflow sync: stopping")
			return nil
		case <-tick.C:
		}
	}
}

type runner struct {
	o       Options
	dir     string
	st      *store.Store
	now     func() time.Time
	started time.Time

	cfg     *config.Config
	binding store.KeyBinding
	workers map[string]*worker

	mu      sync.Mutex
	stopped map[string]string // project -> why it is never reconnected in this run (REPLACED, UPGRADE_REQUIRED)
	once    map[string]bool   // what was logged once

	flushAt   map[string]time.Time
	flushStep map[string]int

	update *updater // the scheduled self-update (nil: none)
}

type worker struct {
	key    string
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *worker) stop() {
	w.cancel()
	<-w.done
}

func (r *runner) stopAll() {
	for pid, w := range r.workers {
		w.stop()
		delete(r.workers, pid)
	}
}

func (r *runner) logOnce(what, format string, a ...any) {
	r.mu.Lock()
	seen := r.once[what]
	r.once[what] = true
	r.mu.Unlock()
	if !seen {
		r.o.Logf(format, a...)
	}
}

// round reads the sign-in, keys and outboxes again: starts a socket for each new key, stops those whose key went,
// sends what waits in the outbox, and checks the quiet exit. A reason to stop, or "".
func (r *runner) round(ctx context.Context) string {
	cfg, err := config.Load(r.dir)
	if err != nil {
		r.logOnce("config", "teamflow sync: cannot read the config: %v", err)
		return ""
	}
	if !cfg.SignedIn() {
		return "signed out"
	}
	b := store.Binding(cfg.Server, cfg.Email)
	if r.cfg == nil || b != r.binding || store.NormalizeServer(cfg.Server) != store.NormalizeServer(r.cfg.Server) {
		r.stopAll() // another sign-in: none of the old sockets belong to it
		r.mu.Lock()
		r.stopped = map[string]string{}
		r.mu.Unlock()
		r.binding = b
	}
	r.cfg = cfg
	projects, err := r.st.KeyProjects(b)
	if err == nil {
		want := map[string]string{}
		for _, pid := range projects {
			if key, _, _, _, ok := r.st.ProjectKey(b, pid); ok {
				want[pid] = key
			}
		}
		for pid, w := range r.workers {
			select {
			case <-w.done:
				delete(r.workers, pid) // ended on its own (dropped key, replaced)
				continue
			default:
			}
			if want[pid] != w.key {
				w.stop()
				delete(r.workers, pid)
			}
		}
		for pid, key := range want {
			r.mu.Lock()
			why := r.stopped[pid]
			r.mu.Unlock()
			if _, running := r.workers[pid]; running || why != "" {
				continue
			}
			wctx, cancel := context.WithCancel(ctx)
			w := &worker{key: key, cancel: cancel, done: make(chan struct{})}
			r.workers[pid] = w
			go func() {
				defer close(w.done)
				r.work(wctx, cfg.Server, pid, key)
			}()
		}
	}
	r.flush(ctx, b, cfg)
	if r.idle() {
		return fmt.Sprintf("no active agent session for %v", r.o.IdleExit)
	}
	return ""
}

// flush sends each outbox that has something waiting through core.Core.Sync (the sender lock, the order, the key
// fetch with its own backoff). An outbox a flush left items in is tried again after a wait, growing while nothing
// moves (and the lock's holder, a command, gets its turn meanwhile).
func (r *runner) flush(ctx context.Context, b store.KeyBinding, cfg *config.Config) {
	projects, _ := r.st.KeyProjects(b)
	waiting, _ := r.st.OutboxProjects(b)
	for _, p := range waiting {
		if !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
	}
	c := &core.Core{Dir: r.dir, Cfg: cfg, St: r.st, Now: r.now}
	for _, pid := range projects {
		ob := store.OutboxOf(b, pid)
		n, err := r.st.Waiting(ob)
		if err != nil || n == 0 {
			delete(r.flushStep, pid)
			delete(r.flushAt, pid)
			continue
		}
		if at, ok := r.flushAt[pid]; ok && r.now().Before(at) && !at.After(r.now().Add(time.Hour)) {
			continue
		}
		c.Sync(ctx, core.DefaultTimeout, pid)
		left, err := r.st.Waiting(ob)
		if err != nil || left == 0 {
			delete(r.flushStep, pid)
			delete(r.flushAt, pid)
			continue
		}
		if left < n { // it moved, then stopped short: the next try after the first wait
			r.flushStep[pid] = 0
		}
		r.flushAt[pid] = r.now().Add(r.o.backoff(r.flushStep[pid]))
		r.flushStep[pid]++
	}
}

// idle says whether no agent session has been active for IdleExit: none seen since (a time ahead, from a clock
// set back, counts as now), no agent process of a recent session alive, and the sync itself started before.
func (r *runner) idle() bool {
	now := r.now()
	last := r.started
	sessions, err := r.st.SessionsSince(now.Add(-agentWindow))
	if err != nil {
		return false // a busy store never ends the sync
	}
	for _, s := range sessions {
		if s.AgentPID > 0 {
			if started, ok := procs.StartTime(s.AgentPID); ok && !started.After(s.UpdatedAt.Add(pidReuseSlack)) {
				return false
			}
		}
		seen := s.UpdatedAt
		if seen.After(now) {
			seen = now
		}
		if seen.After(last) {
			last = seen
		}
	}
	return now.Sub(last) > r.o.IdleExit
}

// ------------------------------------------------------------------ one project

// outcome is how a socket ended.
type outcome int

const (
	stop    outcome = iota // never again in this run (ctx ended, REPLACED, NOT_A_MEMBER, UPGRADE_REQUIRED)
	atOnce                 // reconnect at once (TOO_SLOW, reset)
	silent                 // a live socket gone silent: at once, unless it keeps happening (silentInARow)
	dropped                // a live socket closed: reconnect after the first backoff
	failed                 // a failed connect: backoff, then the fallback
)

// silentInARow: after this many silence-caused reconnects in a row, a silence counts as a failed connect (backoff,
// then the sync.poll fallback): a link always slower than the silence limit settles instead of reconnecting forever.
const silentInARow = 3

// streak counts a project's failed connects and silences in a row.
type streak struct{ fails, silences int }

// end updates the streaks for how a socket ended (live for liveFor; healthy: long enough to say the link works)
// and says what to do next. A socket live at least healthy resets both streaks first, so silences spread over
// days (sleep and wake) never escalate. A silence reconnects at once, unless it is the silentInARow-th in a row:
// then it is a failed connect. Any other ending resets the silences; a reconnect at once or a drop resets the
// failed connects.
func (k *streak) end(o outcome, liveFor, healthy time.Duration) outcome {
	if liveFor >= healthy {
		k.fails, k.silences = 0, 0
	}
	if o == silent {
		k.silences++
		o = atOnce
		if k.silences >= silentInARow {
			o = failed
		}
	} else {
		k.silences = 0
	}
	switch o {
	case atOnce, dropped:
		k.fails = 0
	case failed:
		k.fails++
	}
	return o
}

// work keeps the project's socket open until ctx ends or it must stop.
func (r *runner) work(ctx context.Context, server, pid, key string) {
	defer r.clearLive(pid) // the fallback's last answer is not trusted past the sync's end
	healthy := r.o.Healthy
	var k streak
	for ctx.Err() == nil {
		if k.fails >= r.o.FailsBeforePoll {
			if !r.pollFor(ctx, server, pid, key, r.o.SocketRetry) {
				return
			}
		}
		var since time.Time
		o := r.session(ctx, server, pid, key, &since)
		var liveFor time.Duration
		if !since.IsZero() {
			liveFor = time.Since(since)
		}
		switch k.end(o, liveFor, healthy) {
		case stop:
			return
		case atOnce:
			continue
		case dropped:
			if !sleep(ctx, r.o.backoff(0)) {
				return
			}
		case failed:
			if k.fails < r.o.FailsBeforePoll && !sleep(ctx, r.o.backoff(k.fails-1)) {
				return
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// says logs one line about a project with the ticket and key masked.
func (r *runner) says(pid string, secrets ...string) func(format string, a ...any) {
	return func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		for _, s := range secrets {
			if s != "" {
				msg = strings.ReplaceAll(msg, s, "***")
			}
		}
		r.o.Logf("teamflow sync: %s: %s", pid, msg)
	}
}

// refusal acts on a refusal of the project's key or membership: true when the project must stop. NOT_A_MEMBER
// (live.ticket, sync.poll, the socket) drops the key. NOT_ALLOWED drops it only from sync.poll (keyRefusal), as
// Core.Sync does: from live.ticket it means a server with live sync off ("use sync.poll"), and on the socket an
// expired ticket or a disabled account: a failed connect, the fallback deciding.
func (r *runner) refusal(pid, key, reason string, keyRefusal bool) bool {
	switch protocol.RejectReason(reason) {
	case protocol.RejectReasonNotAMember:
		r.dropKey(pid, key)
		return true
	case protocol.RejectReasonNotAllowed:
		if keyRefusal {
			r.dropKey(pid, key)
			return true
		}
	case protocol.RejectReasonReplaced, protocol.RejectReasonUpgradeRequired:
		r.mu.Lock()
		r.stopped[pid] = reason
		r.mu.Unlock()
		return true
	}
	return false
}

// dropKey drops the project's key, unless it changed since this socket took it (a newer key is not the refused one).
func (r *runner) dropKey(pid, key string) {
	if k, _, _, _, ok := r.st.ProjectKey(r.binding, pid); ok && k == key {
		if err := r.tx(func(tx *store.Tx) error { return tx.DropProjectKey(r.binding, pid) }); err != nil {
			r.o.Logf("teamflow sync: %s: could not drop the refused key: %v", pid, err)
		}
	}
}

// tx runs fn in one transaction, trying again while the store is busy (a hook or command writing).
func (r *runner) tx(fn func(*store.Tx) error) error {
	var err error
	for i := range 10 {
		if err = r.st.Tx(fn); !errors.Is(err, store.ErrBusy) {
			return err
		}
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
	return err
}

type message struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

// deadAfterSilence: the silence timer of a ping sent at pingAt fired; the socket is dead only when the reader heard
// nothing at or after it (unix ns). A frame heard but still queued behind a slow apply keeps it alive.
func deadAfterSilence(heard, pingAt int64) bool { return heard < pingAt }

// session is one socket of the project; since is set when it goes live.
func (r *runner) session(ctx context.Context, server, pid, key string, since *time.Time) outcome {
	say := r.says(pid, key)
	a := api.New(server, key, core.DefaultTimeout).WithRecorder(r.st)
	var tk protocol.LiveTicket
	if err := a.Call(ctx, "live.ticket", protocol.ProjectRef{Project: pid}, &tk); err != nil {
		if ctx.Err() != nil {
			return stop
		}
		var ae *api.APIError
		if errors.As(err, &ae) && r.refusal(pid, key, ae.Reason, false) {
			say("live.ticket refused, not reconnecting: %v", err)
			return stop
		}
		if errors.As(err, &ae) && ae.Reason == string(protocol.RejectReasonNotAllowed) {
			// Live sync off on this server ("use sync.poll"): said once per project, not at every retry.
			r.logOnce("ticket refused "+pid, "teamflow sync: %s: live.ticket: %v", pid, err)
			return failed
		}
		say("live.ticket: %v", err)
		return failed
	}
	say = r.says(pid, key, tk.Ticket)
	u, err := api.LiveURL(server, tk.Org, pid)
	if err != nil {
		say("%v", err)
		return failed
	}
	dctx, cancel := context.WithTimeout(ctx, core.DefaultTimeout)
	conn, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{protocol.ClientHeader: {api.ClientName()}},
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return stop
		}
		say("the live socket did not open: %v", err)
		return failed
	}
	defer conn.CloseNow()
	defer r.clearLive(pid) // however it ends, hooks stop trusting it at once
	conn.SetReadLimit(readLimit)
	cursors, err := r.st.CursorsErr()
	if err != nil { // never from 0 because the store could not say where we are
		say("read the cursor: %v", err)
		return failed
	}
	hello := protocol.NewLiveHello()
	hello.Ticket, hello.Cursor = tk.Ticket, cursors[pid]
	b, _ := hello.MarshalJSON()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		say("send the hello: %v", err)
		return failed
	}
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	msgs := make(chan message, 16)
	var heard atomic.Int64 // when the reader last got anything (unix ns): the silence check reads it
	go func() {
		for {
			typ, data, err := conn.Read(sctx)
			if err == nil {
				heard.Store(time.Now().UnixNano())
			}
			select {
			case msgs <- message{typ, data, err}:
			case <-sctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	end := func(code websocket.StatusCode, o outcome) outcome {
		_ = conn.Close(code, "")
		return o
	}

	live := false
	first := time.NewTimer(r.o.FirstAnswer)
	defer first.Stop()
	var ping <-chan time.Time
	var silence *time.Timer
	var pingAt int64
	silenceC := func() <-chan time.Time {
		if silence == nil {
			return nil
		}
		return silence.C
	}
	stopSilence := func() {
		if silence != nil {
			silence.Stop()
			silence = nil
		}
	}
	defer stopSilence()
	for {
		select {
		case <-ctx.Done():
			return end(websocket.StatusNormalClosure, stop)
		case <-first.C:
			if !live {
				say("no answer to the hello within %v", r.o.FirstAnswer)
				return end(websocket.StatusNormalClosure, failed)
			}
		case <-ping:
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, []byte(protocol.LivePing))
			wcancel()
			if err != nil {
				say("the live socket closed: %v", err)
				return dropped
			}
			if silence == nil {
				pingAt = time.Now().UnixNano()
				silence = time.NewTimer(r.o.Silence)
			}
		case <-silenceC():
			if !deadAfterSilence(heard.Load(), pingAt) {
				stopSilence()
				continue
			}
			say("nothing heard within %v of a ping: reconnecting", r.o.Silence)
			return end(websocket.StatusNormalClosure, silent)
		case m := <-msgs:
			if m.err != nil {
				if ctx.Err() != nil {
					return stop
				}
				if live {
					say("the live socket closed: %v", m.err)
					return dropped
				}
				say("the live socket closed before its first answer: %v", m.err)
				return failed
			}
			stopSilence() // anything heard: the socket lives
			if m.typ == websocket.MessageText && string(m.data) == protocol.LivePong {
				if live { // heard: hooks may trust the replica a while longer
					_ = r.tx(func(tx *store.Tx) error { return tx.SetLiveAt(pid, r.now()) })
				}
				continue
			}
			msg, err := protocol.DecodeLiveMessage(m.data)
			if err != nil {
				say("an unreadable live message: %v", err)
				return end(websocket.StatusUnsupportedData, failed)
			}
			switch v := msg.(type) {
			case protocol.LiveEvents:
				if v.Reset {
					if err := r.tx(func(tx *store.Tx) error { return tx.ResetProject(pid, v.Cursor) }); err != nil {
						say("reset: %v", err)
						return end(websocket.StatusNormalClosure, failed)
					}
					say("the server's head %d is behind this replica: dropped it, catching up from 0", v.Cursor)
					return end(websocket.StatusNormalClosure, atOnce)
				}
				if err := r.apply(pid, v); err != nil {
					say("apply: %v", err)
					return end(websocket.StatusNormalClosure, failed)
				}
				if !live {
					live = true
					*since = time.Now()
					say("live")
					t := time.NewTicker(r.o.PingEvery)
					defer t.Stop()
					ping = t.C
				}
			case protocol.LiveClosed:
				reason := string(v.Reason)
				switch {
				case v.Reason == protocol.RejectReasonTooSlow:
					say("closed (%s: %s): reconnecting at once", reason, v.Message)
					return atOnce
				case r.refusal(pid, key, reason, false):
					say("closed (%s: %s): not reconnecting", reason, v.Message)
					return stop
				}
				say("closed (%s: %s)", reason, v.Message)
				return failed
			case protocol.UnknownLiveMessage:
				if live {
					_ = r.tx(func(tx *store.Tx) error { return tx.SetLiveAt(pid, r.now()) })
				}
				r.logOnce("live message "+v.Tag, "teamflow sync: skipped %q live messages: this server is newer than this client", v.Tag)
			}
		}
	}
}

// clearLive forgets the project's live_at: its socket ended, hooks pull again.
func (r *runner) clearLive(pid string) {
	if err := r.tx(func(tx *store.Tx) error { return tx.ClearLiveAt(pid) }); err != nil {
		r.o.Logf("teamflow sync: %s: could not clear live_at: %v", pid, err)
	}
}

// apply records one LiveEvents in one transaction: each event not applied yet, the cursor, and live_at.
func (r *runner) apply(pid string, v protocol.LiveEvents) error {
	var done []int64
	err := r.tx(func(tx *store.Tx) error {
		done = done[:0]
		if _, err := syncer.ApplyEvents(tx, v.Events, r.now(), func(_ string, seq int64) { done = append(done, seq) }); err != nil {
			return err
		}
		if err := tx.SetCursor(pid, v.Cursor); err != nil {
			return err
		}
		return tx.SetLiveAt(pid, r.now())
	})
	if err == nil && r.o.Applied != nil {
		for _, seq := range done {
			r.o.Applied(pid, seq)
		}
	}
	return err
}

// pollFor is the fallback: sync.poll with PollWait from the cursor until d passes (then the socket is tried
// again). False when the project must stop (its key refused) or ctx ended.
func (r *runner) pollFor(ctx context.Context, server, pid, key string, d time.Duration) bool {
	say := r.says(pid, key)
	r.logOnce("poll "+pid, "teamflow sync: %s: the live socket cannot open: polling (sync.poll) instead", pid)
	a := api.New(server, key, r.o.PollWait+core.DefaultTimeout).WithRecorder(r.st)
	until := time.Now().Add(d)
	fails := 0
	for time.Now().Before(until) {
		pctx, cancel := context.WithDeadline(ctx, until.Add(r.o.PollWait+core.DefaultTimeout))
		_, err := syncer.Poll(pctx, r.st, a, pid, r.o.PollWait, 0)
		cancel()
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			fails = 0
			_ = r.tx(func(tx *store.Tx) error { return tx.SetLiveAt(pid, r.now()) })
			continue
		}
		var ae *api.APIError
		if errors.As(err, &ae) && r.refusal(pid, key, ae.Reason, true) {
			say("sync.poll refused, stopping: %v", err)
			return false
		}
		say("sync.poll: %v", err)
		if !sleep(ctx, r.o.backoff(fails)) {
			return false
		}
		fails++
	}
	return true
}
