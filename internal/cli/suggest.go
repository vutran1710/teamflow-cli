package cli

// Suggestions for a command that does not exist: an agent (or a person) that guesses a name gets the closest real
// ones, so the next try is right ("teamflow plan" -> "did you mean: teamflow project plan, teamflow project
// request-update").

import (
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// maxSuggestions is how many names a refusal offers.
const maxSuggestions = 3

type candidate struct{ path, help string }

// candidates are every command a person or agent may run (hidden ones left out), by path, with what it does.
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
			if c.hidden {
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
// a command with that very word in its path first, then one with a word a typo away, then one whose description
// names the word. Inside a group, its own commands come first.
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
			case len(w) >= 3 && (levenshtein(cw, w) <= 2 || strings.HasPrefix(cw, w)):
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
	for i, g := range got {
		if g.score%3 == 2 && exact[parent(g.path)] && changes(helpOf[g.path]) {
			got[i].score-- // a guessed noun usually means doing something to it: the commands that change it
		}
	}
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
