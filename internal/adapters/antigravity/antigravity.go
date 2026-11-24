// Package antigravity is the Antigravity CLI (`agy`) adapter: it adds a "teamflow" hook to
// ~/.gemini/config/hooks.json, places the one instruction text in ~/.gemini/config/AGENTS.md, /teamf as a skill,
// and an allow rule for teamflow commands and deny rules for the Teamflow home in
// ~/.gemini/antigravity-cli/settings.json. Agents use the teamflow CLI for everything (no MCP server, owner
// 2026-11-25). Everything it writes is Teamflow's own entry or file; the person's
// settings stay as they were, and uninstall takes only Teamflow's out.
//
//	~/.gemini/config/hooks.json              the "teamflow" hook (SessionStart, PreInvocation, Stop)
//	~/.gemini/config/AGENTS.md               the instruction text, in a Teamflow block
//	~/.gemini/config/skills/teamf/SKILL.md   /teamf (the CLI makes each skill a slash command)
//	~/.gemini/antigravity-cli/settings.json  permissions.allow: command(teamflow <work command>), the same by path;
//	                                         permissions.deny: read_file, write_file and command rules
//
// Checked against https://antigravity.google/docs/hooks.md, /docs/mcp.md,
// /docs/permissions.md, /docs/rules.md and /docs/skills.md (2026-11-25). `agy` is not installed on the machine
// this was built on: every path and format here is from the docs, to verify on a real install.
package antigravity

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

const (
	// Name is the agent's name for `teamflow install -a`.
	Name     = "antigravity"
	mark     = " hook " + Name + " "
	hookName = "teamflow"
	// hookTimeout is the seconds Antigravity gives a hook.
	hookTimeout = 15
)

// Events are the hooks Teamflow adds, in order.
var Events = []string{"SessionStart", "PreInvocation", "Stop"}

// Adapter is the Antigravity CLI adapter.
type Adapter struct{}

// New is the Antigravity CLI adapter.
func New() *Adapter { return &Adapter{} }

func init() { adapters.Register(New()) }

// Name is "antigravity".
func (*Adapter) Name() string { return Name }

// Root is ~/.gemini, where Antigravity keeps its config.
func Root() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".gemini")
}

func cfg(rel string) string { return filepath.Join(Root(), "config", rel) }

func hooksPath() string    { return cfg("hooks.json") }
func agentsPath() string   { return cfg("AGENTS.md") }
func skillPath() string    { return cfg(filepath.Join("skills", "teamf", "SKILL.md")) }
func manifestPath() string { return cfg(".teamflow-install.json") }
func settingsPath() string { return filepath.Join(Root(), "antigravity-cli", "settings.json") }

// Detect says whether the Antigravity CLI is here: `agy` on the PATH, or its ~/.gemini/antigravity-cli folder.
func (*Adapter) Detect() bool {
	if _, err := exec.LookPath("agy"); err == nil {
		return true
	}
	fi, err := os.Stat(filepath.Dir(settingsPath()))
	return err == nil && fi.IsDir()
}

// Installed says whether Teamflow's hook is in hooks.json.
func (*Adapter) Installed() bool {
	doc, err := adapters.LoadJSON(hooksPath())
	if err != nil {
		return false
	}
	h, ok := adapters.Obj(adapters.Field(doc, hookName))
	if !ok {
		return false
	}
	b, _ := ojson.Marshal(h)
	return strings.Contains(string(b), mark)
}

func hookDef(binary string) ojson.Obj {
	def := ojson.Obj{}
	for _, ev := range Events {
		def = def.Set(ev, []any{ojson.Obj{
			{Key: "type", Value: "command"},
			{Key: "command", Value: adapters.HookCommand(binary, Name, ev)},
			{Key: "timeout", Value: hookTimeout},
		}})
	}
	return def
}

