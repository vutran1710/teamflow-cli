package adapters

// Running an agent's hook: whatever goes wrong in Teamflow (the store busy, offline, signed out, an old client, a
// bug), the agent's step goes on. The hook answers nothing and the trouble is logged to hook.log in the Teamflow
// home, without the agent's input (a prompt can be private).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/home"
)

// StartSync starts the background sync when this build has one (set by the CLI; nil: none). It is called at every
// session start, so a sync stopped by a reboot or a restart starts again; one already running is left alone.
var StartSync func() error

// EnsureSync starts the background sync if the build has one; a failure is logged, never raised.
func EnsureSync() {
	if StartSync == nil {
		return
	}
	if err := StartSync(); err != nil {
		hookLog("sync", "start", err)
	}
}

// RunHook runs one hook of an agent's adapter with a client opened from disk (every credential and state reloaded:
// a hook is a fresh process). It returns what the hook prints for the agent; nil on any trouble.
func RunHook(a Adapter, event string, stdin []byte, open func() (*core.Core, error)) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			hookLog(a.Name(), event, fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
			out = nil
		}
	}()
	if q, ok := a.(Quick); ok {
		if out, handled := q.Quick(event, stdin); handled {
			return out
		}
	}
	c, err := open()
	if err != nil {
		hookLog(a.Name(), event, err)
		return nil
	}
	defer c.Close()
	out, err = a.Hook(event, stdin, c)
	if err != nil {
		hookLog(a.Name(), event, err)
		return nil
	}
	return out
}

// hookLog appends one trouble to hook.log (0600) in the Teamflow home; never fails.
func hookLog(agent, event string, err error) {
	dir := home.Dir()
	if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
		return
	}
	f, ferr := os.OpenFile(filepath.Join(dir, "hook.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	fmt.Fprintf(f, "%s %s %s: %v\n", time.Now().UTC().Format(time.RFC3339), agent, event, err)
}
