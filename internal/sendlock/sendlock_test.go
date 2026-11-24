package sendlock

// Sad paths covered: a second holder (busy within its wait, in this process and in another), a release letting the
// next one in, a holder killed with SIGKILL (its lock goes with it), a cancelled wait, names that would leave the
// folder, a folder that does not exist, and the lock file's mode.

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOneHolderAtATime(t *testing.T) {
	dir := t.TempDir()
	release, err := Acquire(context.Background(), dir, "ab12", 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Acquire(context.Background(), dir, "ab12", 80*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second holder: %v, want ErrBusy", err)
	}
	if d := time.Since(start); d < 80*time.Millisecond || d > time.Second {
		t.Fatalf("waited %v, want about its wait", d)
	}
	other, err := Acquire(context.Background(), dir, "cd34", 0) // another outbox is not blocked
	if err != nil {
		t.Fatal(err)
	}
	other()
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	again, err := Acquire(context.Background(), dir, "ab12", 2*time.Second) // waits for the release
	if err != nil {
		t.Fatalf("after the release: %v", err)
	}
	again()
	again() // releasing twice is harmless
	fi, err := os.Stat(filepath.Join(dir, "send-ab12.lock"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
}

func TestCancelledWaitGivesUp(t *testing.T) {
	dir := t.TempDir()
	release, err := Acquire(context.Background(), dir, "ab12", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Acquire(ctx, dir, "ab12", 10*time.Second); !errors.Is(err, ErrBusy) || time.Since(start) > time.Second {
		t.Fatalf("cancelled: %v after %v", err, time.Since(start))
	}
}

func TestBadNamesAndFolders(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "../x", "a/b", "ab cd", ".", "AB12"} {
		if _, err := Acquire(context.Background(), dir, name, 0); err == nil || errors.Is(err, ErrBusy) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if _, err := Acquire(context.Background(), filepath.Join(dir, "missing"), "ab12", 0); err == nil {
		t.Fatal("a missing folder gave a lock")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("files left: %v", ents)
	}
}

// helper: TEST_SENDLOCK_HOLD=<dir> holds the lock ab12 and says "held", then sleeps until killed.
func TestMain(m *testing.M) {
	if dir := os.Getenv("TEST_SENDLOCK_HOLD"); dir != "" {
		if _, err := Acquire(context.Background(), dir, "ab12", 0); err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Another process holds the lock: busy. Killed with SIGKILL (no chance to release): its lock is gone at once, so a
// crashed holder never blocks the outbox.
func TestHolderInAnotherProcessAndItsDeath(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TEST_SENDLOCK_HOLD="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("helper said %q", line)
	}
	if _, err := Acquire(context.Background(), dir, "ab12", 100*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("held by another process: %v, want ErrBusy", err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	start := time.Now()
	release, err := Acquire(context.Background(), dir, "ab12", 2*time.Second)
	if err != nil {
		t.Fatalf("after the holder died: %v", err)
	}
	release()
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("the dead holder's lock took %v to go", time.Since(start))
	}
}
