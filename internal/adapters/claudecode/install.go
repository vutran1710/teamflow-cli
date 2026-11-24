// Package claudecode is the Claude Code adapter: it adds hooks (notices and approval requests at session start,
// before each prompt and tool, at the turn's end), places the one instruction text in CLAUDE.md, adds the /teamf
// command, an allow rule per teamflow work command (agents use the teamflow CLI for everything, with no prompt) and
// deny rules for the Teamflow home. Everything it writes is Teamflow's own entry; the person's settings stay as they
// were, and uninstall takes only Teamflow's out.
//
//	<config dir>/settings.json        hooks; permissions.allow for teamflow work commands; permissions.deny for the
//	                                  Teamflow home (backed up once)
//	<config dir>/CLAUDE.md            the instruction text, in a Teamflow block
//	<config dir>/commands/teamf.md    /teamf
//
// No MCP server is registered (owner, 2026-11-25: CLI only). An older install's mcpServers.teamflow in the claude
// json ($CLAUDE_CONFIG_DIR/.claude.json or ~/.claude.json) is taken out by install and by uninstall; a claude json
// without one is never read for anything else, nor written.
package claudecode

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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
	Name = "claude-code"
	// mark is in every hook command Teamflow writes (the Python client's too), so its entries are found again.
	mark    = " hook " + Name + " "
	mcpName = "teamflow"
	// hookTimeout is the seconds Claude Code gives a hook (as the Python client set).
	hookTimeout = 15
	// manifestName records what install added that was not Teamflow's by its look alone (deny rules, and the
	// containers it created), so uninstall takes out exactly that.
	manifestName = ".teamflow-install.json"
)

// Events are the hooks Teamflow adds, in order.
var Events = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop", "SessionEnd"}

// Adapter is the Claude Code adapter.
type Adapter struct{}

// New is the Claude Code adapter.
func New() *Adapter { return &Adapter{} }

func init() { adapters.Register(New()) }

// Name is "claude-code".
func (*Adapter) Name() string { return Name }

// ConfigDir is Claude Code's config folder: $CLAUDE_CONFIG_DIR, else ~/.claude.
func ConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

// ClaudeJSON is where Claude Code keeps its MCP servers (an older install put Teamflow's there): $CLAUDE_CONFIG_DIR/.claude.json, else ~/.claude.json.
func ClaudeJSON() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude.json")
}

func settingsPath() string { return filepath.Join(ConfigDir(), "settings.json") }
func claudeMD() string     { return filepath.Join(ConfigDir(), "CLAUDE.md") }
func teamfPath() string    { return filepath.Join(ConfigDir(), "commands", "teamf.md") }
func oldTeamPath() string  { return filepath.Join(ConfigDir(), "commands", "team.md") }
func manifestPath() string { return filepath.Join(ConfigDir(), manifestName) }

// Detect says whether Claude Code is here: its config folder, or `claude` on the PATH.
func (*Adapter) Detect() bool {
	if fi, err := os.Stat(ConfigDir()); err == nil && fi.IsDir() {
		return true
	}
	_, err := exec.LookPath("claude")
	return err == nil
}

// Installed says whether Teamflow's hooks are in Claude Code's settings (the Python client's count too).
func (*Adapter) Installed() bool {
	s, err := adapters.LoadJSON(settingsPath())
	if err != nil {
		return false
	}
	hooks, _ := adapters.Obj(get(s, "hooks"))
	for _, kv := range hooks {
		groups, _ := kv.Value.([]any)
		for _, g := range groups {
			for _, h := range hookEntries(g) {
				if ours(h) {
					return true
				}
			}
		}
	}
	return false
}

// ------------------------------------------------------------------ install

// manifest is what install created or added beyond Teamflow's own marked entries.
type manifest struct {
	Created []string // containers it made: hooks, hooks.<event>, permissions, permissions.allow, permissions.deny
	Allow   []string // allow rules it added
	Deny    []string // deny rules it added
	found   bool
}

