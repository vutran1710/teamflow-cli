package claudecode

// Install and uninstall, always in a temp HOME and CLAUDE_CONFIG_DIR: never the person's own ~/.claude.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

type env struct {
	home, cfg, tfHome, bin string
}

// newEnv is a temp HOME with a Claude Code config dir (CLAUDE_CONFIG_DIR when viaEnv, else HOME/.claude) and a
// Teamflow home; the binary path has a space, so it must be quoted in the hook commands.
func newEnv(t *testing.T, viaEnv bool) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{home: filepath.Join(root, "home"), bin: filepath.Join(root, "my bin", "teamflow")}
	must(t, os.MkdirAll(e.home, 0o755))
	t.Setenv("HOME", e.home)
	e.tfHome = filepath.Join(e.home, ".teamflow")
	must(t, os.Mkdir(e.tfHome, 0o700))
	t.Setenv("TEAMFLOW_HOME", e.tfHome)
	if viaEnv {
		e.cfg = filepath.Join(root, "claude-config")
		t.Setenv("CLAUDE_CONFIG_DIR", e.cfg)
	} else {
		e.cfg = filepath.Join(e.home, ".claude")
		t.Setenv("CLAUDE_CONFIG_DIR", "")
	}
	return e
}

func (e *env) settings() string { return filepath.Join(e.cfg, "settings.json") }
func (e *env) claudeJSON() string {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return filepath.Join(e.cfg, ".claude.json")
	}
	return filepath.Join(e.home, ".claude.json")
}
func (e *env) claudeMD() string { return filepath.Join(e.cfg, "CLAUDE.md") }
func (e *env) teamf() string    { return filepath.Join(e.cfg, "commands", "teamf.md") }

func (e *env) write(t *testing.T, path, content string, mode os.FileMode) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		panic(err)
	}
}

// snapshot is every file under the config dir and the claude json, by path.
func (e *env) snapshot(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range []string{e.cfg, e.claudeJSON()} {
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				out[p] = string(b)
			}
			return nil
		})
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	must(t, json.Unmarshal([]byte(read(t, path)), &out))
	return out
}

// ourCommands are the hook commands Teamflow wrote, by event.
func ourCommands(t *testing.T, settings map[string]any) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	hooks, _ := settings["hooks"].(map[string]any)
	for ev, groups := range hooks {
		for _, g := range groups.([]any) {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				cmd := h.(map[string]any)["command"].(string)
				if strings.Contains(cmd, " hook claude-code ") {
					out[ev] = append(out[ev], cmd)
				}
			}
		}
	}
	return out
}

