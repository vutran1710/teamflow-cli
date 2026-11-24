// Package cli holds the teamflow commands.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// State is what `teamflow debug state --json` prints: the whole store, keys left out (see store.Dump).
type State = store.Dump

const debugUsage = "usage: teamflow debug state --json\n"

// Debug runs the hidden `teamflow debug` command; `state --json` prints the store as JSON for the e2e harness.
// It is not in the usage text. Exit 0 on success, 1 when the store can't be read, 2 on a usage error.
func Debug(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "state" || args[1] != "--json" {
		fmt.Fprint(stderr, debugUsage)
		return 2
	}
	dir, err := home.Ensure()
	if err != nil {
		fmt.Fprintf(stderr, "teamflow: %v\n", err)
		return 1
	}
	st, err := store.Open(dir)
	if err != nil {
		fmt.Fprintf(stderr, "teamflow: %v\n", err)
		return 1
	}
	defer st.Close()
	state, err := st.Dump()
	if err != nil {
		fmt.Fprintf(stderr, "teamflow: read the store: %v\n", err)
		return 1
	}
	for i := range state.Sessions { // never a real home path: "~/…"
		if cwd := state.Sessions[i].Cwd; cwd != "" {
			state.Sessions[i].Cwd = home.Display(filepath.Clean(cwd))
		}
	}
	out, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "teamflow: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(append(out, '\n'))
	return 0
}