// DenyRules keep Antigravity's file tools out of the Teamflow home (a folder covers what is inside it; denying
// read_file blocks write_file too, both listed for clarity), and its shell commands that name the home. The command
// rules are left out when the binary itself lives in the home (they would block /teamf).
func DenyRules(binary string) []string {
	abs, texts := adapters.HomeForms()
	rules := []string{"read_file(" + abs + ")", "write_file(" + abs + ")"}
	if !adapters.BinaryInHome(binary) {
		for _, t := range append(texts, ".teamflow/", "TEAMFLOW_HOME") {
			rules = append(rules, "command(regex:.*"+regexp.QuoteMeta(t)+".*)")
		}
	}
	return rules
}

// AllowRules let the agent run teamflow work commands without a prompt, by name (on the PATH) and by the binary's
// path: one rule per work command, since command(prefix) matches word by word
// (https://antigravity.google/docs/permissions.md), so setup commands (install, login, self-update, …) keep the
// agent's prompt. sync is left out: its prefix would match `sync --stop`.
//
// Not covered, to verify on a real install: whether a prefix rule also matches a chained command
// (`teamflow status && curl …`). Antigravity's PreToolUse hook could answer "ask" for those, but its output's
// decision is required, and the docs give no neutral answer for every other command (allow may skip the
// person's own ask rules), so no PreToolUse hook is added until that is checked.
func AllowRules(binary string) []string {
	heads := []string{"teamflow"}
	if binary != "" && filepath.Base(binary) != binary {
		heads = append(heads, binary)
	}
	var rules []string
	for _, h := range heads {
		for _, c := range adapters.WorkCommands {
			if c != "sync" {
				rules = append(rules, "command("+h+" "+c+")")
			}
		}
	}
	return rules
}

// settingsRules is settings.json with Teamflow's deny and allow rules.
func settingsRules(doc ojson.Obj, binary string, m *adapters.Manifest) (ojson.Obj, error) {
	doc, err := adapters.AddRules(doc, "settings.json", "permissions", "deny", DenyRules(binary), &m.Deny, m)
	if err != nil {
		return nil, err
	}
	return adapters.AddRules(doc, "settings.json", "permissions", "allow", AllowRules(binary), &m.Allow, m)
}

// Install sets Teamflow up in the Antigravity CLI with binary (an absolute path). Every file is checked before any
// is written: a malformed or read-only one refuses the whole install.
func (*Adapter) Install(binary string) ([]string, error) {
	unlock, err := adapters.InstallLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := adapters.CheckWritable(hooksPath(), agentsPath(), skillPath(), settingsPath(), manifestPath()); err != nil {
		return nil, err
	}
	m, err := adapters.LoadManifest(manifestPath())
	if err != nil {
		return nil, err
	}
	hooks, err := adapters.LoadJSON(hooksPath())
	if err != nil {
		return nil, err
	}
	settings, err := adapters.LoadJSON(settingsPath())
	if err != nil {
		return nil, err
	}
	for _, list := range []string{"deny", "allow"} {
		if err := adapters.CheckShape(settings, "settings.json", "permissions", list); err != nil {
			return nil, err
		}
	}
	if _, err := adapters.HasBlock(agentsPath()); err != nil {
		return nil, err
	}
	if v, has := hooks.Get(hookName); has {
		b, _ := ojson.Marshal(v)
		if !strings.Contains(string(b), mark) {
			return nil, fmt.Errorf("hooks.json: a hook named %q is someone else's; rename it, then run the install again", hookName)
		}
	}
	if b, _ := adapters.ReadFile(skillPath()); b != nil && !strings.Contains(string(b), adapters.TeamfMark) {
		return nil, fmt.Errorf("%s is someone else's /teamf skill; move it, then run the install again", home.Display(skillPath()))
	}
	m.MarkDirs(Root(), ".", "config", filepath.Join("config", "skills"), filepath.Join("config", "skills", "teamf"), "antigravity-cli")
	for name, p := range map[string]string{"hooks.json": hooksPath(), "settings.json": settingsPath()} {
		m.MarkFileIfNew(name, p)
		m.Remember(name, p)
	}

	// TODO(MCP removal follow-up): this is where the MCP server was registered (~/.gemini/config/mcp_config.json);
	// agents use the teamflow CLI instead, allowed below.
	if _, err := settingsRules(settings, binary, &m); err != nil {
		return nil, err
	}
	m.MarkFileIfNew("AGENTS.md", agentsPath())
	// The manifest first: a write failing half way still leaves uninstall knowing what install made.
	if err := m.Save(); err != nil {
		return nil, err
	}
	var done []string
	if err := adapters.SaveJSON(hooksPath(), hooks.Set(hookName, hookDef(binary)), 0o644); err != nil {
		return done, err
	}
	done = append(done, fmt.Sprintf("hook '%s' (%s) in %s", hookName, strings.Join(Events, ", "), home.Display(hooksPath())))
	if err := adapters.PlaceInstructions(agentsPath(), "AGENTS.md", &m); err != nil {
		return done, err
	}
	done = append(done, "the Teamflow instructions in "+home.Display(agentsPath()))
	if err := adapters.WriteFile(skillPath(), []byte(adapters.TeamfSkill(binary)), 0o644); err != nil {
		return done, err
	}
	done = append(done, "/teamf skill at "+home.Display(skillPath()))
	err = adapters.UpdateJSON(settingsPath(), 0o644, func(doc ojson.Obj) (ojson.Obj, error) { return settingsRules(doc, binary, &m) })
	if err != nil {
		return done, err
	}
	done = append(done, "an allow rule for teamflow commands and deny rules for the Teamflow home in "+home.Display(settingsPath()))
	return done, m.Save()
}

