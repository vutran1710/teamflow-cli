package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/procs"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// The terminal, replaced in tests.
var (
	// isTerminal says whether stdin is a terminal (a person at a keyboard).
	isTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	// readPassword reads a line from the terminal without echoing it.
	readPassword = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
	// sessionLog is where a store that can't say which session this runs under is reported (once).
	sessionLog     io.Writer = os.Stderr
	sessionLogOnce sync.Once
)

// pidReuseSlack is how much later than its session was last seen an agent process may seem to have started (clock
// granularity) before its pid is taken as reused by another process.
const pidReuseSlack = 2 * time.Second

// findSession is the agent session this process runs under: the session recorded against the nearest of its
// ancestors (an agent's adapter records its sessions against the agent's process). A session whose pid now belongs
// to a process that started after the session was last seen is a dead agent's, its pid reused: never this one.
func findSession(st *store.Store) (store.Session, bool) {
	s, ok, err := st.SessionOfPIDs(procs.Ancestors(os.Getpid(), 12), func(s store.Session) bool {
		started, ok := procs.StartTime(s.AgentPID)
		return ok && !started.After(s.UpdatedAt.Add(pidReuseSlack))
	})
	if err != nil {
		sessionLogOnce.Do(func() {
			fmt.Fprintf(sessionLog, "teamflow: could not look up the agent session: %v\n", err)
		})
		return store.Session{}, false
	}
	return s, ok
}

// agentMarkers are environment variables an agent sets for the commands it runs; any of them set means this runs
// under an agent. TODO(Task 9): confirm the Cursor CLI, Copilot CLI and Antigravity CLI markers against a real
// install of each (CLAUDECODE is Claude Code's; AI_AGENT is the cross-agent convention).
var agentMarkers = []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "AI_AGENT", "CURSOR_AGENT", "CURSOR_TRACE_ID",
	"COPILOT_CLI", "COPILOT_AGENT", "ANTIGRAVITY_AGENT", "GEMINI_CLI"}

// underAgent says whether an agent marker is set.
func underAgent() bool {
	for _, m := range agentMarkers {
		if os.Getenv(m) != "" {
			return true
		}
	}
	return os.Getenv("TEAMFLOW_ACTOR") == "agent"
}

// kind of a flag's value.
type kind int

const (
	str  kind = iota // --name value, --name=value
	flag             // --name
	list             // --name value, repeatable
)

type flagSpec struct {
	name, short string
	kind        kind
	help        string
}

// command is one command or command group.
type command struct {
	name, args, help string
	hidden           bool
	flags            []flagSpec
	sub              []*command                   // a group's commands
	raw              bool                         // takes its arguments as they are (team)
	run              func(r *inv, a *parsed) int  // nil for a group
	nargs            func(pos []string) (ok bool) // nil: any
}

func (c *command) flagNames() []string {
	var out []string
	for _, f := range c.flags {
		out = append(out, f.name)
	}
	for _, s := range c.sub {
		out = append(out, s.flagNames()...)
	}
	return out
}

func exactly(n int) func([]string) bool { return func(p []string) bool { return len(p) == n } }
func atLeast(n int) func([]string) bool { return func(p []string) bool { return len(p) >= n } }
func between(lo, hi int) func([]string) bool {
	return func(p []string) bool { return len(p) >= lo && len(p) <= hi }
}

type parsed struct {
	pos   []string
	str   map[string]string
	set   map[string]bool
	lists map[string][]string
}

func (p *parsed) get(name string) string { return p.str[name] }
func (p *parsed) has(name string) bool   { return p.set[name] }

