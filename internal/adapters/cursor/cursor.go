// Package cursor is the Cursor CLI adapter: it adds Teamflow's hooks to ~/.cursor/hooks.json, an allow rule for
// teamflow commands and deny rules for the Teamflow home to ~/.cursor/cli-config.json, the one instruction text as
// an always-on rule, and /teamf as a skill. Agents use the teamflow CLI for everything (no MCP server, owner
// 2026-11-25). Everything it writes is Teamflow's own entry or
// file; the person's settings stay as they were, and uninstall takes only Teamflow's out.
//
//	~/.cursor/hooks.json             hooks (Teamflow's entries beside the person's)
//	~/.cursor/cli-config.json        permissions.allow: Shell(teamflow) and Shell(<binary>);
//	                                 permissions.deny: Read(...) and Write(...) for the Teamflow home
//	~/.cursor/rules/teamflow.mdc     the instruction text (alwaysApply)
//	~/.cursor/skills/teamf/SKILL.md  /teamf
//
// Checked against https://cursor.com/docs/agent/hooks, /docs/context/mcp, /docs/cli/reference/permissions,
// /docs/context/rules and /docs/context/skills (2026-11-25). Cursor CLI is not installed on the machine this was
// built on: every path and format here is from the docs, to verify on a real install. In particular the docs name
// no global, always-read instruction file for the CLI (user rules live in Cursor's settings), so the rule file is
// a best effort and the sessionStart hook gives the instruction text as well; and a forum report says the CLI may
// not load the user-level hooks.json (Cursor staff say it does).
package cursor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

const (
	// Name is the agent's name for `teamflow install -a`.
	Name = "cursor"
	mark = " hook " + Name + " "
	// hookTimeout is the seconds Cursor gives a hook.
	hookTimeout = 15
	ruleMark    = "teamflow:rule"
)

// Events are the hooks Teamflow adds, in order.
var Events = []string{"sessionStart", "beforeSubmitPrompt", "beforeShellExecution", "postToolUse", "stop", "sessionEnd"}

// Adapter is the Cursor CLI adapter.
type Adapter struct{}

// New is the Cursor CLI adapter.
func New() *Adapter { return &Adapter{} }

func init() { adapters.Register(New()) }

// Name is "cursor".
func (*Adapter) Name() string { return Name }

// ConfigDir is Cursor's user folder, ~/.cursor.
func ConfigDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".cursor")
}

func hooksPath() string    { return filepath.Join(ConfigDir(), "hooks.json") }
func cliPath() string      { return filepath.Join(ConfigDir(), "cli-config.json") }
func rulePath() string     { return filepath.Join(ConfigDir(), "rules", "teamflow.mdc") }
func skillPath() string    { return filepath.Join(ConfigDir(), "skills", "teamf", "SKILL.md") }
func manifestPath() string { return filepath.Join(ConfigDir(), ".teamflow-install.json") }

// Detect says whether Cursor's CLI is here: `cursor-agent` or `agent` on the PATH, or the ~/.cursor folder.
func (*Adapter) Detect() bool {
	for _, b := range []string{"cursor-agent", "agent"} {
		if _, err := exec.LookPath(b); err == nil {
			return true
		}
	}
	fi, err := os.Stat(ConfigDir())
	return err == nil && fi.IsDir()
}

// Installed says whether Teamflow's hooks are in hooks.json.
func (*Adapter) Installed() bool {
	doc, err := adapters.LoadJSON(hooksPath())
	if err != nil {
		return false
	}
	hooks, _ := adapters.Obj(adapters.Field(doc, "hooks"))
	for _, kv := range hooks {
		entries, _ := kv.Value.([]any)
		if slices.ContainsFunc(entries, ours) {
			return true
		}
	}
	return false
}

func ours(h any) bool {
	o, ok := h.(ojson.Obj)
	return ok && strings.Contains(adapters.Str(adapters.Field(o, "command")), mark)
}

// DenyRules keep Cursor's file tools out of the Teamflow home (absolute paths: the docs do not say ~ is expanded).
// Shell rules match only a command's first word, so shell commands naming the home are refused by the
// beforeShellExecution hook instead.
func DenyRules() []string {
	abs, _ := adapters.HomeForms()
	return []string{"Read(" + abs + ")", "Read(" + abs + "/**)", "Write(" + abs + ")", "Write(" + abs + "/**)"}
}

func ruleText() string {
	return "---\ndescription: Teamflow (" + ruleMark + ", written by `teamflow install`; `teamflow uninstall` removes it)\n" +
		"alwaysApply: true\n---\n" + protocol.AgentInstructions
}

