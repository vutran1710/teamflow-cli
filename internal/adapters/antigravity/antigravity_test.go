package antigravity

// Install, uninstall and the hooks, always in a temp HOME: never the person's own ~/.gemini.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	at "github.com/vutran1710/teamflow-cli/internal/adapters/adaptertest"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

func check(t *testing.T, e *at.Env) {
	t.Helper()
	if _, err := os.Stat(e.Path(".gemini/config/mcp_config.json")); err == nil && strings.Contains(e.Read(".gemini/config/mcp_config.json"), "teamflow") {
		t.Fatal("an MCP server registered: agents use the teamflow CLI")
	}
	var hooks map[string]map[string][]struct {
		Type, Command string
		Timeout       int
	}
	at.Must(t, json.Unmarshal([]byte(e.Read(".gemini/config/hooks.json")), &hooks))
	for _, ev := range Events {
		h := hooks["teamflow"][ev]
		if len(h) != 1 || h[0].Type != "command" || h[0].Command != adapters.ShellQuote(e.Bin)+" hook antigravity "+ev || h[0].Timeout != hookTimeout {
			t.Fatalf("hook %s: %+v", ev, h)
		}
	}
	if !strings.Contains(e.Read(".gemini/config/AGENTS.md"), adapters.BlockBegin) {
		t.Fatal("no instruction block")
	}
	if !strings.Contains(e.Read(".gemini/config/skills/teamf/SKILL.md"), "name: teamf\n") {
		t.Fatal("no /teamf skill")
	}
	var s struct {
		Permissions struct{ Deny, Allow []string }
	}
	at.Must(t, json.Unmarshal([]byte(e.Read(".gemini/antigravity-cli/settings.json")), &s))
	for _, r := range []string{"command(teamflow)", "command(" + e.Bin + ")"} {
		if !slices.Contains(s.Permissions.Allow, r) {
			t.Fatalf("no allow rule %s: %v", r, s.Permissions.Allow)
		}
	}
	for _, r := range []string{"read_file(" + e.TFHome + ")", "command(regex:.*~/\\.teamflow.*)", "command(regex:.*TEAMFLOW_HOME.*)"} {
		if !slices.Contains(s.Permissions.Deny, r) {
			t.Fatalf("no deny rule %s: %v", r, s.Permissions.Deny)
		}
	}
}

// The install suite: fresh, twice, over the person's own config (byte for byte after uninstall), malformed,
// read-only and wrong-shape files refused without writing, a link to nothing, uninstall twice.
func TestInstall(t *testing.T) {
	at.RunInstallSuite(t, at.Spec{
		New: func() adapters.Adapter { return New() },
		Own: map[string]string{
			".gemini/config/mcp_config.json":        `{"mcpServers": {"mine": {"command": "npx", "args": ["-y", "x"], "env": {"K": "«v»"}}}}`,
			".gemini/config/hooks.json":             `{"my-linter": {"PostToolUse": [{"matcher": "run_command", "hooks": [{"command": "./lint.sh"}]}]}}`,
			".gemini/config/AGENTS.md":              "# Mine\n\nBe brief & <kind>.\n",
			".gemini/antigravity-cli/settings.json": `{"theme": "dark", "permissions": {"allow": ["command(npm test)"], "deny": ["command(sudo)"]}}`,
			".gemini/GEMINI.md":                     "Gemini CLI's own file: never touched\n",
		},
		JSONFiles: []string{".gemini/config/hooks.json", ".gemini/antigravity-cli/settings.json"},
		Wrong: map[string]string{
			".gemini/config/hooks.json":             `{"teamflow": {"Stop": [{"command": "someone else's"}]}}`,
			".gemini/antigravity-cli/settings.json": `{"permissions": []}`,
			".gemini/config/skills/teamf/SKILL.md":  "---\nname: teamf\n---\nmine\n",
			".gemini/config/AGENTS.md":              adapters.BlockEnd + "\n" + adapters.BlockBegin + "\n",
		},
		Check: check,
	})
}