// Uninstall takes out Teamflow's hook, instruction block, /teamf skill and allow and deny rules; the person's own
// entries stay as they were.
func (*Adapter) Uninstall() ([]string, error) {
	unlock, err := adapters.InstallLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m, err := adapters.LoadManifest(manifestPath())
	if err != nil {
		return nil, err
	}
	hooks, err := adapters.LoadJSON(hooksPath())
	if err != nil {
		return nil, err
	}
	if _, err := adapters.LoadJSON(settingsPath()); err != nil {
		return nil, err
	}
	if _, err := adapters.HasBlock(agentsPath()); err != nil {
		return nil, err
	}
	var done []string
	if v, has := hooks.Get(hookName); has {
		if b, _ := ojson.Marshal(v); strings.Contains(string(b), mark) {
			doc, _ := adapters.Without(hooks, hookName)
			if err := adapters.SaveOrRemove(hooksPath(), "hooks.json", doc, m, 0o644); err != nil {
				return done, err
			}
			done = append(done, fmt.Sprintf("removed hook '%s' from %s", hookName, home.Display(hooksPath())))
		}
	}
	if ok, err := adapters.RemoveInstructions(agentsPath(), "AGENTS.md", m); err != nil {
		return done, err
	} else if ok {
		done = append(done, "removed the Teamflow instructions from "+home.Display(agentsPath()))
	}
	if ok, err := adapters.RemoveIfOurs(skillPath(), adapters.TeamfMark, false); err != nil {
		return done, err
	} else if ok {
		done = append(done, "removed "+home.Display(skillPath()))
	}
	changed := false
	err = adapters.UpdateJSONWith(settingsPath(), 0o644, func(doc ojson.Obj) (ojson.Obj, error) {
		doc, allowed := adapters.StripRules(doc, "settings.json", "permissions", "allow", m.Allow, m)
		doc, changed = adapters.StripRules(doc, "settings.json", "permissions", "deny", m.Deny, m)
		changed = changed || allowed
		if changed && len(doc) == 0 && m.Made("settings.json") {
			return nil, nil
		}
		return doc, nil
	}, func(doc ojson.Obj) []byte { return m.Restored("settings.json", doc) })
	if err != nil {
		return done, err
	}
	if changed {
		done = append(done, "removed the allow and deny rules from "+home.Display(settingsPath()))
	}
	if err := m.Remove(); err != nil {
		return done, err
	}
	m.RemoveDirs(Root())
	return done, nil
}
