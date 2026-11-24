// Package claudecode is the Claude Code adapter: it registers the MCP server in Claude Code's config, adds hooks
// (notices and approval requests at session start, before each prompt and tool, at the turn's end), places the one
// instruction text in CLAUDE.md, adds the /teamf command and a deny rule for the Teamflow home. Everything it
// writes is Teamflow's own entry; the person's settings stay as they were, and uninstall takes only Teamflow's out.
//
//	<config dir>/settings.json        hooks; permissions.deny for the Teamflow home (backed up once)
//	<claude json>                     mcpServers.teamflow
//	<config dir>/CLAUDE.md            the instruction text, in a Teamflow block
//	<config dir>/commands/teamf.md    /teamf
//
// The config dir is $CLAUDE_CONFIG_DIR or ~/.claude; the claude json $CLAUDE_CONFIG_DIR/.claude.json or
// ~/.claude.json.
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

// ClaudeJSON is where Claude Code keeps its MCP servers: $CLAUDE_CONFIG_DIR/.claude.json, else ~/.claude.json.
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
	Created []string // containers it made: hooks, hooks.<event>, permissions, permissions.deny, mcpServers
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
	m.Created, m.Deny = strings_(get(doc, "created")), strings_(get(doc, "deny"))
	return m, nil
}

func (m manifest) save() error {
	slices.Sort(m.Created)
	m.Created = slices.Compact(m.Created)
	return adapters.SaveJSON(manifestPath(), ojson.Obj{{Key: "created", Value: anyList(m.Created)},
		{Key: "deny", Value: anyList(m.Deny)}}, 0o644)
}

func (m manifest) created(path string) bool { return slices.Contains(m.Created, path) }

// Install sets Teamflow up in Claude Code with binary (an absolute path) in its hooks, MCP entry and /teamf. Every
// file is checked before any is written: a malformed or read-only one refuses the whole install.
func (a *Adapter) Install(binary string) ([]string, error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	for _, p := range []string{settingsPath(), ClaudeJSON(), claudeMD(), teamfPath(), manifestPath()} {
		if err := adapters.Writable(p); err != nil {
			return nil, err
		}
	}
	settings, err := adapters.LoadJSON(settingsPath())
	if err != nil {
		return nil, err
	}
	cj, err := adapters.LoadJSON(ClaudeJSON())
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
	if _, err := installMCP(cj, binary, &manifest{}); err != nil {
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
	done = append(done, fmt.Sprintf("hooks %s and a deny rule for the Teamflow home in %s", strings.Join(Events, ", "),
		home.Display(settingsPath())))
	if _, err := os.Lstat(ClaudeJSON()); errors.Is(err, fs.ErrNotExist) {
		m.Created = append(m.Created, "claude.json")
	}
	if err := updateJSON(ClaudeJSON(), func(doc ojson.Obj) (ojson.Obj, error) { return installMCP(doc, binary, &m) }); err != nil {
		return done, err
	}
	done = append(done, fmt.Sprintf("MCP server '%s' in %s", mcpName, home.Display(ClaudeJSON())))
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
	denyV, has := perms.Get("deny")
	if !has {
		denyV = []any{}
		m.Created = append(m.Created, "permissions.deny")
	}
	deny, ok := denyV.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: permissions.deny is not a list; fix it by hand first", home.Display(settingsPath()))
	}
	want := DenyRules(binary)
	var kept []any
	for _, r := range deny { // a rule an earlier install added and no longer wanted (the home moved) goes
		if rs, ok := r.(string); ok && slices.Contains(m.Deny, rs) && !slices.Contains(want, rs) {
			continue
		}
		kept = append(kept, r)
	}
	deny = kept
	var added []string
	for _, r := range want {
		if slices.Contains(m.Deny, r) {
			added = append(added, r) // ours from before
			if !slices.ContainsFunc(deny, func(x any) bool { return x == r }) {
				deny = append(deny, r)
			}
			continue
		}
		if slices.ContainsFunc(deny, func(x any) bool { return x == r }) {
			continue // the person's own rule: never taken out
		}
		deny = append(deny, r)
		added = append(added, r)
	}
	if deny == nil {
		deny = []any{}
	}
	m.Deny = added
	perms = perms.Set("deny", deny)
	return s.Set("permissions", perms), nil
}

// installMCP is the claude json with Teamflow's MCP server set (the same place when it was there).
func installMCP(doc ojson.Obj, binary string, m *manifest) (ojson.Obj, error) {
	serversV, has := doc.Get("mcpServers")
	if !has {
		serversV = ojson.Obj{}
		m.Created = append(m.Created, "mcpServers")
	}
	servers, ok := serversV.(ojson.Obj)
	if !ok {
		return nil, fmt.Errorf("%s: mcpServers is not an object; fix it by hand first", home.Display(ClaudeJSON()))
	}
	// alwaysLoad: Teamflow's tools are in every prompt, never deferred behind Claude Code's tool search, so an agent
	// that notices something to record has signal_emit's schema at hand.
	servers = servers.Set(mcpName, ojson.Obj{
		{Key: "type", Value: "stdio"},
		{Key: "command", Value: binary},
		{Key: "args", Value: []any{"mcp"}},
		{Key: "env", Value: ojson.Obj{}},
		{Key: "alwaysLoad", Value: true},
	})
	return doc.Set("mcpServers", servers), nil
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

// Uninstall takes out Teamflow's hooks, deny rules, MCP server, instruction block and /teamf (and an old /team of
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
	cj, err := adapters.LoadJSON(ClaudeJSON())
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
	var done []string
	s, changed, _, err := stripHooks(settings, &m)
	if err != nil {
		return nil, err
	}
	s, denyChanged := stripDeny(s, m)
	if changed || denyChanged {
		if err := adapters.SaveJSON(settingsPath(), s, 0o644); err != nil {
			return done, err
		}
		done = append(done, "removed Teamflow's hooks and deny rules from "+home.Display(settingsPath()))
	}
	if servers, ok := adapters.Obj(get(cj, "mcpServers")); ok {
		if _, has := servers.Get(mcpName); has {
			err := updateJSON(ClaudeJSON(), func(doc ojson.Obj) (ojson.Obj, error) {
				servers, _ := adapters.Obj(get(doc, "mcpServers"))
				servers, _ = adapters.Without(servers, mcpName)
				if len(servers) == 0 && (m.created("mcpServers") || !m.found) {
					doc, _ = adapters.Without(doc, "mcpServers")
					if len(doc) == 0 && m.created("claude.json") {
						return nil, nil // the file install made, left empty, goes
					}
					return doc, nil
				}
				return doc.Set("mcpServers", servers), nil
			})
			if err != nil {
				return done, err
			}
			done = append(done, fmt.Sprintf("removed MCP server '%s' from %s", mcpName, home.Display(ClaudeJSON())))
		}
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

// stripDeny takes out the deny rules install added, and the containers it made when they are left empty.
func stripDeny(s ojson.Obj, m manifest) (ojson.Obj, bool) {
	perms, ok := adapters.Obj(get(s, "permissions"))
	if !ok {
		return s, false
	}
	deny, ok := get(perms, "deny").([]any)
	if !ok {
		return s, false
	}
	var kept []any
	for _, r := range deny {
		if rs, ok := r.(string); ok && slices.Contains(m.Deny, rs) {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == len(deny) && !(len(deny) == 0 && m.created("permissions.deny")) {
		return s, false
	}
	if len(kept) == 0 && m.created("permissions.deny") {
		perms, _ = adapters.Without(perms, "deny")
	} else {
		if kept == nil {
			kept = []any{}
		}
		perms = perms.Set("deny", kept)
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
