package live

// The scheduled self-update, run by the background sync against a fake release (package selfupdatetest: a
// throwaway key and a TLS test server; nothing reaches GitHub), with an injectable clock.
//
// Sad paths covered: the same and an older release (nothing done), a bad signature (nothing changed, tried again
// only at the next check), offline and GitHub's rate limit (no retry storm), off by config and by env, a build that
// is not a release, a binary folder that can't be written (one notice per release), a sender lock held past the
// wait (no swap; swapped at the next check) and one released during the wait (swapped after it), a restart (the
// last check kept: no new check), and a clock set back.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate/selfupdatetest"
	"github.com/vutran1710/teamflow-cli/internal/sendlock"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// fakeClock is a clock the test moves; it ticks a microsecond each time it is read.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Microsecond)
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// updating is a machine (signed in on f, with keys for projects, session s1 seen now) whose clock the test moves,
// running a stand-in teamflow 0.1.0 in a folder of its own, with the sync's options for a quick schedule: the first
// check at once, then every 24 h with no jitter.
func updating(t *testing.T, rel *selfupdatetest.Release, projects ...string) (*core.Core, *fakeClock, string, Options, *logs) {
	t.Helper()
	t.Setenv(config.AutoUpdateEnv, "")
	f := newFake(t)
	c := machine(t, f, projects...)
	clk := &fakeClock{t: time.Now()}
	c.Now = clk.now
	exe := selfupdatetest.Installed(t, t.TempDir(), "0.1.0")
	l := &logs{}
	o := fast(l, nil)
	o.IdleExit = 1000 * time.Hour // the test moves the clock by days
	o.AutoUpdate = &AutoUpdate{Updater: rel.Updater(exe, nil), Current: "0.1.0", First: time.Nanosecond,
		Every: 24 * time.Hour, LockWait: 100 * time.Millisecond, Rand: func() float64 { return 0.5 }}
	return c, clk, exe, o, l
}

func binaryOf(t *testing.T, exe string) string {
	t.Helper()
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// besideBinary is what the binary's folder holds, but the update lock (it stays there, as an install's).
func besideBinary(t *testing.T, exe string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(exe))
	var out []string
	for _, e := range entries {
		if e.Name() != selfupdate.LockName {
			out = append(out, e.Name())
		}
	}
	return out
}

func noticesOf(t *testing.T, c *core.Core, session string) []string {
	t.Helper()
	ns, err := c.St.Notices(session, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		out = append(out, n.Text)
	}
	return out
}

// A newer signed release: installed (checked by package selfupdate), the run ends with Updated naming the binary,
// which is now the new release (the restart picks it up); every session hears of it, /teamf status will tell it
// once, and the check is recorded.
func TestScheduledUpdateInstallsAndRestarts(t *testing.T) {
	rel := selfupdatetest.New(t, "v0.1.1", "")
	c, _, exe, o, l := updating(t, rel)
	wait, _ := start(t, c, o)
	err := wait()
	var up *Updated
	if !errors.As(err, &up) || up.Exe != exe || up.Version != "v0.1.1" {
		t.Fatalf("Run ended with %v", err)
	}
	if got := binaryOf(t, up.Exe); got != "teamflow 0.1.1" {
		t.Fatalf("the restart would run %q", got)
	}
	if _, running := Status(c.Dir); running {
		t.Fatal("the lock is still held: the new binary's sync could not start")
	}
	for _, s := range []string{"", "s1"} {
		if got := noticesOf(t, c, s); len(got) != 1 || got[0] != "teamflow was updated to v0.1.1." {
			t.Fatalf("session %q notices %q", s, got)
		}
	}
	if v, _ := c.St.TakeUpdatedTo(); v != "v0.1.1" {
		t.Fatalf("updated to %q", v)
	}
	if c.St.UpdateCheckedAt().IsZero() {
		t.Fatal("the check was not recorded")
	}
	if !strings.Contains(l.String(), "teamflow sync: updated teamflow 0.1.0 -> v0.1.1; restarting on the new binary") {
		t.Fatalf("log: %s", l)
	}
	if strings.Contains(l.String(), os.Getenv("HOME")) {
		t.Fatalf("the log names the home folder: %s", l)
	}
}

