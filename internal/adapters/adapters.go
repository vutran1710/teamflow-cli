// Package adapters is the registry of agent adapters. The core (CLI, store, sync, MCP server) is the same for
// every agent; an adapter adds only that agent's extras (registering the MCP server in the agent's config, its
// hooks, its /teamf command, the one instruction text where the agent reads instructions, a deny rule for the
// Teamflow home). Each supported agent's adapter registers itself here.
package adapters

import (
	"fmt"
	"slices"
	"sync"
)

// Adapter wires Teamflow into one agent.
type Adapter interface {
	// Name is the agent's name as `teamflow install -a` takes it (claude-code, cursor, antigravity, copilot).
	Name() string
	// Detect says whether the agent is installed on this machine.
	Detect() bool
	// Install sets Teamflow up in the agent, calling binary (an absolute path) from its hooks and MCP entry. It is
	// idempotent, and says what it wrote where.
	Install(binary string) ([]string, error)
	// Uninstall removes what Install added, leaving the person's own settings as they were.
	Uninstall() ([]string, error)
	// Installed says whether Teamflow is set up in the agent.
	Installed() bool
}

// Supported are the agents Teamflow supports, in the order they are listed.
var Supported = []string{"claude-code", "cursor", "antigravity", "copilot"}

// IsSupported says whether name is a supported agent.
func IsSupported(name string) bool { return slices.Contains(Supported, name) }

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
