package cli

// Suggestions for a command that does not exist: an agent (or a person) that guesses a name gets the closest real
// ones, so the next try is right ("teamflow plan" -> "did you mean: teamflow project plan, teamflow project
// request-update").

import (
	"slices"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// maxSuggestions is how many names a refusal offers.
const maxSuggestions = 3

type candidate struct{ path, help string }

// candidates are the commands a guess may mean, by path, with what they do: never hidden ones, nor setup commands
// (signing in, installing, the background sync, the outbox): those are the person's to choose, never a correction.
func candidates() []candidate {
	var out []candidate
	seen := map[string]bool{}
	add := func(path, help string) {
		if !seen[path] {
			seen[path] = true
			out = append(out, candidate{path, help})
		}
	}
	var walk func(prefix string, cs []*command)
	walk = func(prefix string, cs []*command) {
		for _, c := range cs {
			if c.hidden || (prefix == "" && slices.Contains(adapters.SetupCommands, c.name)) {
				continue
			}
			path := strings.TrimSpace(prefix + " " + c.name)
			help := c.help
			if c.spec != nil {
				help += " " + c.spec.Description
			}
			add(path, help)
			walk(path, c.sub)
		}
	}
	walk("", commands)
	for _, s := range protocol.AgentCommands {
		add(s.Name, s.Description)
	}
	return out
}

// suggest is up to maxSuggestions real command paths closest to an unknown word typed after prefix ("" at the top):
// a command with that very word in its path first, then one with a word a typo away; a command whose description
// names the word only beside an exact match in its group, and only when it changes it. Inside a group, its own
// commands come first.
func suggest(prefix, typed string) []string {
	w := strings.ToLower(strings.TrimSpace(typed))
	if w == "" || strings.HasPrefix(w, "-") {
		return nil
	}
	type scored struct {
		path  string
		score int
	}
	var got []scored
	helpOf := map[string]string{}
	for _, c := range candidates() {
		helpOf[c.path] = c.help
		words := strings.Fields(c.path)
		score := -1
		for _, cw := range words {
			switch {
			case cw == w:
				score = best(score, 0)
			case len(w) >= 3 && (levenshtein(cw, w) <= typos(w) || strings.HasPrefix(cw, w)):
				score = best(score, 1)
			}
		}
		if score < 0 && len(w) >= 4 {
			for _, hw := range strings.FieldsFunc(strings.ToLower(c.help), func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r == '-')
			}) {
				if hw == w {
					score = 2
					break
				}
			}
		}
		if score < 0 {
			continue
		}
		if prefix != "" && !strings.HasPrefix(c.path+" ", prefix+" ") {
			score += 3 // another group's command: after this group's own
		}
		got = append(got, scored{c.path, score})
	}
	// A command whose description names the word, in the same group as one named by it, comes next to it
	// ("plan": project plan, then project request-update, which rewrites the plan).
	exact := map[string]bool{}
	for _, g := range got {
		if g.score%3 == 0 {
			exact[parent(g.path)] = true
		}
	}
	kept := got[:0]
	for _, g := range got {
		if g.score%3 == 2 { // named only in its description: kept beside an exact match in its group, if it changes it
			if !exact[parent(g.path)] || !changes(helpOf[g.path]) {
				continue
			}
			g.score-- // a guessed noun usually means doing something to it
		}
		kept = append(kept, g)
	}
	got = kept
	var out []string
	for s := 0; s <= 5 && len(out) < maxSuggestions; s++ {
		for _, g := range got {
			if g.score == s && len(out) < maxSuggestions {
				out = append(out, g.path)
			}
		}
	}
	return out
}

// changes says a command's description is of one that changes something (rewrite, update, publish…).
func changes(help string) bool {
	for _, w := range strings.Fields(strings.ToLower(help)) {
		switch strings.Trim(w, ".,;:()") {
		case "rewrite", "update", "write", "change", "publish", "replace", "set":
			return true
		}
	}
	return false
}

func parent(path string) string {
	if i := strings.LastIndex(path, " "); i >= 0 {
		return path[:i]
	}
	return ""
}

// typos is how far a word may be from a command's to be a typo of it: 1 for short words (no "log" for "foo"),
// 2 from five letters.
func typos(w string) int {
	if len(w) <= 4 {
		return 1
	}
	return 2
}

func best(a, b int) int {
	if a < 0 || b < a {
		return b
	}
	return a
}

// didYouMean is the line a refusal of an unknown command ends with ("" when nothing is close).
func didYouMean(prefix, typed string) string {
	s := suggest(prefix, typed)
	if len(s) == 0 {
		return ""
	}
	return "Did you mean: teamflow " + strings.Join(s, ", teamflow ") + "?\n"
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
