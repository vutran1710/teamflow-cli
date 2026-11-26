package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/config"
)

// failingAdapter refuses its install (as a malformed config file of the agent's own would).
type failingAdapter struct{ fakeAdapter }

func (f *failingAdapter) Install(string) ([]string, error) {
	return nil, errors.New("its hooks.json is not valid JSON")
}

// promptAgents registers Claude Code (found), Copilot (not found), Cursor (found, experimental) and Antigravity.
func promptAgents(t *testing.T) (cc, co, cu, ag *fakeAdapter) {
	t.Helper()
	adapters.Reset()
	cc, co = &fakeAdapter{name: "claude-code", detected: true}, &fakeAdapter{name: "copilot"}
	cu, ag = &fakeAdapter{name: "cursor", detected: true}, &fakeAdapter{name: "antigravity"}
	for _, a := range []*fakeAdapter{cc, co, cu, ag} {
		adapters.Register(a)
	}
	t.Cleanup(resetAdapters)
	return
}

// keys makes the terminal take keys one by one (raw mode), counting how often it was put back.
func keys(t *testing.T) *int {
	t.Helper()
	restored := new(int)
	old := rawMode
	rawMode = func() (func(), error) { return func() { *restored++ }, nil }
	t.Cleanup(func() { rawMode = old })
	return restored
}

// nothingDone fails the test when anything was set up or signed in.
func nothingDone(t *testing.T, m *machine, as ...*fakeAdapter) {
	t.Helper()
	noCall(t, m.srv, "auth.login")
	for _, a := range as {
		if a.installed {
			t.Fatalf("%s was set up", a.name)
		}
	}
	if cfg, _ := config.Load(m.dir); cfg != nil && cfg.AuthKey != "" {
		t.Fatal("signed in")
	}
}

// No -e and no -a at a terminal: the email, then the agents (by number where the terminal can't do raw mode),
// then the password; then the same install as the flags, ending in the welcome.
func TestInstallAsksAtATerminal(t *testing.T) {
	cc, co, cu, _ := promptAgents(t)
	m := newMachine(t, false)
	m.srv.on("auth.login", "auth.login")
	terminal(t, true, "pw")
	r := m.run("lan@acme.test\n\n", "install", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stderr", r.err, "Email: ", pickQuestion, "  1) Claude Code (found on this machine)\n",
		"  2) Copilot CLI\n", "  3) Cursor CLI (experimental)\n", "  4) Antigravity CLI (experimental)\n",
		"Numbers, separated by commas [1]: ", "Password: ")
	if !cc.installed || co.installed || cu.installed {
		t.Fatal("the default is the verified agents found here")
	}
	if got := onlyCall(t, m.srv, "auth.login"); !strings.Contains(string(got.Params), `"lan@acme.test"`) {
		t.Fatalf("signed in as %s", got.Params)
	}
	contains(t, "stdout", r.out, "You're all set, lan! Welcome to Acme.\n")

	// Signed in already: the email defaults to that account (no password asked again); "1,2" picks two.
	co.installed, cc.installed = false, false
	r = m.run("\n1,2\n", "install", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stderr", r.err, "Email [lan@acme.test]: ")
	lacks(t, "stderr", r.err, "Password")
	onlyCall(t, m.srv, "auth.login")
	if !cc.installed || !co.installed {
		t.Fatal("1,2 sets up Claude Code and Copilot CLI")
	}
	contains(t, "stdout", r.out, "  ✓ Claude Code is ready\n  ✓ Copilot CLI is ready\n")
}

// Sad paths of the email: not an email, empty with no default; each asked again, nothing done meanwhile.
func TestInstallAsksAgainForABadEmail(t *testing.T) {
	cc, _, _, _ := promptAgents(t)
	m := newMachine(t, false)
	m.srv.on("auth.login", "auth.login")
	terminal(t, true, "pw")
	r := m.run("lan\n\nlan @acme.test\nlan@acme.test\n1\n", "install", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stderr", r.err, `"lan" doesn't look like an email; try again.`,
		"Your email, please (the one you sign in to Teamflow with).", `"lan @acme.test" doesn't look like an email`)
	if strings.Count(r.err, "Email: ") != 4 || !cc.installed {
		t.Fatalf("asked %d times\n%s", strings.Count(r.err, "Email: "), r.err)
	}
	for _, bad := range []string{"lan@", "@acme.test", "lan@acme", "lan@acme.", "a@b@c.d", "lan@.test"} {
		if emailPattern.MatchString(bad) {
			t.Fatalf("%q passed as an email", bad)
		}
	}
}