// The same or an older release: nothing downloaded or changed, the run goes on, and no check again until the next
// is due.
func TestScheduledUpdateDoesNothingWhenNotNewer(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v0.0.9"} {
		t.Run(tag, func(t *testing.T) {
			rel := selfupdatetest.New(t, tag, "")
			c, _, exe, o, l := updating(t, rel)
			wait, stop := start(t, c, o)
			eventually(t, "a check", func() bool { return strings.Contains(l.String(), "is up to date") })
			time.Sleep(10 * o.Rescan)
			stop()
			if err := wait(); err != nil {
				t.Fatal(err)
			}
			if rel.Asked() != 1 || rel.Downloaded() || binaryOf(t, exe) != "teamflow 0.1.0" {
				t.Fatalf("asked %d downloaded %v", rel.Asked(), rel.Downloaded())
			}
			if !strings.Contains(l.String(), "teamflow 0.1.0 is up to date") || len(noticesOf(t, c, "s1")) != 0 {
				t.Fatalf("log %s", l)
			}
		})
	}
}

// A bad signature: nothing changes, and it is tried again only at the next check (24 h on).
func TestScheduledUpdateBadSignatureRetriesNextCycle(t *testing.T) {
	rel := selfupdatetest.New(t, "v0.1.1", "")
	rel.SignedByAnotherKey()
	c, clk, exe, o, l := updating(t, rel)
	wait, stop := start(t, c, o)
	eventually(t, "a check, refused", func() bool { return strings.Contains(l.String(), "failed its signature check") })
	time.Sleep(10 * o.Rescan)
	if rel.Asked() != 1 || binaryOf(t, exe) != "teamflow 0.1.0" || rel.Downloaded() {
		t.Fatalf("asked %d", rel.Asked())
	}
	if left := besideBinary(t, exe); len(left) != 1 {
		t.Fatalf("left beside the binary: %v", left)
	}
	clk.add(23 * time.Hour)
	time.Sleep(10 * o.Rescan)
	if rel.Asked() != 1 {
		t.Fatal("checked again before the next cycle")
	}
	clk.add(time.Hour)
	eventually(t, "the next cycle's check, refused", func() bool {
		return strings.Count(l.String(), "failed its signature check") == 2 // its outcome, not only its request
	})
	stop()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if rel.Asked() != 2 || binaryOf(t, exe) != "teamflow 0.1.0" {
		t.Fatalf("asked %d: %s", rel.Asked(), l)
	}
}

// Offline, or GitHub's rate limit: one try per cycle, never a storm; nothing changes.
func TestScheduledUpdateOfflineOrRateLimitedNoStorm(t *testing.T) {
	t.Run("rate limited", func(t *testing.T) {
		rel := selfupdatetest.New(t, "v0.1.1", "")
		rel.Answer(http.StatusTooManyRequests)
		c, _, exe, o, l := updating(t, rel)
		wait, stop := start(t, c, o)
		eventually(t, "a check", func() bool { return strings.Contains(l.String(), "rate limit") })
		time.Sleep(20 * o.Rescan)
		stop()
		_ = wait()
		if rel.Asked() != 1 || binaryOf(t, exe) != "teamflow 0.1.0" {
			t.Fatalf("asked %d; %s", rel.Asked(), l)
		}
	})
	t.Run("offline", func(t *testing.T) {
		rel := selfupdatetest.New(t, "v0.1.1", "")
		c, _, exe, o, l := updating(t, rel)
		o.AutoUpdate.Updater = selfupdatetest.OfflineUpdater(t, exe)
		wait, stop := start(t, c, o)
		eventually(t, "a check", func() bool { return strings.Contains(l.String(), "no update this time") })
		time.Sleep(20 * o.Rescan)
		stop()
		_ = wait()
		if n := strings.Count(l.String(), "no update this time"); n != 1 || binaryOf(t, exe) != "teamflow 0.1.0" {
			t.Fatalf("tried %d times; %s", n, l)
		}
	})
}