// A fresh install: MCP server, hooks for the five events, the instruction text, /teamf and the deny rule for the
// Teamflow home, each where Claude Code reads it; a second install changes nothing.
func TestInstallFresh(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		t.Run(map[bool]string{false: "HOME/.claude", true: "CLAUDE_CONFIG_DIR"}[viaEnv], func(t *testing.T) {
			e := newEnv(t, viaEnv)
			a := New()
			if a.Installed() {
				t.Fatal("installed before install")
			}
			lines, err := a.Install(e.bin)
			must(t, err)
			for _, l := range lines {
				if strings.Contains(l, e.home) {
					t.Fatalf("a home path in the output: %q", l)
				}
			}
			s := decode(t, e.settings())
			quoted := "'" + e.bin + "'"
			cmds := ourCommands(t, s)
			for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop", "SessionEnd"} {
				if len(cmds[ev]) != 1 || cmds[ev][0] != quoted+" hook claude-code "+ev {
					t.Fatalf("%s hooks: %v", ev, cmds[ev])
				}
			}
			group := s["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
			if h := group["hooks"].([]any)[0].(map[string]any); h["timeout"] != float64(15) || h["type"] != "command" {
				t.Fatalf("hook entry %v", h)
			}
			deny := s["permissions"].(map[string]any)["deny"].([]any)
			for _, want := range []string{"Read(~/.teamflow)", "Read(~/.teamflow/**)", "Edit(~/.teamflow/**)", "Bash(*~/.teamflow*)", "Bash(*$HOME/.teamflow*)", "Bash(*.teamflow/*)"} {
				if !containsAny(deny, want) {
					t.Fatalf("deny lacks %s: %v", want, deny)
				}
			}
			mcp := decode(t, e.claudeJSON())["mcpServers"].(map[string]any)["teamflow"].(map[string]any)
			if mcp["command"] != e.bin || len(mcp["args"].([]any)) != 1 || mcp["args"].([]any)[0] != "mcp" || mcp["type"] != "stdio" {
				t.Fatalf("mcp entry %v", mcp)
			}
			if fi, _ := os.Stat(e.claudeJSON()); fi.Mode().Perm() != 0o600 {
				t.Fatalf("a new claude json must be owner-only: %v", fi.Mode())
			}
			md := read(t, e.claudeMD())
			if !strings.Contains(md, adapters.BlockBegin+"\n"+protocol.AgentInstructions+adapters.BlockEnd) {
				t.Fatal("the instruction text is not in CLAUDE.md word for word")
			}
			teamf := read(t, e.teamf())
			if !strings.Contains(teamf, "`"+quoted+" team --relay $ARGUMENTS`") || strings.Contains(teamf, "{TEAMFLOW}") ||
				strings.Contains(teamf, "{ARGUMENTS}") {
				t.Fatalf("teamf.md:\n%s", teamf)
			}
			if !a.Installed() {
				t.Fatal("not Installed after install")
			}
			before := e.snapshot(t)
			_, err = a.Install(e.bin)
			must(t, err)
			if after := e.snapshot(t); !sameFiles(before, after) {
				t.Fatalf("a second install changed files:\n%v", diffFiles(before, after))
			}
			_, err = a.Uninstall()
			must(t, err)
			if a.Installed() {
				t.Fatal("still installed")
			}
			s = decode(t, e.settings())
			if len(s) != 0 {
				t.Fatalf("settings left: %v", s)
			}
			if _, err := os.Stat(e.teamf()); err == nil {
				t.Fatal("teamf.md left")
			}
			if _, err := os.Stat(e.claudeMD()); err == nil {
				t.Fatal("a CLAUDE.md that install made is left")
			}
			if _, err := os.Stat(e.claudeJSON()); err == nil {
				t.Fatal("a claude json that install made is left")
			}
		})
	}
}

// The person's own settings, hooks, MCP servers, deny rules and instructions stay byte for byte through install and
// uninstall; settings.json is backed up once.
func TestInstallKeepsThePersonsOwn(t *testing.T) {
	e := newEnv(t, true)
	settings := `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "my-guard --strict",
            "timeout": 30
          }
        ]
      }
    ],
    "Notification": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "say done"
          }
        ]
      }
    ]
  },
  "permissions": {
    "allow": [
      "Bash(git status)"
    ],
    "deny": [
      "Read(~/.teamflow/**)",
      "Read(./.env)"
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "echo «x» & <y>"
  }
}
`
	claudeJSON := `{
  "numStartups": 42,
  "mcpServers": {
    "other": {
      "type": "stdio",
      "command": "other-mcp",
      "args": [],
      "env": {
        "TOKEN": "x"
      }
    }
  },
  "projects": {
    "/w": {
      "allowedTools": []
    }
  }
}
`
	md := "# My rules\n\nBe brief.\n"
	e.write(t, e.settings(), settings, 0o644)
	e.write(t, e.claudeJSON(), claudeJSON, 0o600)
	e.write(t, e.claudeMD(), md, 0o644)
	a := New()
	_, err := a.Install(e.bin)
	must(t, err)
	if read(t, e.settings()+".teamflow-backup") != settings {
		t.Fatal("no backup of the settings as they were")
	}
	s := decode(t, e.settings())
	if s["model"] != "opus" || !strings.Contains(read(t, e.settings()), `"command": "my-guard --strict"`) {
		t.Fatal("the person's settings were not kept")
	}
	if !strings.Contains(read(t, e.claudeJSON()), `"other": {`) {
		t.Fatal("the person's MCP server was not kept")
	}
	if !strings.HasPrefix(read(t, e.claudeMD()), md) {
		t.Fatal("the person's CLAUDE.md was not kept")
	}
	// A second install keeps the first backup (the person's own settings, not Teamflow's).
	_, err = a.Install(e.bin)
	must(t, err)
	if read(t, e.settings()+".teamflow-backup") != settings {
		t.Fatal("the backup was overwritten")
	}
	_, err = a.Uninstall()
	must(t, err)
	if got := read(t, e.settings()); got != settings {
		t.Fatalf("settings not restored byte for byte:\n%s", got)
	}
	if got := read(t, e.claudeJSON()); got != claudeJSON {
		t.Fatalf("claude json not restored byte for byte:\n%s", got)
	}
	if got := read(t, e.claudeMD()); got != md {
		t.Fatalf("CLAUDE.md not restored: %q", got)
	}
}

