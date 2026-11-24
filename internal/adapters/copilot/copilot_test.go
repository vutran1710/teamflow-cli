package copilot

// Install, uninstall and the hooks, always in a temp HOME and COPILOT_HOME: never the person's own ~/.copilot.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	at "github.com/vutran1710/teamflow-cli/internal/adapters/adaptertest"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const cfg = ".copilot"

func check(t *testing.T, e *at.Env) {
	t.Helper()
	if strings.Contains(e.Read(cfg+"/mcp-config.json"), "teamflow") {
		t.Fatal("an MCP server registered: agents use the teamflow CLI")
	}
	var hooks struct {
		Version int
		Hooks   map[string][]struct {
			Type, Bash string
			TimeoutSec int
		}
	}
	at.Must(t, json.Unmarshal([]byte(e.Read(cfg+"/hooks/teamflow.json")), &hooks))
	if hooks.Version != 1 || len(hooks.Hooks) != len(Events) {
		t.Fatalf("hooks: %+v", hooks)
	}
	for _, ev := range Events {
		h := hooks.Hooks[ev]
		if len(h) != 1 || h[0].Type != "command" || h[0].Bash != adapters.ShellQuote(e.Bin)+" hook copilot "+ev || h[0].TimeoutSec != hookTimeout {
			t.Fatalf("hook %s: %+v", ev, h)
		}
	}
	if !strings.Contains(e.Read(cfg+"/copilot-instructions.md"), adapters.BlockBegin) {
		t.Fatal("no instruction block")
	}
	skill := e.Read(cfg + "/skills/teamf/SKILL.md")
	if !strings.HasPrefix(skill, "---\nname: teamf\ndescription: ") || !strings.Contains(skill, "\nallowed-tools: shell(teamflow:*)\n") || !strings.Contains(skill, adapters.ShellQuote(e.Bin)+" team --relay ") {
		t.Fatalf("skill: %s", skill)
	}
}

// The install suite: fresh, twice, over the person's own config (byte for byte after uninstall), malformed,
// read-only and wrong-shape files refused without writing, a link to nothing, uninstall twice.
func TestInstall(t *testing.T) {
	at.RunInstallSuite(t, at.Spec{
		New: func() adapters.Adapter { return New() },
		Own: map[string]string{
			cfg + "/mcp-config.json": "{\n  \"mcpServers\": {\n    \"mine\": {\n      \"type\": \"local\",\n      \"command\": \"/bin/x\",\n" +
				"      \"args\": [],\n      \"tools\": [\n        \"*\"\n      ]\n    }\n  }\n}\n",
			cfg + "/copilot-instructions.md": "Always answer in « French » & <be> brief.\n",
			cfg + "/hooks/mine.json":         `{"version": 1, "hooks": {"preToolUse": [{"bash": "./mine.sh"}]}}`,
			cfg + "/settings.json":           "{\n  // the person's comments stay: Teamflow never writes this file\n  \"model\": \"x\"\n}\n",
		},
		JSONFiles: []string{cfg + "/hooks/teamflow.json"},
		Wrong: map[string]string{
			cfg + "/skills/teamf/SKILL.md":   "---\nname: teamf\n---\nsomeone else's\n",
			cfg + "/copilot-instructions.md": adapters.BlockBegin + "\nno end\n",
		},
		Check: check,
	})
}

// $COPILOT_HOME is honoured: everything goes there, nothing in ~/.copilot.
func TestInstallCopilotHome(t *testing.T) {
	e := at.NewEnv(t)
	dir := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv("COPILOT_HOME", dir)
	_, err := New().Install(e.Bin)
	at.Must(t, err)
	if _, err := os.Stat(filepath.Join(dir, "hooks", "teamflow.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Path(cfg)); err == nil {
		t.Fatal("wrote ~/.copilot with COPILOT_HOME set")
	}
	_, err = New().Uninstall()
	at.Must(t, err)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the folder install made is left")
	}
}

