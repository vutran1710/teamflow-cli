package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
)

// A guessed command name is refused (exit 2) with the closest real ones, so the next try is right.
func TestUnknownCommandSuggestsTheClosest(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"plan"}, []string{"teamflow project plan", "teamflow project request-update"}},
		{[]string{"project", "plans"}, []string{"teamflow project plan"}},
		{[]string{"projct"}, []string{"teamflow project"}},
		{[]string{"signal", "emitt"}, []string{"teamflow signal emit"}},
		{[]string{"deliverables", "publsh"}, []string{"teamflow deliverables publish"}},
		{[]string{"aprove"}, []string{"teamflow approve"}},
	} {
		var out, errw strings.Builder
		code := Main(tc.args, strings.NewReader(""), &out, &errw)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", tc.args, code)
		}
		line := ""
		for _, l := range strings.Split(errw.String(), "\n") {
			if strings.HasPrefix(l, "Did you mean: ") {
				line = l
			}
		}
		for _, w := range tc.want {
			if !strings.Contains(line, w) {
				t.Errorf("%v: %q does not suggest %q\n%s", tc.args, line, w, errw.String())
			}
		}
		if n := strings.Count(line, "teamflow "); n > maxSuggestions {
			t.Errorf("%v: %d suggestions", tc.args, n)
		}
	}
}

// Nothing close: the refusal stands alone. Hidden commands (hook, debug) and setup commands (login, sync, outbox…)
// are never offered to a guess; a short word is matched only one letter away.
func TestUnknownCommandSadPaths(t *testing.T) {
	for _, args := range [][]string{{"zzzzqqq"}, {"project", "xqzw"}, {"-x"}, {"foo"}} {
		var out, errw strings.Builder
		if code := Main(args, strings.NewReader(""), &out, &errw); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
		if strings.Contains(errw.String(), "Did you mean") {
			t.Errorf("%v: suggested something for nothing close:\n%s", args, errw.String())
		}
	}
	for _, w := range []string{"hook", "debug", "mcp", "login", "logout", "sync", "outbox", "install", "update"} {
		for _, s := range suggest("", w) {
			if slices.Contains(adapters.SetupCommands, strings.Fields(s)[0]) || strings.Fields(s)[0] == "mcp" {
				t.Errorf("%q suggested %q, a hidden or setup command", w, s)
			}
		}
	}
	if got := suggest("", "plan"); slices.Contains(got, "artifact archive") {
		t.Errorf("plan: %v (artifact archive is no typo of it)", got)
	}
	if s := suggest("", ""); s != nil {
		t.Errorf("an empty word: %v", s)
	}
}