// Over a Python-era install: its hook groups (with the guard's matcher), its MCP entry and its old /team command are
// replaced, never doubled; the person's own entries beside them stay; uninstall leaves only the person's.
func TestInstallOverPythonEra(t *testing.T) {
	e := newEnv(t, false)
	py := func(ev string) string { return `/opt/tf/venv/bin/python -m tf_cli hook claude-code ` + ev }
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo mine"}}},
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": py("SessionStart"), "timeout": 15}}},
			},
			"PreToolUse": []any{map[string]any{"matcher": "Bash|Read|Edit|Write|Grep|Glob|NotebookEdit",
				"hooks": []any{map[string]any{"type": "command", "command": py("PreToolUse"), "timeout": 15}}}},
			"Stop":             []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": py("Stop"), "timeout": 15}}}},
			"SessionEnd":       []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": py("SessionEnd"), "timeout": 15}}}},
			"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": py("UserPromptSubmit"), "timeout": 15}}}},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	e.write(t, e.settings(), string(b)+"\n", 0o644)
	e.write(t, e.claudeJSON(), `{"mcpServers": {"teamflow": {"type": "stdio", "command": "/opt/tf/venv/bin/python", "args": ["-m", "tf_cli", "mcp"], "env": {}, "alwaysLoad": true}, "mine": {"command": "x"}}}`, 0o600)
	e.write(t, filepath.Join(e.cfg, "commands", "team.md"), "Teamflow: run `/opt/tf team $ARGUMENTS`\n", 0o644)
	e.write(t, filepath.Join(e.cfg, "commands", "mine.md"), "mine\n", 0o644)
	if !New().Installed() {
		t.Fatal("a Python-era install counts as installed")
	}
	_, err := New().Install(e.bin)
	must(t, err)
	s := decode(t, e.settings())
	cmds := ourCommands(t, s)
	for ev, cs := range cmds {
		if len(cs) != 1 || !strings.HasPrefix(cs[0], "'"+e.bin+"'") {
			t.Fatalf("%s: %v (doubled or the Python one kept)", ev, cs)
		}
	}
	ss := s["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(ss) != 2 || ss[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "echo mine" {
		t.Fatalf("the person's SessionStart hook: %v", ss)
	}
	pre := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 || pre[0].(map[string]any)["matcher"] != nil {
		t.Fatalf("PreToolUse keeps the guard's matcher: %v", pre)
	}
	servers := decode(t, e.claudeJSON())["mcpServers"].(map[string]any)
	if servers["teamflow"].(map[string]any)["command"] != e.bin || servers["mine"] == nil {
		t.Fatalf("mcp servers %v", servers)
	}
	if _, err := os.Stat(filepath.Join(e.cfg, "commands", "team.md")); err == nil {
		t.Fatal("the old /team command of ours is left")
	}
	if read(t, filepath.Join(e.cfg, "commands", "mine.md")) != "mine\n" {
		t.Fatal("the person's command changed")
	}
	_, err = New().Uninstall()
	must(t, err)
	s = decode(t, e.settings())
	if got := ourCommands(t, s); len(got) != 0 {
		t.Fatalf("left: %v", got)
	}
	if ss := s["hooks"].(map[string]any)["SessionStart"].([]any); len(ss) != 1 {
		t.Fatalf("the person's hook: %v", ss)
	}
	if servers := decode(t, e.claudeJSON())["mcpServers"].(map[string]any); servers["teamflow"] != nil || servers["mine"] == nil {
		t.Fatalf("mcp after uninstall %v", servers)
	}
}