// The command deny rules are valid regular expressions that match a command naming the home and not an ordinary
// one; they are left out when the binary lives in the home (they would block /teamf).
func TestDenyRules(t *testing.T) {
	e := at.NewEnv(t)
	for _, r := range DenyRules(e.Bin) {
		pat, ok := strings.CutPrefix(r, "command(regex:")
		if !ok {
			continue
		}
		re := regexp.MustCompile("^" + strings.TrimSuffix(pat, ")") + "$")
		if re.MatchString("ls -la") || re.MatchString(e.Bin+" team --relay status") {
			t.Fatalf("%s matches an ordinary command", r)
		}
	}
	all := strings.Join(DenyRules(e.Bin), "\n")
	for _, cmd := range []string{"cat ~/.teamflow/config.json", "cat " + e.TFHome + "/store.db", "echo $TEAMFLOW_HOME"} {
		hit := false
		for _, r := range DenyRules(e.Bin) {
			if pat, ok := strings.CutPrefix(r, "command(regex:"); ok && regexp.MustCompile("^"+strings.TrimSuffix(pat, ")")+"$").MatchString(cmd) {
				hit = true
			}
		}
		if !hit {
			t.Fatalf("%q is not denied by\n%s", cmd, all)
		}
	}
	for _, r := range DenyRules(filepath.Join(e.TFHome, "bin", "teamflow")) {
		if strings.HasPrefix(r, "command(") {
			t.Fatalf("a command rule with the binary in the home: %s", r)
		}
	}
}

func in(sid, extra string) string {
	if extra != "" {
		extra = ", " + extra
	}
	return fmt.Sprintf(`{"conversationId": %q, "workspacePaths": [], "transcriptPath": "", "modelName": "m"%s}`, sid, extra)
}