// checkHooks refuses a hooks.json whose shape is not Cursor's.
func checkHooks(doc ojson.Obj) error {
	if err := adapters.CheckShape(doc, "hooks.json", "hooks", ""); err != nil {
		return err
	}
	hooks, _ := adapters.Obj(adapters.Field(doc, "hooks"))
	for _, kv := range hooks {
		if _, ok := kv.Value.([]any); !ok {
			return fmt.Errorf("hooks.json: hooks.%s is not a list; fix it by hand first", kv.Key)
		}
	}
	return nil
}

// stripHooks takes Teamflow's entries out of hooks.json; with m (uninstall) an event list, the hooks object and the
// version install made go when left empty.
func stripHooks(doc ojson.Obj, m *adapters.Manifest) (ojson.Obj, bool) {
	hooks, ok := adapters.Obj(adapters.Field(doc, "hooks"))
	if !ok {
		return doc, false
	}
	changed := false
	var out ojson.Obj
	for _, kv := range hooks {
		entries, _ := kv.Value.([]any)
		var kept []any
		for _, h := range entries {
			if ours(h) {
				changed = true
				continue
			}
			kept = append(kept, h)
		}
		if kept == nil {
			kept = []any{}
		}
		if m != nil && len(kept) == 0 && len(entries) > 0 && m.Made("hooks.json:hooks."+kv.Key) {
			continue
		}
		out = append(out, ojson.KV{Key: kv.Key, Value: kept})
	}
	if out == nil {
		out = ojson.Obj{}
	}
	if m != nil && m.Made("hooks.json:version") {
		doc, _ = adapters.Without(doc, "version")
	}
	if m != nil && len(out) == 0 && m.Made("hooks.json:hooks") {
		doc, _ = adapters.Without(doc, "hooks")
		return doc, changed
	}
	return doc.Set("hooks", out), changed
}

func installHooks(doc ojson.Obj, binary string, m *adapters.Manifest) ojson.Obj {
	doc, _ = stripHooks(doc, nil)
	if _, has := doc.Get("version"); !has {
		doc = append(ojson.Obj{{Key: "version", Value: 1}}, doc...)
		m.Mark("hooks.json:version")
	}
	hv, has := doc.Get("hooks")
	if !has {
		hv = ojson.Obj{}
		m.Mark("hooks.json:hooks")
	}
	hooks := hv.(ojson.Obj)
	for _, ev := range Events {
		v, has := hooks.Get(ev)
		if !has {
			v = []any{}
			m.Mark("hooks.json:hooks." + ev)
		}
		hooks = hooks.Set(ev, append(v.([]any), ojson.Obj{
			{Key: "command", Value: adapters.HookCommand(binary, Name, ev)},
			{Key: "timeout", Value: hookTimeout},
		}))
	}
	return doc.Set("hooks", hooks)
}

// AllowRules let the agent run teamflow commands without a prompt: by name (on the PATH) and by the binary's path.
// Shell rules match a command's first word (https://cursor.com/docs/cli/reference/permissions).
func AllowRules(binary string) []string {
	rules := []string{"Shell(teamflow)"}
	if binary != "" && filepath.Base(binary) != binary {
		rules = append(rules, "Shell("+binary+")")
	}
	return rules
}

// cliRules is cli-config.json with Teamflow's deny and allow rules.
func cliRules(doc ojson.Obj, binary string, m *adapters.Manifest) (ojson.Obj, error) {
	doc, err := adapters.AddRules(doc, "cli-config.json", "permissions", "deny", DenyRules(), &m.Deny, m)
	if err != nil {
		return nil, err
	}
	return adapters.AddRules(doc, "cli-config.json", "permissions", "allow", AllowRules(binary), &m.Allow, m)
}

