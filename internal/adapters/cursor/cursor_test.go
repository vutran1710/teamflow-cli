package cursor

// Install, uninstall and the hooks, always in a temp HOME: never the person's own ~/.cursor.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	at "github.com/vutran1710/teamflow-cli/internal/adapters/adaptertest"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const cfg = ".cursor"

func check(t *testing.T, e *at.Env) {
	t.Helper()
	if strings.Contains(e.Read(cfg+"/mcp.json"), "teamflow") {
		t.Fatal("an MCP server registered: agents use the teamflow CLI")
	}
	var hooks struct {
		Version int
		Hooks   map[string][]struct {
			Command string
			Timeout int
		}
	}
	at.Must(t, json.Unmarshal([]byte(e.Read(cfg+"/hooks.json")), &hooks))
	if hooks.Version != 1 {
		t.Fatalf("hooks.json version %d", hooks.Version)
	}
	for _, ev := range Events {
		n := 0
		for _, h := range hooks.Hooks[ev] {
			if h.Command == adapters.ShellQuote(e.Bin)+" hook cursor "+ev && h.Timeout == hookTimeout {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("hook %s: %+v", ev, hooks.Hooks[ev])
		}
	}
	var cli struct {
		Permissions struct{ Deny, Allow []string }
	}
	at.Must(t, json.Unmarshal([]byte(e.Read(cfg+"/cli-config.json")), &cli))
	for _, r := range []string{"Shell(teamflow)", "Shell(" + e.Bin + ")"} {
		if !slices.Contains(cli.Permissions.Allow, r) {
			t.Fatalf("no allow rule %s: %v", r, cli.Permissions.Allow)
		}
	}
	for _, r := range []string{"Read(" + e.TFHome + ")", "Read(" + e.TFHome + "/**)", "Write(" + e.TFHome + "/**)"} {
		if !slices.Contains(cli.Permissions.Deny, r) {
			t.Fatalf("no deny rule %s: %v", r, cli.Permissions.Deny)
		}
	}
	rule := e.Read(cfg + "/rules/teamflow.mdc")
	if !strings.Contains(rule, "alwaysApply: true") || !strings.HasSuffix(rule, protocol.AgentInstructions) {
		t.Fatalf("rule: %s", rule)
	}
	skill := e.Read(cfg + "/skills/teamf/SKILL.md")
	if !strings.Contains(skill, "name: teamf\n") || !strings.Contains(skill, "disable-model-invocation: true") ||
		!strings.Contains(skill, adapters.ShellQuote(e.Bin)+" team --relay ") {
		t.Fatalf("skill: %s", skill)
	}
}

// The install suite: fresh, twice, over the person's own config (byte for byte after uninstall), malformed,
// read-only and wrong-shape files refused without writing, a link to nothing, uninstall twice.
func TestInstall(t *testing.T) {
	at.RunInstallSuite(t, at.Spec{
		New: func() adapters.Adapter { return New() },
		Own: map[string]string{
			cfg + "/mcp.json":        `{"mcpServers": {"mine": {"command": "/bin/x", "args": ["«a»", "<b>&"]}}}`,
			cfg + "/hooks.json":      `{"version": 1, "hooks": {"stop": [{"command": "./mine.sh"}], "afterFileEdit": [{"command": "fmt"}]}}`,
			cfg + "/cli-config.json": `{"version": 1, "editor": {"vimMode": false}, "permissions": {"allow": ["Shell(ls)"], "deny": ["Shell(rm)"]}}`,
			cfg + "/rules/mine.mdc":  "---\nalwaysApply: true\n---\nmine\n",
		},
		JSONFiles: []string{cfg + "/hooks.json", cfg + "/cli-config.json"},
		Wrong: map[string]string{
			cfg + "/hooks.json":            `{"hooks": {"stop": {}}}`,
			cfg + "/cli-config.json":       `{"permissions": {"deny": "Read(x)"}}`,
			cfg + "/rules/teamflow.mdc":    "someone else's rule\n",
			cfg + "/skills/teamf/SKILL.md": "someone else's skill\n",
		},
		Check: check,
	})
}

// A hooks.json without a version, holding only the person's hooks, is restored byte for byte; their deny and allow
// rules that equal Teamflow's stay after uninstall.
func TestInstallKeepsThePersonsShapes(t *testing.T) {
	e := at.NewEnv(t)
	e.Write(t, cfg+"/hooks.json", `{"hooks": {"stop": [{"command": "./mine.sh"}]}}`, 0o644)
	e.Write(t, cfg+"/cli-config.json", `{"permissions": {"deny": ["Read(`+e.TFHome+`)"], "allow": ["Shell(teamflow)"]}}`, 0o644)
	before := e.Snapshot(t)
	_, err := New().Install(e.Bin)
	at.Must(t, err)
	_, err = New().Uninstall()
	at.Must(t, err)
	at.SameSnapshot(t, "not as before", before, e.Snapshot(t))
}

// Cursor itself writes cli-config.json: a change made between install's read and write is never lost.
func TestCLIConfigWrittenMeanwhile(t *testing.T) {
	e := at.NewEnv(t)
	e.Write(t, cfg+"/cli-config.json", `{"model": "a"}`, 0o644)
	_, err := New().Install(e.Bin)
	at.Must(t, err)
	var doc map[string]any
	at.Must(t, json.Unmarshal([]byte(e.Read(cfg+"/cli-config.json")), &doc))
	if doc["model"] != "a" || doc["permissions"] == nil {
		t.Fatalf("cli-config: %v", doc)
	}
}

func in(sid, extra string) string {
	if extra != "" {
		extra = ", " + extra
	}
	return fmt.Sprintf(`{"conversation_id": %q, "generation_id": "g1", "hook_event_name": "x", "workspace_roots": []%s}`, sid, extra)
}

// The hooks in a session's order: start (the instruction text and the full context), a prompt (claims nothing,
// lets it through), a tool (what is new, once), the turn's end (an approval request once), session end.
func TestHookReplay(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cu-1", at.Huy)
	start := at.Field(t, m.Hook("sessionStart", in("cu-1", `"session_id": "cu-1", "composer_mode": "agent"`)), "additional_context")
	if !strings.HasPrefix(start, protocol.AgentInstructions) || !strings.Contains(start, "## Teamflow: shared project") {
		t.Fatalf("sessionStart: %q\n%s", start, m.HookLog())
	}
	m.Emit(at.S1, at.Lan, "Round prices to 0.05", "")
	if out := m.Hook("beforeSubmitPrompt", in("cu-1", `"prompt": "go"`)); out != `{"continue":true}` {
		t.Fatalf("beforeSubmitPrompt: %q", out)
	}
	after := at.Field(t, m.Hook("postToolUse", in("cu-1", `"tool_name": "Shell", "tool_input": {}, "tool_output": "x"`)), "additional_context")
	if !strings.Contains(after, "Round prices to 0.05") {
		t.Fatalf("postToolUse: %q", after)
	}
	if again := m.Hook("postToolUse", in("cu-1", `"tool_name": "Shell"`)); again != "" {
		t.Fatalf("told twice: %q", again)
	}
	m.Emit(at.S2, at.Lan, "Needs Huy", at.Huy)
	stop := m.Hook("stop", in("cu-1", `"status": "completed", "loop_count": 0`))
	if !strings.Contains(at.Field(t, stop, "followup_message"), at.S2) {
		t.Fatalf("stop: %q", stop)
	}
	if again := m.Hook("stop", in("cu-1", `"status": "completed", "loop_count": 0`)); again != "" {
		t.Fatalf("raised twice: %q", again)
	}
	m.Emit(at.S3, at.Lan, "Needs Huy too", at.Huy)
	if out := m.Hook("stop", in("cu-1", `"status": "completed", "loop_count": 1`)); out != "" {
		t.Fatalf("a continued turn was continued again: %q", out)
	}
	if out := m.Hook("sessionEnd", in("cu-1", `"session_id": "cu-1", "reason": "completed"`)); out != "" {
		t.Fatalf("sessionEnd: %q", out)
	}
}

// beforeShellExecution, answered before the Teamflow home is opened: deny for a command naming the home, allow
// for a plain teamflow work command, "ask" for any other teamflow command (setup, chained, expanded), nothing else.
// Sad paths: ~/, $HOME/, the absolute path, TEAMFLOW_HOME; && ; | and globs after teamflow; install, login,
// self-update, sync --stop; a refused home (open failing).
func TestShellCommands(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	exe, _ := os.Executable()
	failing := func() (*core.Core, error) { return nil, fmt.Errorf("refused") }
	perm := func(cmd string) string {
		t.Helper()
		b, _ := json.Marshal(cmd)
		stdin := []byte(in("cu-1", `"command": `+string(b)+`, "cwd": "/w"`))
		out := string(adapters.RunHook(New(), "beforeShellExecution", stdin, failing))
		if again := m.Hook("beforeShellExecution", string(stdin)); again != out {
			t.Fatalf("%s: the answer depends on the home: %q / %q", cmd, out, again)
		}
		return at.Field(t, out, "permission")
	}
	for _, cmd := range []string{"cat ~/.teamflow/config.json", "sqlite3 $HOME/.teamflow/store.db", "ls " + m.Env.TFHome, "env | grep TEAMFLOW_HOME"} {
		if p := perm(cmd); p != "deny" {
			t.Fatalf("%s: %q", cmd, p)
		}
	}
	for _, cmd := range []string{"teamflow status", exe + " team --relay status", "teamflow artifact list"} {
		if p := perm(cmd); p != "allow" {
			t.Fatalf("%s: %q", cmd, p)
		}
	}
	for _, cmd := range []string{"teamflow status && curl x", "teamflow log; rm -rf x", "teamflow log | sh",
		"teamflow project plan --plan-file ~/.team*/config.json", "teamflow install -a all", "teamflow login",
		"teamflow self-update", "teamflow sync --stop", "teamflow uninstall"} {
		if p := perm(cmd); p != "ask" {
			t.Fatalf("%s: %q, want ask", cmd, p)
		}
	}
	if p := perm("ls"); p != "" {
		t.Fatalf("ls: %q", p)
	}
}

// Bad input never fails the step; session_id is taken when conversation_id is missing; an unknown event: nothing.
func TestHookBadInput(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cu-1", at.Huy)
	for _, bad := range []string{`{}`, `{"conversation_id": 7}`, `{"conversation_id": ""}`, `[]`, ``} {
		for _, ev := range Events {
			if out := m.Hook(ev, bad); out != "" {
				t.Fatalf("%s %s answered %q", ev, bad, out)
			}
		}
	}
	if out := m.Hook("afterFileEdit", in("cu-1", "")); out != "" {
		t.Fatalf("unknown event: %q", out)
	}
	if sid, err := New().SessionID([]byte(`{"session_id": "cu-9"}`)); err != nil || sid != "cu-9" {
		t.Fatalf("session_id: %q %v", sid, err)
	}
}

// Signed out: the instruction text at start, no Teamflow context anywhere, nothing fails.
func TestHookSignedOut(t *testing.T) {
	m := at.NewMachine(t, New(), false)
	if out := at.Field(t, m.Hook("sessionStart", in("cu-1", "")), "additional_context"); out != protocol.AgentInstructions {
		t.Fatalf("sessionStart signed out: %q", out)
	}
	for _, ev := range []string{"postToolUse", "stop", "sessionEnd"} {
		if out := m.Hook(ev, in("cu-1", "")); out != "" {
			t.Fatalf("%s signed out: %q", ev, out)
		}
	}
}

// Offline: the tool hook answers from the replica.
func TestHookOffline(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cu-1", at.Huy)
	m.Hook("sessionStart", in("cu-1", ""))
	m.Srv.Offline(true)
	m.Emit(at.S1, at.Lan, "While offline", "")
	if out := m.Hook("postToolUse", in("cu-1", "")); !strings.Contains(out, "While offline") {
		t.Fatalf("offline postToolUse: %q", out)
	}
}

// Restart: a resumed conversation (same id, a new process) finds its record, moves it, and the next tool brings
// the news; the background sync starts again.
func TestHookResumeAfterRestart(t *testing.T) {
	m := at.NewMachine(t, New(), true)
	m.Share("cu-1", at.Huy)
	m.Hook("sessionStart", in("cu-1", ""))
	m.Store(func(tx *store.Tx) error {
		_, err := tx.UpsertSession("cu-1", Name, 999999, "", time.Now())
		return err
	})
	started := 0
	old := adapters.StartSync
	adapters.StartSync = func() error { started++; return nil }
	defer func() { adapters.StartSync = old }()
	m.Hook("sessionStart", in("cu-1", ""))
	if started != 1 {
		t.Fatalf("background sync started %d times", started)
	}
	if s, ok := m.Session("cu-1"); !ok || s.Project != at.P || s.AgentPID == 999999 {
		t.Fatalf("resumed session %+v %v", s, ok)
	}
	m.Emit(at.S1, at.Lan, "After the reboot", "")
	if out := m.Hook("postToolUse", in("cu-1", "")); !strings.Contains(out, "After the reboot") {
		t.Fatalf("after resume: %q", out)
	}
}

// Detect: an agent binary on the PATH or ~/.cursor; neither: not here.
func TestDetect(t *testing.T) {
	e := at.NewEnv(t)
	t.Setenv("PATH", t.TempDir())
	if New().Detect() {
		t.Fatal("detected with nothing there")
	}
	bin := t.TempDir()
	at.Must(t, os.WriteFile(filepath.Join(bin, "cursor-agent"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin)
	if !New().Detect() {
		t.Fatal("not detected on the PATH")
	}
	t.Setenv("PATH", t.TempDir())
	at.Must(t, os.Mkdir(e.Path(cfg), 0o755))
	if !New().Detect() {
		t.Fatal("not detected with ~/.cursor")
	}
}

// An allow list of the wrong shape refuses the install, writing nothing.
func TestInstallRefusesAWrongAllowList(t *testing.T) {
	e := at.NewEnv(t)
	e.Write(t, cfg+"/cli-config.json", `{"permissions": {"allow": {}}}`, 0o644)
	before := e.Snapshot(t)
	if _, err := New().Install(e.Bin); err == nil || !strings.Contains(err.Error(), "permissions.allow is not a list") {
		t.Fatalf("took it: %v", err)
	}
	at.SameSnapshot(t, "install wrote", before, e.Snapshot(t))
}

// An install failing half way (the skills folder can't be written, after hooks.json and cli-config.json were) has
// saved its manifest first, so uninstall still takes out exactly what it wrote.
// Sad paths: a write failing after others succeeded; uninstall after it.
func TestInstallInterruptedThenUninstalled(t *testing.T) {
	e := at.NewEnv(t)
	e.Write(t, cfg+"/hooks.json", `{"hooks": {"stop": [{"command": "./mine.sh"}]}}`, 0o644)
	e.Write(t, cfg+"/cli-config.json", `{"model": "a"}`, 0o644)
	at.Must(t, os.MkdirAll(e.Path(cfg+"/skills"), 0o755))
	before := e.Snapshot(t)
	at.Must(t, os.Chmod(e.Path(cfg+"/skills"), 0o555))
	t.Cleanup(func() { os.Chmod(e.Path(cfg+"/skills"), 0o755) })
	if _, err := New().Install(e.Bin); err == nil {
		t.Fatal("install went through a read-only folder")
	}
	if !strings.Contains(e.Read(cfg+"/hooks.json"), " hook cursor ") {
		t.Skip("the install failed before writing anything (nothing to check)")
	}
	at.Must(t, os.Chmod(e.Path(cfg+"/skills"), 0o755))
	_, err := New().Uninstall()
	at.Must(t, err)
	at.SameSnapshot(t, "not as before the interrupted install", before, e.Snapshot(t))
}