// Sad paths of the numbered list: none chosen (nothing found, so no default), a number out of range, a word.
func TestInstallAsksAgainForAgents(t *testing.T) {
	cc, co, cu, _ := promptAgents(t)
	cc.detected, cu.detected = false, false
	m := newMachine(t, true)
	terminal(t, true, "")
	r := m.run("lan@acme.test\n\n9\ntwo\n3\n", "install", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stderr", r.err, "Numbers, separated by commas: ", "Choose at least one agent.",
		"Not an agent number: 9; choose from 1 to 4.", "Not an agent number: two;",
		"cursor: experimental: not yet verified on a real install")
	if cc.installed || co.installed || !cu.installed {
		t.Fatal("3 is Cursor CLI")
	}
	contains(t, "stdout", r.out, "✓ Cursor CLI is ready (experimental: tell us if anything feels off)")
}

// The arrow-key list: found agents come chosen; space unchooses, enter with none chosen asks again, down twice
// and space chooses Cursor CLI (experimental: warned), enter confirms; the terminal is put back.
func TestInstallPicksAgentsWithKeys(t *testing.T) {
	cc, co, cu, ag := promptAgents(t)
	m := newMachine(t, true)
	terminal(t, true, "")
	restored := keys(t)
	r := m.run("lan@acme.test\n \r\x1b[B\x1b[B \x1bOB\x1b[A\x1b[Z\rj", "install", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stderr", r.err, "(↑/↓ move, space choose, enter confirm)", "[\x1b[32mx\x1b[0m] Claude Code",
		"found on this machine", "Choose at least one agent: move to it and press space.",
		pickQuestion+" Cursor CLI\r\n", "cursor: experimental: not yet verified on a real install")
	if cc.installed || co.installed || !cu.installed || ag.installed || *restored != 1 {
		t.Fatalf("installed %v %v %v %v, restored %d", cc.installed, co.installed, cu.installed, ag.installed, *restored)
	}
	if !strings.HasSuffix(strings.Split(r.err, pickQuestion+" Cursor CLI\r\n")[0], "\x1b[J") {
		t.Fatal("the list is not cleared before the answer")
	}
}

// Stopping at any question sets nothing up: EOF at the email, at the numbers, Ctrl-C and Ctrl-D in the list, EOF
// in the list. The terminal is always put back.
func TestInstallAbortsCleanly(t *testing.T) {
	cc, co, cu, ag := promptAgents(t)
	m := newMachine(t, false)
	terminal(t, true, "pw")
	for _, in := range []string{"", "lan", "lan@acme.test\n"} {
		r := m.run(in, "install", "-s", m.srv.URL())
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "Aborted: nothing was set up.")
		nothingDone(t, m, cc, co, cu, ag)
	}
	restored := keys(t)
	for _, in := range []string{"lan@acme.test\n\x03", "lan@acme.test\n\x1b[B \x04", "lan@acme.test\n \x1b[B"} {
		r := m.run(in, "install", "-s", m.srv.URL())
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "Aborted: nothing was set up.", "\x1b[?25h") // the cursor shown again
		nothingDone(t, m, cc, co, cu, ag)
	}
	if *restored != 3 {
		t.Fatalf("the terminal was put back %d times of 3", *restored)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "config.json")); err == nil {
		t.Fatal("a config was written")
	}
}