func loadManifest() (manifest, error) {
	doc, err := adapters.LoadJSON(manifestPath())
	if err != nil {
		return manifest{}, err
	}
	b, _ := os.ReadFile(manifestPath())
	m := manifest{found: len(bytes.TrimSpace(b)) > 0}
	m.Created, m.Allow, m.Deny = strings_(get(doc, "created")), strings_(get(doc, "allow")), strings_(get(doc, "deny"))
	return m, nil
}

func (m manifest) save() error {
	slices.Sort(m.Created)
	m.Created = slices.Compact(m.Created)
	return adapters.SaveJSON(manifestPath(), ojson.Obj{{Key: "created", Value: anyList(m.Created)},
		{Key: "allow", Value: anyList(m.Allow)}, {Key: "deny", Value: anyList(m.Deny)}}, 0o644)
}

func (m manifest) created(path string) bool { return slices.Contains(m.Created, path) }

// Install sets Teamflow up in Claude Code with binary (an absolute path) in its hooks, allow rules and /teamf. Every
// file is checked before any is written: a malformed or read-only one refuses the whole install.
func (a *Adapter) Install(binary string) ([]string, error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	for _, p := range []string{settingsPath(), claudeMD(), teamfPath(), manifestPath()} {
		if err := adapters.Writable(p); err != nil {
			return nil, err
		}
	}
	oldMCP := hasOldMCP()
	if oldMCP {
		if err := adapters.Writable(ClaudeJSON()); err != nil {
			return nil, err
		}
	}
	settings, err := adapters.LoadJSON(settingsPath())
	if err != nil {
		return nil, err
	}
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	if _, err := adapters.HasBlock(claudeMD()); err != nil {
		return nil, err
	}
	newSettings, err := installSettings(settings, binary, &m)
	if err != nil {
		return nil, err
	}

	var done []string
	// The person's settings as they were before Teamflow first wrote them (never a file Teamflow made).
	if b, err := adapters.ReadFile(settingsPath()); err == nil && b != nil && !m.found {
		backup := settingsPath() + ".teamflow-backup"
		if _, err := os.Lstat(backup); errors.Is(err, fs.ErrNotExist) {
			if err := adapters.WriteFile(backup, b, 0o600); err != nil {
				return done, err
			}
			done = append(done, "backed up "+home.Display(settingsPath())+" to "+filepath.Base(backup))
		}
	}
	if err := adapters.SaveJSON(settingsPath(), newSettings, 0o644); err != nil {
		return done, err
	}
	done = append(done, fmt.Sprintf("hooks %s, an allow rule for teamflow commands and a deny rule for the Teamflow "+
		"home in %s", strings.Join(Events, ", "), home.Display(settingsPath())))
	if oldMCP {
		if line, err := removeOldMCP(m); err != nil {
			return done, err
		} else if line != "" {
			done = append(done, line)
		}
		// Taken out: the claude json and its mcpServers are no longer Teamflow's to remove.
		m.Created = slices.DeleteFunc(m.Created, func(c string) bool { return c == "claude.json" || c == "mcpServers" })
	}
	if _, err := os.Lstat(claudeMD()); errors.Is(err, fs.ErrNotExist) {
		m.Created = append(m.Created, "CLAUDE.md")
	}
	if err := adapters.PlaceBlock(claudeMD(), protocol.AgentInstructions); err != nil {
		return done, err
	}
	done = append(done, "the Teamflow instructions in "+home.Display(claudeMD()))
	if line, err := removeOldTeam(); err != nil {
		return done, err
	} else if line != "" {
		done = append(done, line)
	}
	if err := adapters.WriteFile(teamfPath(), []byte(teamfText(binary)), 0o644); err != nil {
		return done, err
	}
	done = append(done, "/teamf command at "+home.Display(teamfPath()))
	return done, m.save()
}

