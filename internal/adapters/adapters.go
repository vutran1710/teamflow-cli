// Package adapters is the registry of agent adapters. The core (the teamflow CLI, store and sync) is the same for
// every agent; an adapter adds only that agent's extras (its hooks, its /teamf command, the one instruction text
// where the agent reads instructions, an allow rule for teamflow work commands and a deny rule for the Teamflow
// home). Each supported agent's adapter registers itself here.
package adapters

import (
	"fmt"
	"slices"
	"sync"

	"github.com/vutran1710/teamflow-cli/internal/core"
)

// Adapter wires Teamflow into one agent.
type Adapter interface {
	// Name is the agent's name as `teamflow install -a` takes it (claude-code, cursor, antigravity, copilot).
	Name() string
	// Detect says whether the agent is installed on this machine.
	Detect() bool
	// Install sets Teamflow up in the agent, calling binary (an absolute path) from its hooks and rules. It is
	// idempotent, and says what it wrote where.
	Install(binary string) ([]string, error)
	// Uninstall removes what Install added, leaving the person's own settings as they were.
	Uninstall() ([]string, error)
	// Installed says whether Teamflow is set up in the agent.
	Installed() bool
	// Hook answers one of the agent's hook calls: event is the agent's own event name, stdin what the agent sent.
	// It maps the agent's events to Teamflow's (session start, before each prompt, before a tool, the turn's end,
	// session end) and returns what the hook prints for the agent (nil: nothing). Run it through RunHook, which
	// never lets an error fail the agent's step.
	Hook(event string, stdin []byte, c *core.Core) ([]byte, error)
	// SessionID is the agent's session id in a hook's input.
	SessionID(stdin []byte) (string, error)
}

// Quick is an adapter that answers some events without the Teamflow home (no store, no config): a permission
// answer (allow a teamflow command, deny a tool call naming the home) must never depend on the home opening.
// handled false: the event goes to Hook as usual.
type Quick interface {
	Quick(event string, stdin []byte) (out []byte, handled bool)
}

// Supported are the agents Teamflow supports, in the order they are listed.
var Supported = []string{"claude-code", "cursor", "antigravity", "copilot"}

// IsSupported says whether name is a supported agent.
func IsSupported(name string) bool { return slices.Contains(Supported, name) }

// Experimental are the supported agents not yet verified on a real install: set up only when named (`-a cursor`),
// with a warning; never by `-a all` or picked from the agents found on the machine.
var Experimental = []string{"cursor", "antigravity"}

// ExperimentalNote is what is said about an experimental agent.
const ExperimentalNote = "experimental: not yet verified on a real install"

// IsExperimental says whether name is an experimental agent.
func IsExperimental(name string) bool { return slices.Contains(Experimental, name) }

var (
	mu       sync.Mutex
	registry = map[string]Adapter{}
)

// Register adds an agent's adapter. Only a supported agent may register; a second adapter for it replaces the first.
func Register(a Adapter) {
	if !IsSupported(a.Name()) {
		panic(fmt.Sprintf("adapters: %q is not a supported agent", a.Name()))
	}
	mu.Lock()
	defer mu.Unlock()
	registry[a.Name()] = a
}

// Get is the adapter for an agent, when this build has one.
func Get(name string) (Adapter, bool) {
	mu.Lock()
	defer mu.Unlock()
	a, ok := registry[name]
	return a, ok
}

// All are the registered adapters, in the order of Supported.
func All() []Adapter {
	mu.Lock()
	defer mu.Unlock()
	var out []Adapter
	for _, n := range Supported {
		if a, ok := registry[n]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Reset forgets every adapter (for tests).
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	registry = map[string]Adapter{}
}