// Sad paths: a malformed settings.json or claude json, hooks or deny of the wrong shape, a damaged CLAUDE.md block,
// or a read-only settings.json: refused with a message, and nothing at all is written anywhere.
func TestInstallRefusals(t *testing.T) {
	cases := map[string]func(e *env){
		"malformed settings.json": func(e *env) { e.write(nil, e.settings(), `{"hooks": {`, 0o644) },
		"malformed claude json":   func(e *env) { e.write(nil, e.claudeJSON(), `{"mcpServers": nope}`, 0o600) },
		"hooks not an object":     func(e *env) { e.write(nil, e.settings(), `{"hooks": []}`, 0o644) },
		"an event not a list":     func(e *env) { e.write(nil, e.settings(), `{"hooks": {"Stop": {}}}`, 0o644) },
		"deny not a list":         func(e *env) { e.write(nil, e.settings(), `{"permissions": {"deny": "x"}}`, 0o644) },
		"mcpServers not an object": func(e *env) {
			e.write(nil, e.claudeJSON(), `{"mcpServers": []}`, 0o600)
		},
		"a damaged CLAUDE.md block": func(e *env) { e.write(nil, e.claudeMD(), adapters.BlockBegin+"\nhalf\n", 0o644) },
		"read-only settings.json":   func(e *env) { e.write(nil, e.settings(), `{}`, 0o444) },
		"read-only claude json":     func(e *env) { e.write(nil, e.claudeJSON(), `{}`, 0o400) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			setup(e)
			before := e.snapshot(t)
			lines, err := New().Install(e.bin)
			if err == nil {
				t.Fatalf("installed: %v", lines)
			}
			if after := e.snapshot(t); !sameFiles(before, after) {
				t.Fatalf("files changed on a refusal: %v (%v)", diffFiles(before, after), err)
			}
			if strings.Contains(err.Error(), e.home) {
				t.Fatalf("a home path in the message: %v", err)
			}
		})
	}
	// Uninstall refuses a malformed file too, and changes nothing.
	e := newEnv(t, true)
	e.write(t, e.settings(), `{"hooks": {`, 0o644)
	if _, err := New().Uninstall(); err == nil {
		t.Fatal("uninstalled from a malformed settings.json")
	}
	if read(t, e.settings()) != `{"hooks": {` {
		t.Fatal("a malformed settings.json was changed")
	}
}

// Uninstall with nothing installed changes nothing and is not an error; a /teamf command that is not Teamflow's
// is left alone.
func TestUninstallNothing(t *testing.T) {
	e := newEnv(t, true)
	e.write(t, e.teamf(), "my own teamf\n", 0o644)
	e.write(t, e.settings(), "{\n  \"model\": \"x\"\n}\n", 0o644)
	before := e.snapshot(t)
	_, err := New().Uninstall()
	must(t, err)
	if after := e.snapshot(t); !sameFiles(before, after) {
		t.Fatalf("changed: %v", diffFiles(before, after))
	}
}

// Hooks firing while Teamflow is being installed (and two installs racing) never break either: the settings end up
// valid with one set of Teamflow hooks, and every hook answers.
func TestConcurrentHooksAndInstalls(t *testing.T) {
	e := newEnv(t, true)
	e.write(t, e.settings(), "{\n  \"model\": \"x\"\n}\n", 0o644)
	a := New()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Install(e.bin); err != nil {
				errs <- err
			}
		}()
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = adapters.RunHook(a, "UserPromptSubmit", []byte(`{"session_id": "s1", "prompt": "hi"}`), core.Open)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	cmds := ourCommands(t, decode(t, e.settings()))
	for ev, cs := range cmds {
		if len(cs) != 1 {
			t.Fatalf("%s doubled by racing installs: %v", ev, cs)
		}
	}
	if len(cmds) != 5 {
		t.Fatalf("events %v", cmds)
	}
}