// Flags skip their question; with no terminal (or --json, --password-stdin, or stderr not a terminal) a missing
// flag is refused as before, never asked and never waited on.
func TestInstallAsksOnlyWhatIsMissing(t *testing.T) {
	cc, co, _, _ := promptAgents(t)
	m := newMachine(t, true)
	terminal(t, true, "")
	r := m.run("1,2\n", "install", "-e", "lan@acme.test", "-s", m.srv.URL())
	wantCode(t, r, 0)
	lacks(t, "stderr", r.err, "Email")
	contains(t, "stderr", r.err, pickQuestion)
	if !cc.installed || !co.installed {
		t.Fatal("1,2")
	}
	r = m.run("lan@acme.test\n", "install", "-a", "copilot", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stderr", r.err, "Email [lan@acme.test]: ")
	lacks(t, "stderr", r.err, pickQuestion)
	r = m.run("", "install", "-a", "copilot", "-e", "lan@acme.test", "-s", m.srv.URL())
	wantCode(t, r, 0)
	lacks(t, "stderr", r.err, "Email", pickQuestion)

	for _, extra := range [][]string{{"--json"}, {"--password-stdin"}} {
		r = m.run("lan@acme.test\n1\n", append([]string{"install", "-s", m.srv.URL()}, extra...)...)
		wantCode(t, r, 2)
		contains(t, "stderr", r.err, "Missing option '-e' / '--email'")
		r = m.run("pw\n", append([]string{"install", "-e", "lan@acme.test", "-s", m.srv.URL()}, extra...)...)
		wantCode(t, r, 1)
		contains(t, "stdout+stderr", r.out+r.err, "say which agents to set up: -a <agent>")
		lacks(t, "stderr", r.err, pickQuestion)
	}
	old := errTerminal
	errTerminal = func() bool { return false }
	r = m.run("lan@acme.test\n1\n", "install", "-s", m.srv.URL())
	errTerminal = old
	wantCode(t, r, 2)
	lacks(t, "stderr", r.err, "Email:")
	terminal(t, false, "")
	r = m.run("lan@acme.test\n1\n", "install", "-s", m.srv.URL())
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "Missing option '-e' / '--email'")
	r = m.run("1\n", "install", "-e", "lan@acme.test", "-s", m.srv.URL())
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "say which agents to set up: -a <agent> (repeatable), or -a all")
}

// The welcome, word for word: one agent; several (an experimental one noted); a failed one owned up to beside
// one that worked; no display name; several orgs; no org known (the server unreachable after the install).
func TestInstallWelcome(t *testing.T) {
	const middle = "\nFrom now on your agent quietly keeps your team in the loop as you work,\n" +
		"and the calls that need you reach you right away.\n\n" +
		"Next: start a session and ask your agent which project to work on,\nor type /teamf project.\n" +
		"Your team's work is also in the web app: https://teamflow.10l.ink/ui/\n\n"
	srv := "https://teamflow.10l.ink"
	cases := []struct {
		who          account
		done, failed []string
		want         string
	}{
		{account{"Lan Nguyen", []string{"Acme"}}, []string{"claude-code"}, nil,
			"\nYou're all set, Lan! Welcome to Acme.\n\n  ✓ Claude Code is ready\n" + middle +
				"Glad you're here. Happy building!\n"},
		{account{"Lan", []string{"Acme", "Beta", "Gamma"}}, []string{"claude-code", "copilot", "cursor"}, nil,
			"\nYou're all set, Lan! Welcome to Acme, Beta and Gamma.\n\n  ✓ Claude Code is ready\n" +
				"  ✓ Copilot CLI is ready\n  ✓ Cursor CLI is ready (experimental: tell us if anything feels off)\n" + middle +
				"Glad you're here. Happy building!\n"},
		{account{"", []string{"Acme"}}, []string{"copilot"}, nil,
			"\nYou're all set! Welcome to Acme.\n\n  ✓ Copilot CLI is ready\n" + middle +
				"Glad you're here. Happy building!\n"},
		{account{"Lan", nil}, []string{"claude-code"}, []string{"cursor", "antigravity"},
			"\nAlmost there, Lan! You're in teamflow.10l.ink.\n\n  ✓ Claude Code is ready\n" +
				"  ✗ Cursor CLI isn't set up yet (the reason is above)\n" +
				"  ✗ Antigravity CLI isn't set up yet (the reason is above)\n" + middle +
				"To finish, fix what's said above about Cursor CLI and Antigravity CLI and run teamflow install again.\n"},
	}
	for _, c := range cases {
		if got := welcome(c.who, srv, c.done, c.failed); got != c.want {
			t.Fatalf("welcome(%v, %v, %v):\n%s\nwant:\n%s", c.who, c.done, c.failed, got, c.want)
		}
	}
}

