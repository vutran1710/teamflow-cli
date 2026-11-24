// Package procs walks process ancestry: how a command finds the agent session it runs under. Hooks record each
// session against the agent's process id; a command run from the agent's shell, or the MCP server the agent
// started, walks up its own ancestors to that process (Python's tf_client.procs).
package procs

// Ancestors are pid's parent, grandparent, … (nearest first), at most limit of them, stopping before init.
func Ancestors(pid, limit int) []int {
	var out []int
	for cur := Parent(pid); cur > 1 && len(out) < limit; cur = Parent(cur) {
		out = append(out, cur)
	}
	return out
}