func injected(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var v struct {
		InjectSteps []struct {
			SystemMessage struct{ SystemMessage string } `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.InjectSteps) != 1 {
		t.Fatalf("not injectSteps: %q", out)
	}
	return v.InjectSteps[0].SystemMessage.SystemMessage
}

// The hooks in a session's order: start (full context), the turn's first model call (what is new), a later call
// after a tool (what is new since, once), the turn's end (an approval request once).
func TestHookReplay(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("ag-1", at.Huy)
	if out := injected(t, m.Hook("SessionStart", in("ag-1", ""))); !strings.Contains(out, "## Teamflow: shared project") {
		t.Fatalf("SessionStart: %q\n%s", out, m.HookLog())
	}
	m.Emit(at.S1, at.Lan, "Round prices to 0.05", "")
	if out := injected(t, m.Hook("PreInvocation", in("ag-1", `"invocationNum": 0, "initialNumSteps": 3, "lastUserInput": "go"`))); !strings.Contains(out, "Round prices to 0.05") {
		t.Fatalf("first PreInvocation: %q", out)
	}
	if out := m.Hook("PreInvocation", in("ag-1", `"invocationNum": 1`)); out != "" {
		t.Fatalf("told twice: %q", out)
	}
	m.Emit(at.S3, at.Lan, "Mid-turn news", "")
	if out := injected(t, m.Hook("PreInvocation", in("ag-1", `"invocationNum": 2`))); !strings.Contains(out, "Mid-turn news") {
		t.Fatalf("later PreInvocation: %q", out)
	}
	m.Emit(at.S2, at.Lan, "Needs Huy", at.Huy)
	stop := m.Hook("Stop", in("ag-1", `"executionNum": 1, "terminationReason": "done", "fullyIdle": true`))
	if at.Field(t, stop, "decision") != "continue" || !strings.Contains(at.Field(t, stop, "reason"), at.S2) {
		t.Fatalf("Stop: %q", stop)
	}
	if again := m.Hook("Stop", in("ag-1", `"executionNum": 2`)); again != "" {
		t.Fatalf("raised twice: %q", again)
	}
	// The person's own /teamf approve: the prompt hook adds nothing (the command records the verdict after it).
	if out := m.Hook("PreInvocation", in("ag-1", `"invocationNum": 0, "lastUserInput": "/teamf approve `+at.S2+`"`)); out != "" {
		t.Fatalf("typed verdict: %q", out)
	}
}

// Bad input never fails the step; an unknown event answers nothing.
func TestHookBadInput(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("ag-1", at.Huy)
	for _, bad := range []string{`{}`, `{"conversationId": 7}`, `{"conversationId": ""}`, `[]`, ``} {
		for _, ev := range Events {
			if out := m.Hook(ev, bad); out != "" {
				t.Fatalf("%s %s answered %q", ev, bad, out)
			}
		}
	}
	if out := m.Hook("PostToolUse", in("ag-1", "")); out != "" {
		t.Fatalf("unknown event: %q", out)
	}
}

// Signed out: nothing anywhere, nothing fails.
func TestHookSignedOut(t *testing.T) {
	m := at.NewMachine(t, New(), false)
	for _, ev := range Events {
		if out := m.Hook(ev, in("ag-1", `"invocationNum": 0`)); out != "" {
			t.Fatalf("%s signed out: %q", ev, out)
		}
	}
}

// Offline: the turn's first call answers from the replica and says the server is unreachable.
func TestHookOffline(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("ag-1", at.Huy)
	m.Hook("SessionStart", in("ag-1", ""))
	m.Srv.Offline(true)
	m.Emit(at.S1, at.Lan, "While offline", "")
	out := injected(t, m.Hook("PreInvocation", in("ag-1", `"invocationNum": 0`)))
	if !strings.Contains(out, "While offline") || !strings.Contains(out, "unreachable") {
		t.Fatalf("offline: %q", out)
	}
}

// Restart: a resumed conversation (same id, a new process) finds its record and moves it; the background sync
// starts again; the next turn brings the news.
func TestHookResumeAfterRestart(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("ag-1", at.Huy)
	m.Hook("SessionStart", in("ag-1", ""))
	m.Store(func(tx *store.Tx) error {
		_, err := tx.UpsertSession("ag-1", Name, 999999, "", time.Now())
		return err
	})
	started := 0
	old := adapters.StartSync
	adapters.StartSync = func() error { started++; return nil }
	defer func() { adapters.StartSync = old }()
	m.Hook("SessionStart", in("ag-1", ""))
	if started != 1 {
		t.Fatalf("background sync started %d times", started)
	}
	if s, ok := m.Session("ag-1"); !ok || s.Project != at.P || s.AgentPID == 999999 {
		t.Fatalf("resumed session %+v %v", s, ok)
	}
	m.Emit(at.S1, at.Lan, "After the reboot", "")
	if out := m.Hook("PreInvocation", in("ag-1", `"invocationNum": 0`)); !strings.Contains(out, "After the reboot") {
		t.Fatalf("after resume: %q", out)
	}
}

// Detect: agy on the PATH, or ~/.gemini/antigravity-cli; ~/.gemini alone (Gemini CLI) is not Antigravity.
func TestDetect(t *testing.T) {
	e := at.NewEnv(t)
	t.Setenv("PATH", t.TempDir())
	at.Must(t, os.Mkdir(e.Path(".gemini"), 0o755))
	if New().Detect() {
		t.Fatal("detected from ~/.gemini alone")
	}
	at.Must(t, os.Mkdir(e.Path(".gemini/antigravity-cli"), 0o755))
	if !New().Detect() {
		t.Fatal("not detected with its folder")
	}
	at.Must(t, os.Remove(e.Path(".gemini/antigravity-cli")))
	bin := t.TempDir()
	at.Must(t, os.WriteFile(filepath.Join(bin, "agy"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin)
	if !New().Detect() {
		t.Fatal("not detected on the PATH")
	}
}