// Detect: the config folder or the binary on the PATH; neither: not here.
func TestDetect(t *testing.T) {
	e := at.NewEnv(t)
	t.Setenv("PATH", t.TempDir())
	if New().Detect() {
		t.Fatal("detected with nothing there")
	}
	at.Must(t, os.Mkdir(e.Path(cfg), 0o755))
	if !New().Detect() {
		t.Fatal("not detected with ~/.copilot")
	}
	at.Must(t, os.Remove(e.Path(cfg)))
	bin := t.TempDir()
	at.Must(t, os.WriteFile(filepath.Join(bin, "copilot"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin)
	if !New().Detect() {
		t.Fatal("not detected on the PATH")
	}
}

func in(sid string, extra string) string {
	if extra != "" {
		extra = ", " + extra
	}
	return fmt.Sprintf(`{"sessionId": %q, "timestamp": 1764000000000, "cwd": ""%s}`, sid, extra)
}

// The hooks in a session's order: start (full context), a prompt (claims nothing), a tool (what is new, once), the
// turn's end (an approval request raised once), session end. Each notice is told exactly once.
func TestHookReplay(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cp-1", at.Huy)
	start := at.Field(t, m.Hook("sessionStart", in("cp-1", `"source": "startup"`)), "additionalContext")
	if !strings.Contains(start, "## Teamflow: shared project") || !strings.Contains(start, "Ship pricing v1") {
		t.Fatalf("sessionStart: %q\n%s", start, m.HookLog())
	}
	m.Emit(at.S1, at.Lan, "Round prices to 0.05", "")
	if out := m.Hook("userPromptSubmitted", in("cp-1", `"prompt": "go"`)); out != "" {
		t.Fatalf("userPromptSubmitted answered %q (Copilot drops it: whatever it claimed would be lost)", out)
	}
	after := at.Field(t, m.Hook("postToolUse", in("cp-1", `"toolName": "bash", "toolArgs": {"command": "ls"}, "toolResult": {"resultType": "success"}`)), "additionalContext")
	if !strings.Contains(after, "Round prices to 0.05") {
		t.Fatalf("postToolUse: %q", after)
	}
	if again := m.Hook("postToolUse", in("cp-1", `"toolName": "bash"`)); again != "" {
		t.Fatalf("told twice: %q", again)
	}
	m.Emit(at.S2, at.Lan, "Needs Huy", at.Huy)
	stop := m.Hook("agentStop", in("cp-1", `"stopReason": "end_turn", "stop_hook_active": false`))
	if at.Field(t, stop, "decision") != "block" || !strings.Contains(at.Field(t, stop, "reason"), at.S2) {
		t.Fatalf("agentStop: %q", stop)
	}
	if again := m.Hook("agentStop", in("cp-1", `"stop_hook_active": false`)); again != "" {
		t.Fatalf("raised twice: %q", again)
	}
	if out := m.Hook("agentStop", in("cp-1", `"stop_hook_active": true`)); out != "" {
		t.Fatalf("a continued turn was blocked again: %q", out)
	}
	if out := m.Hook("sessionEnd", in("cp-1", `"reason": "complete"`)); out != "" {
		t.Fatalf("sessionEnd: %q", out)
	}
	// The approval request raised at agentStop is not told again after a tool.
	if out := m.Hook("postToolUse", in("cp-1", `"toolName": "bash"`)); strings.Contains(out, "new approval request") {
		t.Fatalf("raised twice after a tool: %q", out)
	}
}

// preToolUse refuses tool calls that name the Teamflow home (its path in any form, or TEAMFLOW_HOME), and lets
// every other call through, /teamf's own run of the binary included.
// Sad paths: ~/, $HOME/, ${HOME}/, the absolute path, a .teamflow/ path, TEAMFLOW_HOME, JSON-escaped args.
func TestPreToolUseKeepsOutOfTheHome(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	exe, _ := os.Executable()
	for _, args := range []string{
		`{"command": "cat ~/.teamflow/config.json"}`,
		`{"command": "cat $HOME/.teamflow/store.db"}`,
		`{"command": "ls ${HOME}/.teamflow"}`,
		`{"path": "` + m.Env.TFHome + `/config.json"}`,
		`"{\"path\": \"` + m.Env.TFHome + `\"}"`,
		`{"command": "cp x/.teamflow/config.json /tmp"}`,
		`{"command": "echo $TEAMFLOW_HOME"}`,
	} {
		out := m.Hook("preToolUse", in("cp-1", `"toolName": "bash", "toolArgs": `+args))
		if at.Field(t, out, "permissionDecision") != "deny" || at.Field(t, out, "permissionDecisionReason") == "" {
			t.Fatalf("%s: %q", args, out)
		}
	}
	for _, args := range []string{
		`{"command": "ls"}`,
		`{"command": "cat .teamflow"}`, // the project's own share file
		`{"command": "` + exe + ` team --relay status"}`,
	} {
		if out := m.Hook("preToolUse", in("cp-1", `"toolName": "bash", "toolArgs": `+args)); at.Field(t, out, "permissionDecision") == "deny" {
			t.Fatalf("%s refused: %q", args, out)
		}
	}
}

// Bad input never fails the step: every event answers nothing for malformed or id-less input; an unknown event
// answers nothing; snake_case session_id (the VS Code-compatible payload) is taken.
func TestHookBadInput(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cp-1", at.Huy)
	for _, bad := range []string{`{}`, `{"sessionId": 7}`, `{"sessionId": ""}`, `[]`, ``, `{"sessionId": "cp-1"`} {
		for _, ev := range Events {
			if out := m.Hook(ev, bad); out != "" {
				t.Fatalf("%s %s answered %q", ev, bad, out)
			}
		}
		if _, err := New().SessionID([]byte(bad)); err == nil {
			t.Fatalf("SessionID(%q) took it", bad)
		}
	}
	if !strings.Contains(m.HookLog(), "copilot") || strings.Contains(m.HookLog(), "cp-1\"") {
		t.Fatalf("hook.log: %q", m.HookLog())
	}
	if out := m.Hook("notification", in("cp-1", "")); out != "" {
		t.Fatalf("unknown event: %q", out)
	}
	if sid, err := New().SessionID([]byte(`{"session_id": "cp-9"}`)); err != nil || sid != "cp-9" {
		t.Fatalf("snake case: %q %v", sid, err)
	}
}

// Signed out: no context anywhere, nothing fails.
func TestHookSignedOut(t *testing.T) {
	m := at.NewMachine(t, New(), false)
	for _, ev := range []string{"sessionStart", "userPromptSubmitted", "postToolUse", "agentStop", "sessionEnd"} {
		if out := m.Hook(ev, in("cp-1", `"source": "startup"`)); out != "" {
			t.Fatalf("%s signed out: %q", ev, out)
		}
	}
}

// Offline: the tool hook answers from the replica; the turn's end still raises a request from the replica.
func TestHookOffline(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cp-1", at.Huy)
	m.Hook("sessionStart", in("cp-1", `"source": "startup"`))
	m.Srv.Offline(true)
	m.Emit(at.S1, at.Lan, "While offline", "")
	m.Emit(at.S2, at.Lan, "Needs Huy", at.Huy)
	if out := m.Hook("postToolUse", in("cp-1", `"toolName": "bash"`)); !strings.Contains(out, "While offline") || !strings.Contains(out, at.S2) {
		t.Fatalf("offline postToolUse: %q", out)
	}
}

// Restart: Copilot resumes its session after a reboot (same id, a new process): the record is found, moves to
// the new process, resume adds nothing, the background sync starts again, and the next tool brings the news.
func TestHookResumeAfterRestart(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cp-1", at.Huy)
	m.Hook("sessionStart", in("cp-1", `"source": "startup"`))
	m.Store(func(tx *store.Tx) error {
		_, err := tx.UpsertSession("cp-1", Name, 999999, "", time.Now())
		return err
	})
	started := 0
	old := adapters.StartSync
	adapters.StartSync = func() error { started++; return fmt.Errorf("none in this build") }
	defer func() { adapters.StartSync = old }()
	if out := m.Hook("sessionStart", in("cp-1", `"source": "resume"`)); out != "" {
		t.Fatalf("resume added %q", out)
	}
	if started != 1 {
		t.Fatalf("background sync started %d times", started)
	}
	if s, ok := m.Session("cp-1"); !ok || s.Project != at.P || s.AgentPID == 999999 {
		t.Fatalf("resumed session %+v %v", s, ok)
	}
	m.Emit(at.S1, at.Lan, "After the reboot", "")
	if out := m.Hook("postToolUse", in("cp-1", `"toolName": "bash"`)); !strings.Contains(out, "After the reboot") {
		t.Fatalf("after resume: %q", out)
	}
	m.Hook("userPromptSubmitted", in("cp-1", `"prompt": "go"`))
	if started != 2 { // a prompt starts it too when it is not running
		t.Fatalf("background sync started %d times", started)
	}
}

// 50 tool steps in a turn make at most one network sync (the tool hooks stay off the network).
func TestPostToolUseStaysOffTheNetwork(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cp-1", at.Huy)
	m.Hook("sessionStart", in("cp-1", `"source": "startup"`))
	before := m.Srv.Count("sync.poll")
	for range 50 {
		m.Hook("preToolUse", in("cp-1", `"toolName": "bash", "toolArgs": {"command": "ls"}`))
		m.Hook("postToolUse", in("cp-1", `"toolName": "bash"`))
	}
	if n := m.Srv.Count("sync.poll") - before; n > 1 {
		t.Fatalf("%d syncs for 50 tool steps", n)
	}
}

// preToolUse allows a plain teamflow work command without a prompt (by name or by the binary's path, quoted or
// not); answers "ask" for any other teamflow command: setup commands, and anything chained, piped, redirected,
// substituted or expanded (globs, braces, ~, variables); answers nothing for other commands and other tools.
// Sad paths: ; && | > $( ) backticks, a newline, * ? [ ] { } ~ $, every setup command, sync --stop, an option
// first, a look-alike name, a non-shell tool, args as a JSON string.
func TestPreToolUseAllowsTeamflowCommands(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	exe, _ := os.Executable()
	q := func(cmd string) string { b, _ := json.Marshal(map[string]string{"command": cmd}); return string(b) }
	decision := func(tool, args string) string {
		t.Helper()
		return at.Field(t, m.Hook("preToolUse", in("cp-1", `"toolName": "`+tool+`", "toolArgs": `+args)), "permissionDecision")
	}
	for _, cmd := range []string{"teamflow status", exe + " team --relay approvals", adapters.ShellQuote(exe) + " log",
		"teamflow artifact list", "teamflow project plan --plan 'Ship: v1'"} {
		if d := decision("bash", q(cmd)); d != "allow" {
			t.Fatalf("%s: %q", cmd, d)
		}
	}
	str, _ := json.Marshal(q("teamflow status"))
	if d := decision("bash", string(str)); d != "allow" {
		t.Fatalf("args as a string: %q", d)
	}
	for _, cmd := range []string{"teamflow status; rm -rf x", "teamflow status && curl x", "teamflow log | sh",
		"teamflow log > x", "teamflow $(id)", "teamflow `id`", "teamflow status\nrm x", "teamflow log $X",
		"teamflow project plan --plan-file ~/.team*/config.json", "teamflow project plan --plan-file ~/.TEAMFLOW/config.json",
		"teamflow project plan --plan-file x?y", "teamflow project plan --plan-file [ab]", "teamflow artifact write {a,b}",
		"teamflow install -a all", "teamflow uninstall", "teamflow login -e a@b.c", "teamflow logout", "teamflow self-update",
		"teamflow hook copilot sessionStart", "teamflow mcp", "teamflow debug state --json", "teamflow sync --stop", "teamflow sync --stop=true", "teamflow sync -stop", "teamflow sync",
		"teamflow", "teamflow --help", "teamflow 'install'"} {
		if d := decision("bash", q(cmd)); d != "ask" {
			t.Fatalf("%s: %q, want ask", cmd, d)
		}
	}
	for _, cmd := range []string{"teamflowx status", "rm teamflow", "ls"} {
		if d := decision("bash", q(cmd)); d != "" {
			t.Fatalf("%s: %q", cmd, d)
		}
	}
	if d := decision("view", q("teamflow status")); d != "" {
		t.Fatalf("a non-shell tool: %q", d)
	}
}

// preToolUse answers before the Teamflow home is opened: with a home that refuses to open (here: open to others,
// and the open itself failing), the deny, the allow and the ask still hold.
// Sad paths: a refused home; an open that fails; a store never touched.
func TestPreToolUseWithARefusedHome(t *testing.T) {
	e := at.NewEnv(t)
	at.Must(t, os.Chmod(e.TFHome, 0o755)) // a home open to others is refused by home.Ensure
	q := func(cmd string) string { b, _ := json.Marshal(map[string]string{"command": cmd}); return string(b) }
	failing := func() (*core.Core, error) { return nil, fmt.Errorf("the home is refused") }
	for cmd, want := range map[string]string{"cat ~/.teamflow/config.json": "deny", "teamflow status": "allow", "teamflow install": "ask"} {
		for _, open := range []func() (*core.Core, error){core.Open, failing} {
			out := adapters.RunHook(New(), "preToolUse", []byte(in("cp-1", `"toolName": "bash", "toolArgs": `+q(cmd))), open)
			if got := at.Field(t, string(out), "permissionDecision"); got != want {
				t.Fatalf("%s with a refused home: %q, want %s", cmd, got, want)
			}
		}
	}
	if _, err := os.Stat(e.TFHome + "/store.db"); err == nil {
		t.Fatal("the store was opened for a permission answer")
	}
}