// Off by config or by env, or a build that is not a release: no check at all (said once in the log).
func TestScheduledUpdateOptOut(t *testing.T) {
	for name, set := range map[string]func(t *testing.T, c *core.Core, o *Options){
		"config": func(t *testing.T, c *core.Core, _ *Options) {
			if err := config.SetAutoUpdate(c.Dir, false); err != nil {
				t.Fatal(err)
			}
		},
		"env":       func(t *testing.T, _ *core.Core, _ *Options) { t.Setenv(config.AutoUpdateEnv, "0") },
		"dev build": func(_ *testing.T, _ *core.Core, o *Options) { o.AutoUpdate.Current = "dev" },
	} {
		t.Run(name, func(t *testing.T) {
			rel := selfupdatetest.New(t, "v0.1.1", "")
			c, _, exe, o, l := updating(t, rel)
			set(t, c, &o)
			wait, stop := start(t, c, o)
			time.Sleep(20 * o.Rescan)
			stop()
			if err := wait(); err != nil {
				t.Fatal(err)
			}
			if rel.Asked() != 0 || binaryOf(t, exe) != "teamflow 0.1.0" || !c.St.UpdateCheckedAt().IsZero() {
				t.Fatalf("asked %d", rel.Asked())
			}
			if strings.Count(l.String(), "auto-update is off")+strings.Count(l.String(), "is not a release") != 1 {
				t.Fatalf("log: %s", l)
			}
		})
	}
}

// A binary folder that can't be written: one notice per release (to the person: reinstall), never a retry storm; a
// newer release later gets its own notice.
func TestScheduledUpdateNotWritableOneNoticePerVersion(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	rel := selfupdatetest.New(t, "v0.1.1", "")
	c, clk, exe, o, l := updating(t, rel)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	wait, stop := start(t, c, o)
	want := "a new teamflow v0.1.1 is available; the install folder isn't writable: reinstall with install.sh"
	eventually(t, "the notice", func() bool { return len(noticesOf(t, c, "s1")) == 1 })
	clk.add(25 * time.Hour) // the next cycle: checked again, no second notice
	eventually(t, "the next check", func() bool { return strings.Count(l.String(), "can't be installed here") == 2 })
	stop()
	_ = wait()
	for _, s := range []string{"", "s1"} {
		if got := noticesOf(t, c, s); len(got) != 1 || got[0] != want {
			t.Fatalf("session %q: %q", s, got)
		}
	}
	if strings.Contains(l.String(), os.Getenv("HOME")+"/") {
		t.Fatalf("the log names the home folder: %s", l)
	}
	// A newer release: its own notice (s1 seen again meanwhile).
	rel2 := selfupdatetest.New(t, "v0.1.2", "")
	o.AutoUpdate.Updater = rel2.Updater(exe, nil)
	clk.add(25 * time.Hour)
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", clk.now()); err != nil {
		t.Fatal(err)
	}
	wait, stop = start(t, c, o)
	eventually(t, "the second release's notice", func() bool { return len(noticesOf(t, c, "s1")) == 2 })
	stop()
	_ = wait()
	if got := noticesOf(t, c, "s1"); got[1] != "a new teamflow v0.1.2 is available; the install folder isn't writable: reinstall with install.sh" {
		t.Fatalf("%q", got)
	}
	if binaryOf(t, exe) != "teamflow 0.1.0" {
		t.Fatal("the binary changed")
	}
}

