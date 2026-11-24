package cli

// `teamflow sync`: the background sync (internal/live), one copy per machine. A setup command: never auto-allowed
// for agents. Install starts it, and so does every session start and prompt hook when it is not running (after a
// reboot, a restart, or its quiet exit).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/live"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// SyncLog is the background sync's output in the Teamflow home (0600). Nothing secret is written to it.
const SyncLog = "sync.log"

// syncLogMax: a sync.log past this is moved to sync.log.old when the next copy starts.
const syncLogMax = 1 << 20

// stopWait is how long --stop waits for the running copy to let go.
const stopWait = 10 * time.Second

var syncCmd = &command{
	name: "sync", help: "The background sync (one per machine): keeps the local replica live.",
	flags: []flagSpec{
		{name: "background", kind: flag, help: "Start it detached; its output goes to sync.log in the Teamflow home"},
		{name: "status", kind: flag, help: "Say whether it runs, and which projects are live"},
		{name: "stop", kind: flag, help: "Stop the running copy"},
	},
	nargs: exactly(0), run: runSync,
}

// spawn starts the detached copy and lets it go (replaced in tests).
var spawn = func(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func runSync(r *inv, a *parsed) int {
	n := 0
	for _, f := range []string{"background", "status", "stop"} {
		if a.has(f) {
			n++
		}
	}
	if n > 1 {
		fmt.Fprintln(r.errw, "usage: teamflow sync [--background|--status|--stop]\n\nError: choose one of --background, --status, --stop")
		return 2
	}
	dir, err := home.Ensure()
	if err != nil {
		return r.failErr(err)
	}
	switch {
	case a.has("status"):
		return syncStatus(r, dir)
	case a.has("stop"):
		stopped, err := live.Stop(dir, stopWait)
		if err != nil {
			return r.failErr(err)
		}
		if !stopped {
			r.println("the background sync is not running")
			return 0
		}
		r.println("the background sync stopped")
		return 0
	case a.has("background"):
		started, err := startBackground(dir)
		if err != nil {
			return r.failErr(err)
		}
		if !started {
			r.println("the background sync is already running")
			return 0
		}
		r.printf("the background sync started (its log: %s)\n", home.Display(filepath.Join(dir, SyncLog)))
		return 0
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	if !c.SignedIn() {
		return r.failErr(core.ErrNotSignedIn)
	}
	ctx, stop := signal.NotifyContext(r.ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	if err := live.Run(ctx, c, live.Options{}); err != nil {
		if errors.Is(err, live.ErrRunning) {
			r.println("the background sync is already running")
			return 0
		}
		return r.failErr(err)
	}
	return 0
}

func syncStatus(r *inv, dir string) int {
	pid, ok := live.Status(dir)
	if !ok {
		r.println("the background sync is not running")
		return 0
	}
	r.printf("the background sync is running (pid %d)\n", pid)
	c, err := r.core()
	if err != nil || !c.SignedIn() {
		return 0
	}
	projects, err := c.KeyProjects()
	if err != nil {
		return 0
	}
	now := time.Now()
	b := store.Binding(c.Cfg.Server, c.Cfg.Email)
	for _, p := range projects {
		_, _, _, name, _ := c.St.ProjectKey(b, p)
		at := c.St.LiveAt(p)
		switch {
		case c.LiveFresh(p):
			r.printf("  %s (%s): live, heard %ds ago\n", p, name, int(now.Sub(at).Seconds()))
		case at.IsZero():
			r.printf("  %s (%s): not live yet\n", p, name)
		default:
			r.printf("  %s (%s): not live (last heard %s)\n", p, name, at.Local().Format(time.RFC3339))
		}
	}
	return 0
}

// StartSync starts the background sync detached when it is not running and the machine is signed in; otherwise it
// does nothing. Install, session start and prompt hooks call it (adapters.StartSync).
func StartSync() error {
	dir, err := home.Ensure()
	if err != nil {
		return err
	}
	_, err = startBackground(dir)
	if errors.Is(err, core.ErrNotSignedIn) {
		return nil
	}
	return err
}

// startBackground launches `teamflow sync` in a session of its own (setsid), stdin from /dev/null and its output
// appended to sync.log (0600). Nothing secret is in its arguments: it reads its keys from the store. False when one
// already runs.
func startBackground(dir string) (bool, error) {
	if _, running := live.Status(dir); running {
		return false, nil
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return false, err
	}
	if !cfg.SignedIn() {
		return false, core.ErrNotSignedIn
	}
	bin, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("start the background sync: %w", err)
	}
	path := filepath.Join(dir, SyncLog)
	if fi, err := os.Stat(path); err == nil && fi.Size() > syncLogMax {
		_ = os.Rename(path, path+".old")
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return false, fmt.Errorf("start the background sync: %w", err)
	}
	_ = out.Chmod(0o600)
	in, err := os.Open(os.DevNull)
	if err != nil {
		_ = out.Close()
		return false, fmt.Errorf("start the background sync: %w", err)
	}
	cmd := exec.Command(bin, "sync")
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = spawn(cmd)
	_ = in.Close()
	_ = out.Close()
	if err != nil {
		return false, fmt.Errorf("start the background sync: %w", err)
	}
	return true, nil
}
