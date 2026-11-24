// Package sendlock is the outbox's sender lock: one sender at a time per project key, across every process on the
// machine (commands, hooks, the background sync). It is an flock(2) on a file in the Teamflow home, so a holder
// that dies, even by SIGKILL, lets go at once: the kernel drops the lock with the process. A waiter waits at most
// its wait, then gives up (ErrBusy) and leaves its items queued for the holder.
package sendlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrBusy is another sender holding the lock past the wait.
var ErrBusy = errors.New("another teamflow is sending this project's queued signals")

// poll is how often a waiter tries again.
const poll = 10 * time.Millisecond

// Acquire takes the lock name (lower-case hex: a key ref) in dir, waiting up to wait (0: one try). It returns the
// release, which is safe to call more than once.
func Acquire(ctx context.Context, dir, name string, wait time.Duration) (func(), error) {
	if !validName(name) {
		return nil, fmt.Errorf("sender lock: bad name %q", name)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("sender lock: no folder %s", filepath.Base(dir))
	}
	f, err := os.OpenFile(filepath.Join(dir, "send-"+name+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sender lock: %w", err)
	}
	_ = f.Chmod(0o600)
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
					_ = f.Close()
				})
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("sender lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ErrBusy
		case <-time.After(poll):
		}
	}
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}