// Never mid-send: with a project's sender lock held past the wait, nothing is swapped (tried again at the next
// check, then swapped); a lock let go during the wait: swapped after it.
func TestScheduledUpdateWaitsForTheSenderLock(t *testing.T) {
	t.Run("held past the wait", func(t *testing.T) {
		rel := selfupdatetest.New(t, "v0.1.1", "")
		c, clk, exe, o, l := updating(t, rel, P1)
		release, err := sendlock.Acquire(context.Background(), c.Dir, store.OutboxOf(binding(c), P1).Ref(), 0)
		if err != nil {
			t.Fatal(err)
		}
		wait, stop := start(t, c, o)
		eventually(t, "the refusal", func() bool { return strings.Contains(l.String(), "a send was still in flight") })
		if binaryOf(t, exe) != "teamflow 0.1.0" {
			t.Fatal("swapped while a send was in flight")
		}
		if left := besideBinary(t, exe); len(left) != 1 {
			t.Fatalf("left beside the binary: %v", left)
		}
		release()
		clk.add(25 * time.Hour)
		var up *Updated
		if err := wait(); !errors.As(err, &up) || binaryOf(t, exe) != "teamflow 0.1.1" {
			t.Fatalf("Run ended with %v", err)
		}
		stop()
	})
	t.Run("let go during the wait", func(t *testing.T) {
		rel := selfupdatetest.New(t, "v0.1.1", "")
		c, _, exe, o, _ := updating(t, rel, P1)
		o.AutoUpdate.LockWait = 5 * time.Second
		release, err := sendlock.Acquire(context.Background(), c.Dir, store.OutboxOf(binding(c), P1).Ref(), 0)
		if err != nil {
			t.Fatal(err)
		}
		wait, _ := start(t, c, o)
		eventually(t, "downloaded", rel.Downloaded)
		time.Sleep(300 * time.Millisecond) // checked and waiting for the lock
		if binaryOf(t, exe) != "teamflow 0.1.0" {
			t.Fatal("swapped while the lock was held")
		}
		release()
		var up *Updated
		if err := wait(); !errors.As(err, &up) || binaryOf(t, exe) != "teamflow 0.1.1" {
			t.Fatalf("not swapped after the lock was let go: %v", err)
		}
	})
}

// A restart keeps the last check: a new run does not check again until the next is due.
func TestScheduledUpdateRestartKeepsTheLastCheck(t *testing.T) {
	rel := selfupdatetest.New(t, "v0.1.0", "")
	c, clk, _, o, _ := updating(t, rel)
	wait, stop := start(t, c, o)
	eventually(t, "a check", func() bool { return c.St.UpdateCheckedAt() != (time.Time{}) && rel.Asked() == 1 })
	stop()
	_ = wait()
	clk.add(time.Hour)
	wait, stop = start(t, c, o) // the sync restarted (a reboot, its quiet exit): the check is kept
	time.Sleep(20 * o.Rescan)
	stop()
	_ = wait()
	if rel.Asked() != 1 {
		t.Fatalf("asked %d times", rel.Asked())
	}
}