// Detect: Claude Code's config folder or its binary on the PATH.
func TestDetect(t *testing.T) {
	e := newEnv(t, true)
	t.Setenv("PATH", t.TempDir())
	if New().Detect() {
		t.Fatal("detected with nothing there")
	}
	must(t, os.MkdirAll(e.cfg, 0o755))
	if !New().Detect() {
		t.Fatal("not detected with its config folder")
	}
}

func containsAny(xs []any, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func sameFiles(a, b map[string]string) bool { return len(diffFiles(a, b)) == 0 }

func diffFiles(a, b map[string]string) []string {
	var out []string
	for p, v := range a {
		if w, ok := b[p]; !ok || w != v {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p+" (new)")
		}
	}
	return out
}

// A Python-era settings.json with only Teamflow's Stop hook: after install and uninstall it is as it was before
// Teamflow (the person's own key kept, the Python entry and the hooks it lived in gone).
func TestPythonOnlyHookRestoredAfterUninstall(t *testing.T) {
	e := newEnv(t, true)
	e.write(t, e.settings(), `{
  "model": "x",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/opt/tf/bin/teamflow hook claude-code Stop",
            "timeout": 15
          }
        ]
      }
    ]
  }
}
`, 0o644)
	_, err := New().Install(e.bin)
	must(t, err)
	_, err = New().Uninstall()
	must(t, err)
	if got := read(t, e.settings()); got != "{\n  \"model\": \"x\"\n}\n" {
		t.Fatalf("not as before Teamflow:\n%s", got)
	}
}

// CLAUDE.md: one install made is removed with the block; one the person had (even empty) stays.
func TestClaudeMDMissingVersusEmpty(t *testing.T) {
	e := newEnv(t, true)
	_, err := New().Install(e.bin)
	must(t, err)
	_, err = New().Install(e.bin) // a reinstall still knows install made it
	must(t, err)
	_, err = New().Uninstall()
	must(t, err)
	if _, err := os.Stat(e.claudeMD()); err == nil {
		t.Fatal("the CLAUDE.md install made is left")
	}
	e.write(t, e.claudeMD(), "", 0o644)
	_, err = New().Install(e.bin)
	must(t, err)
	_, err = New().Uninstall()
	must(t, err)
	if b, err := os.ReadFile(e.claudeMD()); err != nil || len(b) != 0 {
		t.Fatalf("the person's empty CLAUDE.md: %q %v", b, err)
	}
	// The person wrote into a CLAUDE.md install made: it stays, with their words.
	must(t, os.Remove(e.claudeMD()))
	_, err = New().Install(e.bin)
	must(t, err)
	must(t, os.WriteFile(e.claudeMD(), []byte(read(t, e.claudeMD())+"mine\n"), 0o644))
	_, err = New().Uninstall()
	must(t, err)
	if got := read(t, e.claudeMD()); got != "mine\n" {
		t.Fatalf("the person's words in a made CLAUDE.md: %q", got)
	}
}

// The hook before each tool goes through core.MidTurn: 50 tool steps make at most one network sync (the throttle's
// own sad paths, a black-holed server and a live background sync, are tested in core).
func TestPreToolUseStaysOffTheNetwork(t *testing.T) {
	m := newMachine(t, true)
	m.share("huy-1", Huy)
	for range 50 {
		m.hook("PreToolUse", `{"session_id": "huy-1", "tool_name": "Bash"}`)
	}
	m.srv.mu.Lock()
	polls := 0
	for _, c := range m.srv.calls {
		if c == "sync.poll" {
			polls++
		}
	}
	m.srv.mu.Unlock()
	if polls > 1 {
		t.Fatalf("50 tool steps made %d syncs", polls)
	}
}
