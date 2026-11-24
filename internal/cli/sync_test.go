package cli

// `teamflow sync [--background|--status|--stop]` and the start hooks use (StartSync): one copy per machine, started
// detached with its output in sync.log (0600), nothing secret in its arguments, never started signed out.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/live"
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
