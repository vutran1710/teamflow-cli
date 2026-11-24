package adapters

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/procs"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

type hookAdapter struct {
	fakeAdapter
	hook func(event string, stdin []byte, c *core.Core) ([]byte, error)
}

func (h *hookAdapter) Hook(event string, stdin []byte, c *core.Core) ([]byte, error) {
	return h.hook(event, stdin, c)
}

func tempHome(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	must(t, os.Mkdir(dir, 0o700))
	t.Setenv("TEAMFLOW_HOME", dir)
	return dir
}

// A hook never fails the agent's step: its answer passes through; an error, a panic, a busy store or a home that
// can't be opened give no answer at all, and are logged to hook.log (owner-only) without the agent's input (a
// prompt can be private).
func TestRunHookNeverFails(t *testing.T) {
	dir := tempHome(t)
	ok := &hookAdapter{fakeAdapter: fakeAdapter{name: "claude-code"}, hook: func(e string, in []byte, c *core.Core) ([]byte, error) {
		if c == nil || c.St == nil {
			t.Fatal("no client")
		}
		return []byte(`{"x": 1}`), nil
	}}
	if got := RunHook(ok, "SessionStart", []byte(`{}`), core.Open); string(got) != `{"x": 1}` {
		t.Fatalf("answer %q", got)
	}
	secret := "my private prompt"
	for name, fn := range map[string]func(string, []byte, *core.Core) ([]byte, error){
		"an error":     func(string, []byte, *core.Core) ([]byte, error) { return []byte("half"), errors.New("boom") },
		"a busy store": func(string, []byte, *core.Core) ([]byte, error) { return nil, store.ErrBusy },
		"a panic":      func(string, []byte, *core.Core) ([]byte, error) { panic("kaboom") },
	} {
		t.Run(name, func(t *testing.T) {
			a := &hookAdapter{fakeAdapter: fakeAdapter{name: "claude-code"}, hook: fn}
			if got := RunHook(a, "UserPromptSubmit", []byte(`{"prompt": "`+secret+`"}`), core.Open); got != nil {
				t.Fatalf("answered %q", got)
			}
		})
	}
	log := filepath.Join(dir, "hook.log")
	b, err := os.ReadFile(log)
	must(t, err)
	for _, want := range []string{"boom", "busy", "kaboom", "claude-code UserPromptSubmit"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("hook.log lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("hook.log holds the agent's input")
	}
	if fi, _ := os.Stat(log); fi.Mode().Perm() != 0o600 {
		t.Fatalf("hook.log mode %v", fi.Mode())
	}

	// The home can't be opened (open to others): no answer, no crash.
	must(t, os.Chmod(dir, 0o755))
	if got := RunHook(ok, "SessionStart", nil, core.Open); got != nil {
		t.Fatalf("answered with a bad home: %q", got)
	}
	must(t, os.Chmod(dir, 0o700))
	// Opening fails outright.
	if got := RunHook(ok, "SessionStart", nil, func() (*core.Core, error) { return nil, errors.New("no") }); got != nil {
		t.Fatalf("answered %q", got)
	}
}

// The session a process runs under is the one recorded against its nearest ancestor (two agents in nested shells
// each find their own), never a dead agent's whose pid now belongs to a newer process, and none when nothing is
// recorded.
func TestAncestryFindsTheNearestSession(t *testing.T) {
	dir := tempHome(t)
	st, err := store.Open(dir)
	must(t, err)
	defer st.Close()
	var f Ancestry
	if _, ok, err := f.Current(st); err != nil || ok {
		t.Fatalf("nothing recorded: %v %v", ok, err)
	}
	parent := os.Getppid()
	grand := ancestorsOf(t)[1]
	now := time.Now()
	_, err = st.UpsertSession("outer", "claude-code", grand, "", now)
	must(t, err)
	s, ok, err := f.Current(st)
	if err != nil || !ok || s.ID != "outer" {
		t.Fatalf("outer agent: %v %v %v", s.ID, ok, err)
	}
	_, err = st.UpsertSession("inner", "claude-code", parent, "", now)
	must(t, err)
	if s, ok, _ := f.Current(st); !ok || s.ID != "inner" {
		t.Fatalf("the nested agent's process finds its own session: %v", s.ID)
	}
	// A session seen before its pid's process started is a dead agent's: skipped for the next ancestor.
	_, err = st.UpsertSession("inner", "claude-code", parent, "", time.Unix(1_000_000_000, 0))
	must(t, err)
	if s, ok, _ := f.Current(st); !ok || s.ID != "outer" {
		t.Fatalf("a reused pid was taken: %v", s.ID)
	}
}

// StartSync is called on session start when the build has a background sync; its failure never fails the hook.
func TestBackgroundSyncIsTolerated(t *testing.T) {
	old := StartSync
	defer func() { StartSync = old }()
	StartSync = nil
	EnsureSync() // absent: nothing
	called := 0
	StartSync = func() error { called++; return errors.New("not in this build") }
	EnsureSync()
	if called != 1 {
		t.Fatalf("called %d", called)
	}
}

func ancestorsOf(t *testing.T) []int {
	t.Helper()
	a := procs.Ancestors(os.Getpid(), 12)
	if len(a) < 2 {
		t.Skip("fewer than two ancestors")
	}
	return a
}