// Through install: the account with no display name is greeted without one; the server unreachable after the
// sign-in falls back to the sign-in's name; a failed agent beside one set up: warm, honest, exit 1.
func TestInstallWelcomeThroughInstall(t *testing.T) {
	cc, _, _, _ := promptAgents(t)
	m := newMachine(t, true)
	m.srv.on("account.me", `{"result": {"account": {"id": "ac_00000000000000000000000001", "email": "lan@acme.test", "name": "", "alias": "", "avatar": null, "created_at": "2026-01-15T09:00:00Z", "disabled_at": null}, "orgs": [], "protocol": 1, "min_client_protocol": 1}}`)
	r := m.run("", "install", "-a", "claude-code", "-e", "lan@acme.test", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "\nYou're all set! Welcome to 127.0.0.1:", "  ✓ Claude Code is ready\n")
	lacks(t, "stdout", r.out, "hooks", "wrote", "signed in as")

	// Signing in, then the server gone: the sign-in's name, no org.
	m2 := newMachine(t, false)
	m2.srv.on("auth.login", "auth.login")
	m2.srv.on("account.me", `{"error": {"code": -32000, "message": "down", "data": {"reason": "UNAVAILABLE"}}}`)
	r = m2.run("pw\n", "install", "-a", "claude-code", "-e", "lan@acme.test", "-s", m2.srv.URL(), "--password-stdin")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "You're all set, lan! Welcome to 127.0.0.1:")
	lacks(t, "stdout", r.out, "Acme")

	// One agent refusing beside one set up (signed in already: no name known with the server gone).
	adapters.Register(&failingAdapter{fakeAdapter{name: "cursor", detected: true}})
	cc.installed = false
	r = m2.run("", "install", "-a", "claude-code", "-a", "cursor", "-e", "lan@acme.test", "-s", m2.srv.URL())
	wantCode(t, r, 1)
	contains(t, "stdout", r.out, "Almost there! You're in 127.0.0.1:", "  ✓ Claude Code is ready\n",
		"  ✗ Cursor CLI isn't set up yet (the reason is above)\n",
		"To finish, fix what's said above about Cursor CLI and run teamflow install again.\n")
	lacks(t, "stdout", r.out, "Happy building")
	contains(t, "stderr", r.err, "cursor: not set up: its hooks.json is not valid JSON", "not set up in cursor;")

	// Every agent refusing: no welcome at all, only what went wrong.
	adapters.Reset()
	adapters.Register(&failingAdapter{fakeAdapter{name: "cursor", detected: true}})
	r = m2.run("", "install", "-a", "cursor", "-e", "lan@acme.test", "-s", m2.srv.URL())
	wantCode(t, r, 1)
	if r.out != "" {
		t.Fatalf("a welcome with nothing set up: %q", r.out)
	}
}

// --verbose adds what was written where; --json holds all of it (what was set up, the changes, the account).
func TestInstallVerboseAndJSON(t *testing.T) {
	promptAgents(t)
	m := newMachine(t, true)
	r := m.run("", "install", "-a", "claude-code", "-e", "lan@acme.test", "-s", m.srv.URL(), "--verbose")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "signed in as lan@acme.test\n", "claude-code:\n  wrote ~/.claude-code/settings\n",
		"✓ Claude Code is ready")
	r = m.run("", "install", "-a", "claude-code", "-e", "lan@acme.test", "-s", m.srv.URL(), "--json")
	wantCode(t, r, 0)
	var got map[string]any
	if err := json.Unmarshal([]byte(r.out), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"installed": []any{"claude-code"}, "failed": []any{}, "on_path": false,
		"changes": map[string]any{"claude-code": []any{"wrote ~/.claude-code/settings"}},
		"account": map[string]any{"email": "lan@acme.test", "name": "lan", "orgs": []any{"Acme"}, "server": m.srv.URL()}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("answer %v", got)
	}
	contains(t, "stderr", r.err, "✓ Claude Code is ready")
}
