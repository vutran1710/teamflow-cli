// Package copilot is the GitHub Copilot CLI adapter: it adds a hooks file of its own, places the one instruction
// text in copilot-instructions.md and /teamf as a skill. Agents use the teamflow CLI for everything (no MCP server,
// owner 2026-11-25).
// Everything it writes is Teamflow's own entry or file; the person's settings stay as they were, and uninstall
// takes only Teamflow's out.
//
//	<config>/hooks/teamflow.json       Teamflow's hooks (a file of its own)
//	<config>/copilot-instructions.md   the instruction text, in a Teamflow block
//	<config>/skills/teamf/SKILL.md     /teamf (allowed-tools: shell(teamflow:*))
//
// The config folder is $COPILOT_HOME, else ~/.copilot. Checked against Copilot CLI 1.0.63 and
// https://docs.github.com/en/copilot/reference/hooks-configuration (2026-11-25).
//
// Copilot keeps no allow or deny rules in a user's config (only --allow-tool/--deny-tool on the command line, and
// managed policy), so the preToolUse hook answers instead: it allows a plain teamflow command (no prompt) and
// refuses tool calls that name the Teamflow home. The /teamf skill lists shell(teamflow) in allowed-tools too. Its
// userPromptSubmitted hook cannot add context, so notices arrive at session start, after each tool
// (postToolUse additionalContext), at the end of every teamflow command's output, and an approval request at the turn's end (agentStop).
package copilot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

const (
	// Name is the agent's name for `teamflow install -a`.
	Name = "copilot"
	mark = " hook " + Name + " "
	// hookTimeout is the seconds Copilot gives a hook; past it Copilot goes on without it (fail-open, even for
	// preToolUse).
	hookTimeout = 15
)

// Events are the hooks Teamflow adds, in order.
var Events = []string{"sessionStart", "userPromptSubmitted", "preToolUse", "postToolUse", "agentStop", "sessionEnd"}

// Adapter is the Copilot CLI adapter.
type Adapter struct{}

// New is the Copilot CLI adapter.
func New() *Adapter { return &Adapter{} }

func init() { adapters.Register(New()) }

// Name is "copilot".
func (*Adapter) Name() string { return Name }

// ConfigDir is Copilot's config folder: $COPILOT_HOME, else ~/.copilot.
func ConfigDir() string {
	if d := os.Getenv("COPILOT_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".copilot")
}

func hooksPath() string    { return filepath.Join(ConfigDir(), "hooks", "teamflow.json") }
func instrPath() string    { return filepath.Join(ConfigDir(), "copilot-instructions.md") }
func skillPath() string    { return filepath.Join(ConfigDir(), "skills", "teamf", "SKILL.md") }
func manifestPath() string { return filepath.Join(ConfigDir(), ".teamflow-install.json") }

// Detect says whether Copilot CLI is here: its config folder, or `copilot` on the PATH.
func (*Adapter) Detect() bool {
	if fi, err := os.Stat(ConfigDir()); err == nil && fi.IsDir() {
		return true
	}
	_, err := exec.LookPath("copilot")
	return err == nil
}

// Installed says whether Teamflow's hooks file is in Copilot's config.
func (*Adapter) Installed() bool {
	b, err := adapters.ReadFile(hooksPath())
	return err == nil && strings.Contains(string(b), mark)
}

// hooksFile is Teamflow's hooks file for Copilot.
func hooksFile(binary string) ojson.Obj {
	hooks := ojson.Obj{}
	for _, ev := range Events {
		hooks = hooks.Set(ev, []any{ojson.Obj{
			{Key: "type", Value: "command"},
			{Key: "bash", Value: adapters.HookCommand(binary, Name, ev)},
			{Key: "timeoutSec", Value: hookTimeout},
		}})
	}
	return ojson.Obj{{Key: "version", Value: 1}, {Key: "hooks", Value: hooks}}
}

// Install sets Teamflow up in Copilot CLI with binary (an absolute path). Every file is checked before any is
// written: a malformed or read-only one refuses the whole install.
func (*Adapter) Install(binary string) ([]string, error) {
	unlock, err := adapters.InstallLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := adapters.CheckWritable(hooksPath(), instrPath(), skillPath(), manifestPath()); err != nil {
		return nil, err
	}
	m, err := adapters.LoadManifest(manifestPath())
	if err != nil {
		return nil, err
	}
	if _, err := adapters.LoadJSON(hooksPath()); err != nil {
		return nil, err
	}
	if _, err := adapters.HasBlock(instrPath()); err != nil {
		return nil, err
	}
	if b, _ := adapters.ReadFile(skillPath()); b != nil && !strings.Contains(string(b), adapters.TeamfMark) {
		return nil, fmt.Errorf("%s is someone else's /teamf skill; move it, then run the install again", home.Display(skillPath()))
	}
	m.MarkDirs(ConfigDir(), ".", "hooks", "skills", filepath.Join("skills", "teamf"))
	m.MarkFileIfNew("copilot-instructions.md", instrPath())
	// The manifest first: a write failing half way still leaves uninstall knowing what install made.
	if err := m.Save(); err != nil {
		return nil, err
	}
	var done []string
	if err := adapters.SaveJSON(hooksPath(), hooksFile(binary), 0o644); err != nil {
		return done, err
	}
	done = append(done, fmt.Sprintf("hooks %s in %s (preToolUse allows teamflow commands and keeps tool calls out of the Teamflow home)",
		strings.Join(Events, ", "), home.Display(hooksPath())))
	if err := adapters.PlaceInstructions(instrPath(), "copilot-instructions.md", &m); err != nil {
		return done, err
	}
	done = append(done, "the Teamflow instructions in "+home.Display(instrPath()))
	if err := adapters.WriteFile(skillPath(), []byte(adapters.TeamfSkill(binary, "allowed-tools: shell(teamflow:*)")), 0o644); err != nil {
		return done, err
	}
	done = append(done, "/teamf skill at "+home.Display(skillPath()))
	return done, m.Save()
}

// Uninstall takes out Teamflow's hooks file, instruction block and /teamf skill; the person's own
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
	if _, err := adapters.LoadJSON(hooksPath()); err != nil {
		return nil, err
	}
	if _, err := adapters.HasBlock(instrPath()); err != nil {
		return nil, err
	}
	var done []string
	if ok, err := adapters.RemoveIfOurs(hooksPath(), mark, false); err != nil {
		return done, err
	} else if ok {
		done = append(done, "removed "+home.Display(hooksPath()))
	}
	if ok, err := adapters.RemoveInstructions(instrPath(), "copilot-instructions.md", m); err != nil {
		return done, err
	} else if ok {
		done = append(done, "removed the Teamflow instructions from "+home.Display(instrPath()))
	}
	if ok, err := adapters.RemoveIfOurs(skillPath(), adapters.TeamfMark, false); err != nil {
		return done, err
	} else if ok {
		done = append(done, "removed "+home.Display(skillPath()))
	}
	if err := m.Remove(); err != nil {
		return done, err
	}
	m.RemoveDirs(ConfigDir())
	return done, nil
}