var errHelp = errors.New("help")

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// parse reads flags anywhere among the arguments ("--" ends them).
func parse(c *command, args []string) (*parsed, error) {
	p := &parsed{str: map[string]string{}, set: map[string]bool{}, lists: map[string][]string{}}
	byName := map[string]flagSpec{}
	for _, f := range c.flags {
		byName["--"+f.name] = f
		if f.short != "" {
			byName["-"+f.short] = f
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.pos = append(p.pos, args[i+1:]...)
			break
		}
		if a == "-h" || a == "--help" {
			return nil, errHelp
		}
		if len(a) < 2 || a[0] != '-' {
			p.pos = append(p.pos, a)
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		f, ok := byName[name]
		if !ok {
			return nil, usageError{"No such option: " + name}
		}
		switch f.kind {
		case flag:
			if inline {
				return nil, usageError{"--" + f.name + " takes no value"}
			}
			p.set[f.name] = true
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, usageError{"--" + f.name + " needs a value"}
			}
			i++
			value = args[i]
		}
		p.set[f.name] = true
		if f.kind == list {
			p.lists[f.name] = append(p.lists[f.name], value)
		} else {
			p.str[f.name] = value
		}
	}
	return p, nil
}

// inv is one invocation: its streams, and the client once opened.
type inv struct {
	ctx    context.Context
	in     *bufio.Reader
	out    io.Writer
	errw   io.Writer
	relay  bool // behind the /teamf slash command: a refusal is printed on stdout and exits 0
	client *core.Core
}

// fail refuses with msg: on stderr with exit 1, or, behind the relay, on stdout with exit 0 (there a failing
// command shows the person only "Shell command failed", and the agent gets nothing to tell them).
func (r *inv) fail(msg string) int {
	if r.relay {
		fmt.Fprintf(r.out, "Teamflow refused: %s\n", strings.TrimPrefix(msg, "refused: "))
		return 0
	}
	fmt.Fprintln(r.errw, msg)
	return 1
}

func (r *inv) failErr(err error) int { return r.fail(errText(err)) }

// errText is how an error reads to the person.
func errText(err error) string {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return err.Error()
}

func (r *inv) println(a ...any)               { fmt.Fprintln(r.out, a...) }
func (r *inv) printf(format string, a ...any) { fmt.Fprintf(r.out, format, a...) }

// core opens the client (the Teamflow home, its config and store) once.
func (r *inv) core() (*core.Core, error) {
	if r.client != nil {
		return r.client, nil
	}
	c, err := core.Open()
	if err != nil {
		return nil, err
	}
	r.client = c
	return c, nil
}

// session is the agent session this runs under, if any.
func (r *inv) session() (store.Session, bool) {
	c, err := r.core()
	if err != nil {
		return store.Session{}, false
	}
	return findSession(c.St)
}

const noSession = "no agent session found above this process; run this from inside an agent session"

// confirm asks a yes/no question on the terminal; without one it refuses (pass --yes).
func (r *inv) confirm(question string) (bool, string) {
	if !isTerminal() {
		return false, "not confirmed: there is no terminal to ask on; pass --yes"
	}
	fmt.Fprintf(r.errw, "%s [y/N]: ", question)
	line, _ := r.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, ""
	}
	return false, "Aborted."
}

// ------------------------------------------------------------------ the command table

var commands []*command

func init() {
	commands = []*command{
		loginCmd, logoutCmd, projectsCmd, installCmd, uninstallCmd, statusCmd, stopCmd, approveCmd, rejectCmd,
		teamCmd, topicCmd, artifactCmd, deliverablesCmd, orgCmd, logCmd,
		{name: "hook", hidden: true, raw: true, run: runHook},
		{name: "mcp", hidden: true, raw: true, run: later("the MCP server")},
		{name: "sync", help: "The background sync (one per machine).", raw: true, run: later("the background sync")},
		{name: "self-update", help: "Update teamflow to the newest release.", raw: true, run: later("self-update")},
		{name: "debug", hidden: true, raw: true},
	}
}

// later is a command that arrives in a later build.
func later(what string) func(*inv, *parsed) int {
	return func(r *inv, _ *parsed) int { return r.fail(what + " is not available in this build yet") }
}

