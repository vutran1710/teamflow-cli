package adapters

// Finding the agent session a process runs under. Hooks record each session (by the id the agent gives them)
// against the agent's process; the MCP server the agent started, and any command run from its shell, walk up their
// own ancestors to the nearest process with a recorded session. Two agents started from nested shells are each the
// nearest to their own children, so each finds its own session.

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/procs"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// pidReuseSlack is how much later than its session was last seen an agent process may seem to have started (clock
// granularity) before its pid is taken as reused by another process.
const pidReuseSlack = 2 * time.Second

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 12

// Ancestry finds the session recorded against the nearest of this process's ancestors, never a dead agent's
// session whose pid now belongs to a process that started after the session was last seen.
type Ancestry struct{}

// Current is this process's agent session (false when none).
func (Ancestry) Current(st *store.Store) (store.Session, bool, error) {
	return st.SessionOfPIDs(procs.Ancestors(os.Getpid(), maxAncestors), func(s store.Session) bool {
		started, ok := procs.StartTime(s.AgentPID)
		return ok && !started.After(s.UpdatedAt.Add(pidReuseSlack))
	})
}

// AgentPID is the agent process a hook runs under: the pid in envPID (an agent marker the agent sets for its
// children) when it is one of the hook's ancestors, else the nearest ancestor whose command name starts with one
// of names, else the nearest ancestor that is not a shell (an agent may run hooks through sh -c), else the parent.
func AgentPID(envPID string, names ...string) int {
	chain := procs.Ancestors(os.Getpid(), maxAncestors)
	if envPID != "" {
		if p, err := strconv.Atoi(os.Getenv(envPID)); err == nil && p > 1 {
			for _, a := range chain {
				if a == p {
					return p
				}
			}
		}
	}
	for _, a := range chain {
		n := procs.Name(a)
		for _, name := range names {
			if strings.HasPrefix(n, name) {
				return a
			}
		}
	}
	for _, a := range chain {
		if !shells[strings.TrimPrefix(procs.Name(a), "-")] {
			return a
		}
	}
	if len(chain) > 0 {
		return chain[0]
	}
	return os.Getppid()
}

// shells are the command names of the shells an agent may start a hook through.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}