// The schedule: First after the start, then Every ± the jitter after the last check; none yet, or one ahead of
// now (a clock set back): due.
func TestUpdateSchedule(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		last, now time.Time
		rnd       float64
		want      bool
	}{
		"before the first wait":       {time.Time{}, t0.Add(9 * time.Minute), 0.5, false},
		"the first check":             {time.Time{}, t0.Add(10 * time.Minute), 0.5, true},
		"checked an hour ago":         {t0.Add(-time.Hour), t0.Add(10 * time.Minute), 0.5, false},
		"a day later":                 {t0.Add(-24 * time.Hour), t0.Add(10 * time.Minute), 0.5, true},
		"jitter +1h: not yet at 24h":  {t0.Add(-24 * time.Hour), t0.Add(10 * time.Minute), 1, false},
		"jitter +1h: at 25h":          {t0.Add(-25*time.Hour + 10*time.Minute), t0.Add(10 * time.Minute), 1, true},
		"jitter -1h: at 23h":          {t0.Add(-23*time.Hour + 10*time.Minute), t0.Add(10 * time.Minute), 0, true},
		"jitter -1h: not at 22h":      {t0.Add(-22 * time.Hour), t0.Add(10 * time.Minute), 0, false},
		"a check ahead (clock back)":  {t0.Add(48 * time.Hour), t0.Add(10 * time.Minute), 0.5, true},
		"never checked, long running": {time.Time{}, t0.Add(72 * time.Hour), 0.5, true},
	} {
		a := (&AutoUpdate{Rand: func() float64 { return c.rnd }}).withDefaults()
		u := &updater{a: a, last: c.last}
		u.draw()
		if got := u.due(c.now, t0); got != c.want {
			t.Errorf("%s: due %v (jitter %v)", name, got, u.jitter)
		}
	}
	if a := (&AutoUpdate{Jitter: -time.Hour}).withDefaults(); a.First != 10*time.Minute || a.Every != 24*time.Hour ||
		a.Jitter != time.Hour || a.LockWait != 30*time.Second {
		t.Fatalf("defaults %+v", a)
	}
}

// The binary on disk is no longer the one this sync started from (a developer's `go build -o …`, a newer install):
// the scheduled update never replaces it; the sync just restarts onto it, asking GitHub nothing.
func TestScheduledUpdateNeverReplacesABinaryChangedOnDisk(t *testing.T) {
	rel := selfupdatetest.New(t, "v0.1.1", "")
	c, _, exe, o, l := updating(t, rel)
	wait, _ := start(t, c, o)
	eventually(t, "running", func() bool { _, ok := Status(c.Dir); return ok })
	if err := os.WriteFile(exe, []byte(selfupdatetest.Script("dev")), 0o755); err != nil { // the same file, rewritten
		t.Fatal(err)
	}
	// (started before the rewrite: First is a nanosecond, so the check may already be under way; either way:)
	err := wait()
	var up *Updated
	if !errors.As(err, &up) || up.Exe != exe || up.Version != "" {
		t.Fatalf("Run ended with %v", err)
	}
	if got := binaryOf(t, exe); got != "teamflow dev" {
		t.Fatalf("the binary on disk was replaced: %q", got)
	}
	if strings.Contains(l.String(), "updated teamflow") || len(noticesOf(t, c, "s1")) != 0 {
		t.Fatalf("told of an update: %s", l)
	}
}

// Changed on disk before the first check: nothing is asked of GitHub at all.
func TestScheduledUpdateChecksTheBinaryBeforeAsking(t *testing.T) {
	rel := selfupdatetest.New(t, "v0.1.1", "")
	c, _, exe, o, l := updating(t, rel)
	o.AutoUpdate.First = 300 * time.Millisecond // the clock ticks a microsecond a read: never, unless moved
	clk := c.Now
	moved := false
	var mu sync.Mutex
	c.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if moved {
			return clk().Add(time.Hour)
		}
		return clk()
	}
	wait, _ := start(t, c, o)
	eventually(t, "running", func() bool { _, ok := Status(c.Dir); return ok })
	if err := os.WriteFile(exe, []byte(selfupdatetest.Script("dev")), 0o755); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	moved = true
	mu.Unlock()
	var up *Updated
	if err := wait(); !errors.As(err, &up) || up.Version != "" || rel.Asked() != 0 {
		t.Fatalf("Run ended with %v; GitHub asked %d times", err, rel.Asked())
	}
	if !strings.Contains(l.String(), "restarting on it") {
		t.Fatalf("log: %s", l)
	}
}