// Install sets Teamflow up in Cursor's CLI with binary (an absolute path). Every file is checked before any is
// written: a malformed or read-only one refuses the whole install.
func (*Adapter) Install(binary string) ([]string, error) {
	unlock, err := adapters.InstallLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := adapters.CheckWritable(hooksPath(), cliPath(), rulePath(), skillPath(), manifestPath()); err != nil {
		return nil, err
	}
	m, err := adapters.LoadManifest(manifestPath())
	if err != nil {
		return nil, err
	}
	hooksDoc, err := adapters.LoadJSON(hooksPath())
	if err != nil {
		return nil, err
	}
	cli, err := adapters.LoadJSON(cliPath())
	if err != nil {
		return nil, err
	}
	if err := checkHooks(hooksDoc); err != nil {
		return nil, err
	}
	for _, list := range []string{"deny", "allow"} {
		if err := adapters.CheckShape(cli, "cli-config.json", "permissions", list); err != nil {
			return nil, err
		}
	}
	for _, p := range []string{rulePath(), skillPath()} {
		if b, _ := adapters.ReadFile(p); b != nil && !strings.Contains(string(b), ruleMark) && !strings.Contains(string(b), adapters.TeamfMark) {
			return nil, fmt.Errorf("%s is someone else's file; move it, then run the install again", home.Display(p))
		}
	}
	m.MarkDirs(ConfigDir(), ".", "rules", "skills", filepath.Join("skills", "teamf"))
	for _, f := range []string{"hooks.json", "cli-config.json"} {
		m.MarkFileIfNew(f, filepath.Join(ConfigDir(), f))
		m.Remember(f, filepath.Join(ConfigDir(), f))
	}

	// TODO(MCP removal follow-up): this is where the MCP server was registered (~/.cursor/mcp.json); agents use the
	// teamflow CLI instead, allowed below.
	newHooks := installHooks(hooksDoc, binary, &m)
	if _, err := cliRules(cli, binary, &m); err != nil {
		return nil, err
	}
	// The manifest first: a write failing half way still leaves uninstall knowing what install made.
	if err := m.Save(); err != nil {
		return nil, err
	}
	var done []string
	if err := adapters.SaveJSON(hooksPath(), newHooks, 0o644); err != nil {
		return done, err
	}
	done = append(done, fmt.Sprintf("hooks %s in %s", strings.Join(Events, ", "), home.Display(hooksPath())))
	err = adapters.UpdateJSON(cliPath(), 0o644, func(doc ojson.Obj) (ojson.Obj, error) { return cliRules(doc, binary, &m) })
	if err != nil {
		return done, err
	}
	done = append(done, "an allow rule for teamflow commands and deny rules for the Teamflow home in "+home.Display(cliPath()))
	if err := adapters.WriteFile(rulePath(), []byte(ruleText()), 0o644); err != nil {
		return done, err
	}
	done = append(done, "the Teamflow instructions in "+home.Display(rulePath()))
	if err := adapters.WriteFile(skillPath(), []byte(adapters.TeamfSkill(binary, "disable-model-invocation: true")), 0o644); err != nil {
		return done, err
	}
	done = append(done, "/teamf skill at "+home.Display(skillPath()))
	return done, m.Save()
}

// Uninstall takes out Teamflow's hooks, allow and deny rules, rule file and /teamf skill; the person's own
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
	hooksDoc, err := adapters.LoadJSON(hooksPath())
	if err != nil {
		return nil, err
	}
	if _, err := adapters.LoadJSON(cliPath()); err != nil {
		return nil, err
	}
	if err := checkHooks(hooksDoc); err != nil {
		return nil, err
	}
	var done []string
	if doc, changed := stripHooks(hooksDoc, &m); changed {
		if err := adapters.SaveOrRemove(hooksPath(), "hooks.json", doc, m, 0o644); err != nil {
			return done, err
		}
		done = append(done, "removed Teamflow's hooks from "+home.Display(hooksPath()))
	}
	changed := false
	err = adapters.UpdateJSONWith(cliPath(), 0o644, func(doc ojson.Obj) (ojson.Obj, error) {
		doc, allowed := adapters.StripRules(doc, "cli-config.json", "permissions", "allow", m.Allow, m)
		doc, changed = adapters.StripRules(doc, "cli-config.json", "permissions", "deny", m.Deny, m)
		changed = changed || allowed
		if changed && len(doc) == 0 && m.Made("cli-config.json") {
			return nil, nil
		}
		return doc, nil
	}, func(doc ojson.Obj) []byte { return m.Restored("cli-config.json", doc) })
	if err != nil {
		return done, err
	}
	if changed {
		done = append(done, "removed the allow and deny rules from "+home.Display(cliPath()))
	}
	for _, f := range []struct{ path, mark string }{{rulePath(), ruleMark}, {skillPath(), adapters.TeamfMark}} {
		if ok, err := adapters.RemoveIfOurs(f.path, f.mark, false); err != nil {
			return done, err
		} else if ok {
			done = append(done, "removed "+home.Display(f.path))
		}
	}
	if err := m.Remove(); err != nil {
		return done, err
	}
	m.RemoveDirs(ConfigDir())
	return done, nil
}
