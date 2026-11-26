package cli

// `teamflow sync [--background|--status|--stop]` and the start hooks use (StartSync): one copy per machine, started
// detached with its output in sync.log (0600), nothing secret in its arguments, never started signed out.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/live"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate/selfupdatetest"
)

// spawned records what a start would have launched instead of launching it.
func spawned(t *testing.T) *[]*exec.Cmd {
	t.Helper()
	var got []*exec.Cmd
	old := spawn
	spawn = func(cmd *exec.Cmd) error {
		got = append(got, cmd)
		for _, f := range []any{cmd.Stdin, cmd.Stdout} {
			if c, ok := f.(*os.File); ok {
				_ = c.Close()
			}
		}
		return nil
	}
	t.Cleanup(func() { spawn = old })
	return &got
}

// holdLock takes the one-copy lock as a running copy would, with pid in it.
func holdLock(t *testing.T, dir string, pid int) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, live.LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() })
}

func TestSyncCommand(t *testing.T) {
	t.Run("not running", func(t *testing.T) {
		m := newMachine(t, true)
		got := spawned(t)
		r := m.run("", "sync", "--status")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "not running")
		r = m.run("", "sync", "--stop")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "not running")
		if len(*got) != 0 {
			t.Fatal("status or stop started one")
		}
	})
	t.Run("bad use", func(t *testing.T) {
		m := newMachine(t, true)
		got := spawned(t)
		for _, args := range [][]string{{"sync", "--status", "--stop"}, {"sync", "--background", "--stop"}, {"sync", "now"}, {"sync", "--bogus"}} {
			wantCode(t, m.run("", args...), 2)
		}
		if len(*got) != 0 {
			t.Fatal("started on bad use")
		}
	})
	t.Run("background", func(t *testing.T) {
		m := newMachine(t, true)
		log := filepath.Join(m.dir, "sync.log")
		must(t, os.WriteFile(log, []byte("an older run\n"), 0o644))
		got := spawned(t)
		r := m.run("", "sync", "--background")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "started", "sync.log")
		if len(*got) != 1 {
			t.Fatalf("started %d", len(*got))
		}
		cmd := (*got)[0]
		if len(cmd.Args) != 2 || cmd.Args[1] != "sync" {
			t.Fatalf("args %q", cmd.Args)
		}
		for _, a := range cmd.Args {
			lacks(t, "args", a, auth, projKey, "tfa_", "tfk_")
		}
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
			t.Fatal("not detached from the terminal's session")
		}
		if out, ok := cmd.Stdout.(*os.File); !ok || out.Name() != log || cmd.Stderr != cmd.Stdout {
			t.Fatalf("output to %v", cmd.Stdout)
		}
		if in, ok := cmd.Stdin.(*os.File); !ok || in.Name() != os.DevNull {
			t.Fatalf("input from %v", cmd.Stdin)
		}
		if fi, err := os.Stat(log); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("sync.log %v %v", fi, err)
		}
		if b, _ := os.ReadFile(log); !strings.Contains(string(b), "an older run") {
			t.Fatal("the log was not appended to")
		}
	})
	t.Run("already running", func(t *testing.T) {
		m := newMachine(t, true)
		got := spawned(t)
		holdLock(t, m.dir, 4242)
		for _, args := range [][]string{{"sync"}, {"sync", "--background"}} {
			r := m.run("", args...)
			wantCode(t, r, 0)
			contains(t, "stdout", r.out, "already running")
		}
		r := m.run("", "sync", "--status")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "running (pid 4242)")
		if err := StartSync(); err != nil || len(*got) != 0 {
			t.Fatalf("a second copy was started: %v %d", err, len(*got))
		}
	})
	t.Run("signed out", func(t *testing.T) {
		m := newMachine(t, false)
		got := spawned(t)
		for _, args := range [][]string{{"sync"}, {"sync", "--background"}} {
			r := m.run("", args...)
			wantCode(t, r, 1)
			contains(t, "stderr", r.err, "not signed in")
		}
		if err := StartSync(); err != nil || len(*got) != 0 { // a hook never starts one signed out, never fails
			t.Fatalf("started signed out: %v %d", err, len(*got))
		}
	})
	t.Run("hooks start one", func(t *testing.T) {
		m := newMachine(t, true)
		got := spawned(t)
		if err := StartSync(); err != nil || len(*got) != 1 {
			t.Fatalf("%v %d", err, len(*got))
		}
		if fi, err := os.Stat(filepath.Join(m.dir, "sync.log")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("sync.log %v %v", fi, err)
		}
	})
}

// scheduledUpdate makes `teamflow sync` check at once against rel for a stand-in teamflow 0.1.0, and records the
// restart instead of making it (fail: the restart fails).
func scheduledUpdate(t *testing.T, rel *selfupdatetest.Release, fail error) (exe string, restarted *string) {
	t.Helper()
	t.Setenv(config.AutoUpdateEnv, "")
	exe = selfupdatetest.Installed(t, t.TempDir(), "0.1.0")
	var got string
	oldO, oldR := syncOptions, restartSync
	syncOptions = func() live.Options {
		return live.Options{Rescan: 20 * time.Millisecond, Logf: func(string, ...any) {},
			AutoUpdate: &live.AutoUpdate{Updater: rel.Updater(exe, nil), Current: "0.1.0", First: time.Nanosecond}}
	}
	restartSync = func(e string) error { got = e; return fail }
	t.Cleanup(func() { syncOptions, restartSync = oldO, oldR })
	return exe, &got
}

// After its scheduled update the background sync goes on as the new binary, in this process's place (the
// restart runs the binary now on disk: the new release); /teamf status will tell it once. Sad path: the restart
// fails (said, exit 1: the next agent hook starts the sync again).
func TestSyncRestartsOnTheNewBinary(t *testing.T) {
	m := newMachine(t, true)
	m.inSession("s1", "", nil)
	exe, restarted := scheduledUpdate(t, selfupdatetest.New(t, "v0.1.1", ""), nil)
	r := m.run("", "sync")
	wantCode(t, r, 0)
	if *restarted != exe {
		t.Fatalf("restarted %q, want %q (%s)", *restarted, exe, r)
	}
	if out, err := exec.Command(*restarted, "--version").Output(); err != nil || string(out) != "teamflow 0.1.1\n" {
		t.Fatalf("the restart runs %q %v", out, err)
	}
	if v, _ := m.store().TakeUpdatedTo(); v != "v0.1.1" {
		t.Fatalf("updated to %q", v)
	}
	if _, running := live.Status(m.dir); running {
		t.Fatal("the lock is still held")
	}

	m = newMachine(t, true)
	m.inSession("s1", "", nil)
	scheduledUpdate(t, selfupdatetest.New(t, "v0.1.1", ""), errors.New("exec format error"))
	r = m.run("", "sync")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "the background sync could not restart on the new binary: exec format error; the next agent hook starts it")
}