// Rewritten while the update waits for a send to finish: the swap refuses (the file is not what it was), the binary
// stays, and the sync restarts onto it.
func TestScheduledUpdateRefusesABinaryChangedMeanwhile(t *testing.T) {
	rel := selfupdatetest.New(t, "v0.1.1", "")
	c, _, exe, o, _ := updating(t, rel, P1)
	o.AutoUpdate.LockWait = 5 * time.Second
	release, err := sendlock.Acquire(context.Background(), c.Dir, store.OutboxOf(binding(c), P1).Ref(), 0)
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := start(t, c, o)
	eventually(t, "downloaded", rel.Downloaded)
	time.Sleep(300 * time.Millisecond) // checked, waiting for the sender lock
	if err := os.WriteFile(exe, []byte(selfupdatetest.Script("dev")), 0o755); err != nil {
		t.Fatal(err)
	}
	release()
	var up *Updated
	if err := wait(); !errors.As(err, &up) || up.Version != "" {
		t.Fatalf("Run ended with %v", err)
	}
	if got := binaryOf(t, exe); got != "teamflow dev" {
		t.Fatalf("replaced: %q", got)
	}
}

// A manual update (self-update, /teamf upgrade) and the scheduled one at once, the scheduled one seeing an older
// "latest" (a lagging answer): they take turns, and the binary never goes back to the older release, whichever
// goes first.
func TestManualAndScheduledUpdatesNeverDowngrade(t *testing.T) {
	t.Run("manual first", func(t *testing.T) {
		newer, older := selfupdatetest.New(t, "v0.1.2", ""), selfupdatetest.New(t, "v0.1.1", "")
		c, _, exe, o, _ := updating(t, older)
		manual := newer.Updater(exe, nil)
		manual.Lock, manual.Expect = true, selfupdate.ExpectVersion("0.1.0")
		inHold, let := make(chan struct{}), make(chan struct{})
		manual.Hold = func(context.Context) (func(), error) { close(inHold); <-let; return func() {}, nil }
		done := make(chan error, 1)
		go func() { _, err := manual.Install(context.Background(), "0.1.0"); done <- err }()
		select { // the manual update is about to put v0.1.2 in place, holding the update lock
		case <-inHold:
		case err := <-done:
			t.Fatalf("the manual update ended before its swap: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the manual update never reached its swap")
		}
		wait, _ := start(t, c, o)
		time.Sleep(300 * time.Millisecond) // the scheduled check waits its turn meanwhile
		close(let)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the manual update never finished")
		}
		var up *Updated
		if err := wait(); !errors.As(err, &up) || up.Version != "" {
			t.Fatalf("Run ended with %v", err)
		}
		if got := binaryOf(t, exe); got != "teamflow 0.1.2" {
			t.Fatalf("downgraded: %q", got)
		}
	})
	t.Run("scheduled first", func(t *testing.T) {
		newer, older := selfupdatetest.New(t, "v0.1.2", ""), selfupdatetest.New(t, "v0.1.1", "")
		c, _, exe, o, _ := updating(t, older)
		wait, _ := start(t, c, o)
		var up *Updated
		if err := wait(); !errors.As(err, &up) || up.Version != "v0.1.1" {
			t.Fatalf("Run ended with %v", err)
		}
		manual := newer.Updater(exe, nil)
		manual.Lock, manual.Expect = true, selfupdate.ExpectVersion("0.1.0")
		if _, err := manual.Install(context.Background(), "0.1.0"); selfupdate.KindOf(err) != selfupdate.Changed {
			t.Fatalf("a manual update started from 0.1.0 went on: %v", err)
		}
		if got := binaryOf(t, exe); got != "teamflow 0.1.1" {
			t.Fatalf("%q", got)
		}
	})
}