func find(cs []*command, name string) *command {
	for _, c := range cs {
		if c.name == name {
			return c
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: teamflow <command> [options]")
	fmt.Fprintln(w, "\nTeamflow: share agent context with your team.\n\nCommands:")
	var lines []string
	for _, c := range commands {
		if c.hidden {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-14s %s", c.name, c.help))
	}
	fmt.Fprintln(w, strings.Join(lines, "\n"))
	fmt.Fprintln(w, "\nRun teamflow <command> --help for its options.")
}

func commandUsage(w io.Writer, path string, c *command) {
	line := "usage: teamflow " + path
	if len(c.sub) > 0 {
		line += " <command>"
	}
	if c.args != "" {
		line += " " + c.args
	}
	if len(c.flags) > 0 {
		line += " [options]"
	}
	fmt.Fprintln(w, line)
	if c.help != "" {
		fmt.Fprintln(w, "\n"+c.help)
	}
	if len(c.sub) > 0 {
		fmt.Fprintln(w, "\nCommands:")
		for _, s := range c.sub {
			fmt.Fprintf(w, "  %-14s %s\n", s.name, s.help)
		}
	}
	if len(c.flags) > 0 {
		fmt.Fprintln(w, "\nOptions:")
		fs := append([]flagSpec(nil), c.flags...)
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].name < fs[j].name })
		for _, f := range fs {
			name := "--" + f.name
			if f.short != "" {
				name = "-" + f.short + ", " + name
			}
			if f.kind != flag {
				name += " TEXT"
			}
			fmt.Fprintf(w, "  %-24s %s\n", name, f.help)
		}
	}
}

// Main runs the teamflow command line: 0 on success, 1 on a refusal, 2 on a usage error.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		usage(stdout)
		return 0
	}
	c := find(commands, args[0])
	if c == nil {
		fmt.Fprintf(stderr, "usage: teamflow <command> [options]\nTry 'teamflow --help' for help.\n\nError: No such command '%s'.\n", args[0])
		return 2
	}
	if c.name == "debug" {
		return Debug(args[1:], stdout, stderr)
	}
	r := &inv{ctx: context.Background(), in: bufio.NewReader(stdin), out: stdout, errw: stderr}
	defer func() {
		if r.client != nil {
			r.client.Close()
		}
	}()
	path, rest := c.name, args[1:]
	for len(c.sub) > 0 {
		if len(rest) > 0 && (rest[0] == "--help" || rest[0] == "-h") {
			commandUsage(stdout, path, c)
			return 0
		}
		var sub *command
		if len(rest) > 0 {
			sub = find(c.sub, rest[0])
		}
		if sub == nil && c.run == nil {
			if len(rest) == 0 {
				commandUsage(stderr, path, c)
				return 2
			}
			fmt.Fprintf(stderr, "usage: teamflow %s <command>\n\nError: No such command '%s'.\n", path, rest[0])
			return 2
		}
		if sub == nil {
			break
		}
		c, path, rest = sub, path+" "+sub.name, rest[1:]
	}
	if c.raw {
		if len(rest) > 0 && (rest[0] == "--help" || rest[0] == "-h") {
			commandUsage(stdout, path, c)
			return 0
		}
		return c.run(r, &parsed{pos: rest})
	}
	p, err := parse(c, rest)
	if errors.Is(err, errHelp) {
		commandUsage(stdout, path, c)
		return 0
	}
	if err == nil && c.nargs != nil && !c.nargs(p.pos) {
		err = usageError{"wrong number of arguments"}
	}
	if err == nil {
		for _, f := range c.flags {
			if strings.HasSuffix(f.help, "(required)") && !p.set[f.name] {
				err = usageError{"Missing option '--" + f.name + "'"}
			}
		}
	}
	if err != nil {
		commandUsage(stderr, path, c)
		fmt.Fprintf(stderr, "\nError: %s\n", err)
		return 2
	}
	return c.run(r, p)
}