// hasOldMCP says whether the claude json holds Teamflow's MCP server, from an install made before agents used the
// CLI. A claude json that cannot be read as one says no: Teamflow has nothing else to do there.
func hasOldMCP() bool {
	cj, err := adapters.LoadJSON(ClaudeJSON())
	if err != nil {
		return false
	}
	servers, ok := adapters.Obj(get(cj, "mcpServers"))
	if !ok {
		return false
	}
	_, has := servers.Get(mcpName)
	return has
}

// removeOldMCP takes Teamflow's MCP server out of the claude json; mcpServers left empty goes when Teamflow made it
// (or with no record), and the file too when Teamflow made it and nothing is left.
func removeOldMCP(m manifest) (string, error) {
	err := updateJSON(ClaudeJSON(), func(doc ojson.Obj) (ojson.Obj, error) {
		servers, ok := adapters.Obj(get(doc, "mcpServers"))
		if !ok {
			return doc, nil
		}
		servers, _ = adapters.Without(servers, mcpName)
		if len(servers) == 0 && (m.created("mcpServers") || !m.found) {
			doc, _ = adapters.Without(doc, "mcpServers")
			if len(doc) == 0 && m.created("claude.json") {
				return nil, nil // the file an old install made, left empty, goes
			}
			return doc, nil
		}
		return doc.Set("mcpServers", servers), nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("removed MCP server '%s' from %s (agents use the teamflow command now)", mcpName,
		home.Display(ClaudeJSON())), nil
}

func lock() (func(), error) { return adapters.InstallLock() }

// updateJSON rewrites a JSON file Claude Code itself may be writing (adapters.UpdateJSON), 0600 when new.
func updateJSON(path string, change func(ojson.Obj) (ojson.Obj, error)) error {
	return adapters.UpdateJSON(path, 0o600, change)
}

// hookCommand is how Claude Code runs Teamflow's hook for an event.
func hookCommand(binary, event string) string {
	return shellQuote(binary) + " hook " + Name + " " + event
}

// installSettings is settings with Teamflow's earlier hooks taken out (its own or the Python client's) and its
// hooks and deny rules added; refused when the settings' shape is not Claude Code's.
func installSettings(s ojson.Obj, binary string, m *manifest) (ojson.Obj, error) {
	s, _, emptied, err := stripHooks(s, nil)
	if err != nil {
		return nil, err
	}
	// An event (or the whole hooks object) that held only Teamflow's earlier entries is Teamflow's: uninstall
	// takes it out again.
	for _, ev := range emptied {
		m.Created = append(m.Created, "hooks."+ev)
	}
	if hooks, ok := adapters.Obj(get(s, "hooks")); ok && len(emptied) > 0 && len(emptied) == len(hooks) {
		m.Created = append(m.Created, "hooks")
	}
	hooksV, has := s.Get("hooks")
	if !has {
		hooksV = ojson.Obj{}
		m.Created = append(m.Created, "hooks")
	}
	hooks := hooksV.(ojson.Obj)
	for _, ev := range Events {
		groupsV, has := hooks.Get(ev)
		if !has {
			groupsV = []any{}
			m.Created = append(m.Created, "hooks."+ev)
		}
		group := ojson.Obj{{Key: "hooks", Value: []any{ojson.Obj{
			{Key: "type", Value: "command"},
			{Key: "command", Value: hookCommand(binary, ev)},
			{Key: "timeout", Value: hookTimeout},
		}}}}
		hooks = hooks.Set(ev, append(groupsV.([]any), group))
	}
	s = s.Set("hooks", hooks)

	permsV, has := s.Get("permissions")
	if !has {
		permsV = ojson.Obj{}
		m.Created = append(m.Created, "permissions")
	}
	perms, ok := permsV.(ojson.Obj)
	if !ok {
		return nil, fmt.Errorf("%s: permissions is not an object; fix it by hand first", home.Display(settingsPath()))
	}
	if perms, m.Deny, err = addRules(perms, "deny", DenyRules(binary), m.Deny, m); err != nil {
		return nil, err
	}
	if perms, m.Allow, err = addRules(perms, "allow", AllowRules(binary), m.Allow, m); err != nil {
		return nil, err
	}
	return s.Set("permissions", perms), nil
}

// addRules is perms with Teamflow's rules in its list key (allow or deny): a rule an earlier install added and no
// longer wanted goes, a rule the person had already is theirs and stays theirs, and the rest are added. It returns
// the rules that are Teamflow's now.
func addRules(perms ojson.Obj, key string, want, before []string, m *manifest) (ojson.Obj, []string, error) {
	listV, has := perms.Get(key)
	if !has {
		listV = []any{}
		m.Created = append(m.Created, "permissions."+key)
	}
	list, ok := listV.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("%s: permissions.%s is not a list; fix it by hand first", home.Display(settingsPath()), key)
	}
	var kept []any
	for _, r := range list { // a rule an earlier install added and no longer wanted (the home moved) goes
		if rs, ok := r.(string); ok && slices.Contains(before, rs) && !slices.Contains(want, rs) {
			continue
		}
		kept = append(kept, r)
	}
	list = kept
	var added []string
	for _, r := range want {
		if slices.Contains(before, r) {
			added = append(added, r) // ours from before
			if !slices.ContainsFunc(list, func(x any) bool { return x == r }) {
				list = append(list, r)
			}
			continue
		}
		if slices.ContainsFunc(list, func(x any) bool { return x == r }) {
			continue // the person's own rule: never taken out
		}
		list = append(list, r)
		added = append(added, r)
	}
	if list == nil {
		list = []any{}
	}
	return perms.Set(key, list), added, nil
}

// AllowRules let every teamflow work command run with no prompt (adapters.WorkCommands; setup commands keep Claude
// Code's prompt): Bash(teamflow <command>:*), a prefix rule Claude Code checks per command, never across a chain
// (`teamflow status && curl …` still asks), and the same by the binary's path when it is called by one. The deny
// rules for the Teamflow home win over these, so a work command naming the home still asks.
func AllowRules(binary string) []string {
	heads := []string{"teamflow"}
	if binary != "" && filepath.Base(binary) != binary {
		heads = append(heads, shellQuote(binary)) // as the agent types it: quoted when it needs it
	}
	var rules []string
	for _, h := range heads {
		for _, c := range adapters.WorkCommands {
			rules = append(rules, "Bash("+h+" "+c+":*)")
		}
	}
	return rules
}

// DenyRules keep Claude Code's file tools, and shell commands that name it, out of the Teamflow home (the person's
// sign-in and keys). Bash rules are left out when the binary itself lives in the home (they would block /teamf).
func DenyRules(binary string) []string {
	dir, _ := filepath.Abs(home.Dir())
	h, _ := os.UserHomeDir()
	var p string
	var texts []string
	if rel, ok := strings.CutPrefix(dir, h+string(filepath.Separator)); ok && h != "" && h != "/" {
		p = "~/" + rel
		texts = []string{"~/" + rel, "$HOME/" + rel, "${HOME}/" + rel, dir}
	} else {
		p = "/" + dir // Claude Code reads //path as absolute
		texts = []string{dir}
	}
	rules := []string{"Read(" + p + ")", "Read(" + p + "/**)", "Edit(" + p + ")", "Edit(" + p + "/**)"}
	if !strings.HasPrefix(binary, dir+string(filepath.Separator)) && !strings.Contains(binary, ".teamflow/") {
		for _, t := range texts {
			rules = append(rules, "Bash(*"+t+"*)")
		}
		rules = append(rules, "Bash(*.teamflow/*)", "Bash(*TEAMFLOW_HOME*)")
	}
	return rules
}

// teamfText is /teamf for Claude Code: its frontmatter (from the one text's first lines), then the one text with
// the binary and Claude Code's $ARGUMENTS filled in.
func teamfText(binary string) string {
	body := strings.NewReplacer("{TEAMFLOW}", shellQuote(binary), "{ARGUMENTS}", "$ARGUMENTS").Replace(protocol.AgentTeamf)
	desc, _, _ := strings.Cut(protocol.AgentTeamf, "\n\n")
	hint := ""
	for _, line := range strings.Split(protocol.AgentTeamf, "\n") {
		if h, ok := strings.CutPrefix(line, "Arguments: "); ok {
			hint = h
		}
	}
	return "---\n" +
		"description: " + strings.Join(strings.Fields(desc), " ") + "\n" +
		"argument-hint: " + hint + "\n" +
		"allowed-tools: Bash(" + shellQuote(binary) + " team *)\n" +
		"---\n" + body
}

// removeOldTeam removes Teamflow's earlier /team command (renamed /teamf); someone else's /team stays.
func removeOldTeam() (string, error) {
	b, err := adapters.ReadFile(oldTeamPath())
	if err != nil || b == nil {
		return "", err
	}
	if !strings.Contains(string(b), " team $ARGUMENTS`") || !strings.Contains(string(b), "Teamflow") {
		return "", nil
	}
	if err := os.Remove(oldTeamPath()); err != nil {
		return "", err
	}
	return "removed the old /team command " + home.Display(oldTeamPath()), nil
}

// ------------------------------------------------------------------ uninstall

// Uninstall takes out Teamflow's hooks, allow and deny rules, an old MCP server, instruction block and /teamf (and an old /team of
// Teamflow's); the person's own entries stay as they were.
func (a *Adapter) Uninstall() ([]string, error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	settings, err := adapters.LoadJSON(settingsPath())
	if err != nil {
		return nil, err
	}
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	if _, err := adapters.HasBlock(claudeMD()); err != nil {
		return nil, err
	}
	// Every file it may write or remove, checked before any is changed: a refusal leaves everything as it was.
	check := []string{settingsPath(), claudeMD(), teamfPath(), oldTeamPath(), manifestPath()}
	if hasOldMCP() {
		check = append(check, ClaudeJSON())
	}
	for _, p := range check {
		if err := adapters.Writable(p); err != nil {
			return nil, err
		}
	}
	var done []string
	s, changed, _, err := stripHooks(settings, &m)
	if err != nil {
		return nil, err
	}
	s, denyChanged := stripRules(s, "deny", m.Deny, m)
	s, allowChanged := stripRules(s, "allow", m.Allow, m)
	denyChanged = denyChanged || allowChanged
	if changed || denyChanged {
		if err := adapters.SaveJSON(settingsPath(), s, 0o644); err != nil {
			return done, err
		}
		done = append(done, "removed Teamflow's hooks, allow and deny rules from "+home.Display(settingsPath()))
	}
	if hasOldMCP() { // left by an install from before agents used the CLI
		line, err := removeOldMCP(m)
		if err != nil {
			return done, err
		}
		done = append(done, line)
	}
	if removed, err := adapters.RemoveBlock(claudeMD()); err != nil {
		return done, err
	} else if removed {
		done = append(done, "removed the Teamflow instructions from "+home.Display(claudeMD()))
		// A CLAUDE.md install made, left empty, goes too; one the person had, or wrote into, stays.
		if b, err := adapters.ReadFile(claudeMD()); err == nil && len(b) == 0 && m.created("CLAUDE.md") {
			if err := os.Remove(claudeMD()); err != nil {
				return done, err
			}
		}
	}
	if b, err := adapters.ReadFile(teamfPath()); err != nil {
		return done, err
	} else if b != nil && strings.Contains(string(b), " team --relay ") {
		if err := os.Remove(teamfPath()); err != nil {
			return done, err
		}
		done = append(done, "removed "+home.Display(teamfPath()))
	}
	if line, err := removeOldTeam(); err != nil {
		return done, err
	} else if line != "" {
		done = append(done, line)
	}
	if m.found {
		if err := os.Remove(manifestPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return done, err
		}
	}
	return done, nil
}

// stripRules takes out the rules of a permissions list (allow or deny) that install added, and the containers it
// made when they are left empty.
func stripRules(s ojson.Obj, key string, ours []string, m manifest) (ojson.Obj, bool) {
	perms, ok := adapters.Obj(get(s, "permissions"))
	if !ok {
		return s, false
	}
	list, ok := get(perms, key).([]any)
	if !ok {
		return s, false
	}
	var kept []any
	for _, r := range list {
		if rs, ok := r.(string); ok && slices.Contains(ours, rs) {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == len(list) && !(len(list) == 0 && m.created("permissions."+key)) {
		return s, false
	}
	if len(kept) == 0 && m.created("permissions."+key) {
		perms, _ = adapters.Without(perms, key)
	} else {
		if kept == nil {
			kept = []any{}
		}
		perms = perms.Set(key, kept)
	}
	if len(perms) == 0 && m.created("permissions") {
		s, _ = adapters.Without(s, "permissions")
		return s, true
	}
	return s.Set("permissions", perms), true
}

// stripHooks takes Teamflow's hook entries (its own or the Python client's) out of the settings; a group left with
// no hooks goes. With m (uninstall), an event list and the hooks object left empty go too when install made them
// (or, with no record, as the Python client did). Refused when hooks is not Claude Code's shape.
func stripHooks(s ojson.Obj, m *manifest) (ojson.Obj, bool, []string, error) {
	hooksV, has := s.Get("hooks")
	if !has {
		return s, false, nil, nil
	}
	hooks, ok := hooksV.(ojson.Obj)
	if !ok {
		return nil, false, nil, fmt.Errorf("%s: hooks is not an object; fix it by hand first", home.Display(settingsPath()))
	}
	changed := false
	var emptied []string
	var out ojson.Obj
	for _, kv := range hooks {
		groups, ok := kv.Value.([]any)
		if !ok {
			return nil, false, nil, fmt.Errorf("%s: hooks.%s is not a list; fix it by hand first", home.Display(settingsPath()), kv.Key)
		}
		var kept []any
		for _, g := range groups {
			obj, isObj := g.(ojson.Obj)
			entries := hookEntries(g)
			if !isObj || len(entries) == 0 {
				kept = append(kept, g)
				continue
			}
			var mine []any
			for _, h := range entries {
				if !ours(h) {
					mine = append(mine, h)
				}
			}
			switch {
			case len(mine) == len(entries):
				kept = append(kept, g)
			case len(mine) > 0:
				changed = true
				kept = append(kept, obj.Set("hooks", mine))
			default:
				changed = true
			}
		}
		if kept == nil {
			kept = []any{}
		}
		if len(kept) == 0 && len(groups) > 0 {
			emptied = append(emptied, kv.Key)
		}
		if m != nil && len(kept) == 0 && len(groups) > 0 && (m.created("hooks."+kv.Key) || !m.found) {
			continue
		}
		out = append(out, ojson.KV{Key: kv.Key, Value: kept})
	}
	if m != nil && len(out) == 0 && changed && (m.created("hooks") || !m.found) {
		s, _ = adapters.Without(s, "hooks")
		return s, true, emptied, nil
	}
	if out == nil {
		out = ojson.Obj{}
	}
	return s.Set("hooks", out), changed, emptied, nil
}

func hookEntries(g any) []any {
	obj, ok := g.(ojson.Obj)
	if !ok {
		return nil
	}
	hs, _ := get(obj, "hooks").([]any)
	return hs
}

func ours(h any) bool {
	obj, ok := h.(ojson.Obj)
	return ok && strings.Contains(adapters.Str(get(obj, "command")), mark)
}

func get(o ojson.Obj, key string) any {
	v, _ := o.Get(key)
	return v
}

func strings_(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func anyList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string { return adapters.ShellQuote(s) }