// linkedUpdate: the sync was started through a symlink (os.Executable gives the link on macOS) to a stand-in
// teamflow 0.1.0; after it starts, change repoints what the link names (it returns the binary the link names now);
// then the check runs. That binary is never replaced, and the sync restarts onto it.
func linkedUpdate(t *testing.T, change func(t *testing.T, link string) string) {
	t.Helper()
	rel := selfupdatetest.New(t, "v0.1.1", "")
	c, _, target, o, l := updating(t, rel)
	link := filepath.Join(t.TempDir(), "teamflow")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	o.AutoUpdate.Updater.Exe = link
	o.AutoUpdate.First = 300 * time.Millisecond // the clock ticks a microsecond a read: never, unless moved
	clk := c.Now
	moved := false
	var mu sync.Mutex
	c.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if moved {
			return clk().Add(time.Hour)
		}
		return clk()
	}
	wait, _ := start(t, c, o)
	eventually(t, "running", func() bool { _, ok := Status(c.Dir); return ok })
	now := change(t, link)
	mu.Lock()
	moved = true
	mu.Unlock()
	err := wait()
	if got := binaryOf(t, now); got != "teamflow dev" {
		t.Fatalf("the binary another install put at %s was replaced: it says %q (log %s)", now, got, l)
	}
	var up *Updated
	if !errors.As(err, &up) || up.Version != "" || up.Exe != link || rel.Asked() != 0 {
		t.Fatalf("Run ended with %v; GitHub asked %d times", err, rel.Asked())
	}
}

// The symlink is repointed at another binary (a developer's build, a package manager's newer version).
func TestScheduledUpdateSymlinkRetargeted(t *testing.T) {
	linkedUpdate(t, func(t *testing.T, link string) string {
		dev := selfupdatetest.Installed(t, t.TempDir(), "dev")
		tmp := link + ".new"
		if err := os.Symlink(dev, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
		return dev
	})
}

// install.sh's `mv -f new $bindir/teamflow` (or `go build -o`) over a symlinked install: a regular file replaces the
// link.
func TestScheduledUpdateFileRenamedOverSymlink(t *testing.T) {
	linkedUpdate(t, func(t *testing.T, link string) string {
		tmp := link + ".new"
		if err := os.WriteFile(tmp, []byte(selfupdatetest.Script("dev")), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
		return link
	})
}

// Two Teamflow homes (two people on one machine, TEAMFLOW_HOME) share one binary: their updates take turns too (the
// update lock lives beside the binary). Home B's manual update to v0.1.2 holds it; home A's scheduled update (seeing
// a lagging v0.1.1) waits, then finds the binary changed and restarts onto it: never a downgrade.
func TestUpdatesOfTwoHomesSharingABinaryTakeTurns(t *testing.T) {
	newer, older := selfupdatetest.New(t, "v0.1.2", ""), selfupdatetest.New(t, "v0.1.1", "")
	c, _, exe, o, _ := updating(t, older) // home A
	manual := newer.Updater(exe, nil)     // home B's: its own home, the same binary
	manual.Lock, manual.Expect = true, selfupdate.ExpectVersion("0.1.0")
	inHold, let := make(chan struct{}), make(chan struct{})
	manual.Hold = func(context.Context) (func(), error) { close(inHold); <-let; return func() {}, nil }
	done := make(chan error, 1)
	go func() { _, err := manual.Install(context.Background(), "0.1.0"); done <- err }()
	select {
	case <-inHold:
	case err := <-done:
		t.Fatalf("home B's update ended before its swap: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("home B's update never reached its swap")
	}
	wait, _ := start(t, c, o)
	time.Sleep(300 * time.Millisecond) // home A's check waits its turn meanwhile
	if binaryOf(t, exe) != "teamflow 0.1.0" {
		t.Fatal("home A's update did not wait its turn")
	}
	close(let)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("home B's update never finished")
	}
	var up *Updated
	if err := wait(); !errors.As(err, &up) || up.Version != "" {
		t.Fatalf("home A's sync ended with %v", err)
	}
	if got := binaryOf(t, exe); got != "teamflow 0.1.2" {
		t.Fatalf("downgraded: %q", got)
	}
}
