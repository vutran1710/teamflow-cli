package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

var (
	serverFlag        = flagSpec{name: "server", short: "s", help: "The Teamflow server (default: the one you signed in to, else " + config.DefaultServer + ")"}
	emailFlag         = flagSpec{name: "email", short: "e", help: "Your account's email"}
	passwordStdinFlag = flagSpec{name: "password-stdin", kind: flag, help: "Read the password from stdin (for scripts); never pass it as an argument"}
	jsonFlag          = flagSpec{name: "json", kind: flag, help: "Print JSON"}
)

// serverFor is the server to sign in to: the one given, else the one configured, else the default.
func serverFor(given string, cfg *config.Config) string {
	switch {
	case given != "":
		return given
	case cfg != nil && cfg.Server != "":
		return cfg.Server
	}
	return config.DefaultServer
}

// ------------------------------------------------------------------ login

var loginCmd = &command{
	name: "login", help: "Sign in with email and password; the password is asked for without echo and never kept.",
	flags: []flagSpec{serverFlag, emailFlag, passwordStdinFlag}, nargs: exactly(0), run: runLogin,
}

// password reads the password: from stdin with --password-stdin, else from the terminal without echo. Never an
// argument, and never read silently from a pipe.
func (r *inv) password(fromStdin bool) (string, error) {
	if fromStdin {
		line, err := r.in.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if !isTerminal() {
		return "", errors.New("no terminal to ask for the password on: type it in your own terminal, or pipe it with --password-stdin")
	}
	fmt.Fprint(r.errw, "Password: ")
	pw, err := readPassword()
	fmt.Fprintln(r.errw)
	return pw, err
}

// signIn signs this machine in to server as email, printing who it is signed in as.
func (r *inv) signIn(server, email string, fromStdin bool) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	pw, err := r.password(fromStdin)
	if err != nil {
		return r.failErr(err)
	}
	if pw == "" {
		return r.fail("not signed in: no password given")
	}
	out, err := c.Login(r.ctx, server, email, pw)
	var ae *api.APIError
	switch {
	case errors.Is(err, api.ErrOffline):
		return r.fail(fmt.Sprintf("could not reach %s: %v", server, err))
	case errors.Is(err, api.ErrUpgradeRequired):
		return r.failErr(err)
	case errors.As(err, &ae):
		return r.fail("not signed in: " + ae.Message)
	case err != nil:
		return r.fail("not signed in: " + err.Error())
	}
	until := out.ExpiresAt
	if len(until) >= 10 {
		until = until[:10]
	}
	r.printf("signed in as %s <%s> until %s\n", out.Account.Name, out.Account.Email, until)
	return 0
}

func runLogin(r *inv, a *parsed) int {
	if a.get("email") == "" {
		return usageFail(r, "login", "Missing option '--email'")
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	return r.signIn(serverFor(a.get("server"), c.Cfg), a.get("email"), a.has("password-stdin"))
}

func usageFail(r *inv, path, msg string) int {
	commandUsage(r.errw, path, findPath(path))
	fmt.Fprintf(r.errw, "\nError: %s\n", msg)
	return 2
}

func findPath(path string) *command {
	cs, c := commands, (*command)(nil)
	for _, n := range strings.Fields(path) {
		if c = find(cs, n); c == nil {
			return &command{name: path}
		}
		cs = c.sub
	}
	return c
}

// ------------------------------------------------------------------ logout, projects

var logoutCmd = &command{
	name: "logout", help: "Sign out: revoke the auth key and forget this machine's project keys.", nargs: exactly(0),
	run: func(r *inv, _ *parsed) int {
		c, err := r.core()
		if err != nil {
			return r.failErr(err)
		}
		if err := c.Logout(r.ctx); err != nil {
			return r.failErr(err)
		}
		r.println("signed out")
		return 0
	},
}

var projectsCmd = &command{
	name: "projects", help: "Every project you are in, across your orgs.", nargs: exactly(0),
	run: func(r *inv, _ *parsed) int {
		c, err := r.core()
		if err != nil {
			return r.failErr(err)
		}
		out, err := c.Projects(r.ctx)
		if err != nil {
			return r.failErr(err)
		}
		for _, v := range out.Projects {
			r.printf("%s\t%s\t%s\n", orgName(out.Orgs, v.Project.Org), v.Project.Name, v.Project.ID)
		}
		return 0
	},
}

func orgName(orgs map[string]string, id string) string {
	if n, ok := orgs[id]; ok {
		return n
	}
	return id
}

// ------------------------------------------------------------------ status

var statusCmd = &command{name: "status", help: "Server, sign-in, keys, spool, sessions and agents.", nargs: exactly(0), run: runStatus}

func runStatus(r *inv, _ *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	sid := ""
	if s, ok := r.session(); ok {
		sid = s.ID
	}
	for _, line := range c.Status(r.ctx, sid, home.Display) {
		r.println(line)
	}
	all := adapters.All()
	if len(all) == 0 {
		r.println("agents: no agent adapter is available in this build yet")
	}
	for _, a := range all {
		state := "not installed"
		if a.Installed() {
			state = "installed"
		}
		r.printf("%s: %s\n", a.Name(), state)
	}
	return 0
}

// ------------------------------------------------------------------ install, uninstall

var agentFlag = flagSpec{name: "agent", short: "a", kind: list,
	help: "An agent to set up: " + strings.Join(adapters.Supported, ", ") + ", or all (those found here, except " +
		strings.Join(adapters.Experimental, " and ") + ", which are experimental, not yet verified on a real install, " +
		"and set up only when named); repeatable"}

var installCmd = &command{
	name: "install", help: "Sign in, set Teamflow up in your agents, and start the background sync.",
	flags: []flagSpec{agentFlag, emailFlag, serverFlag, passwordStdinFlag,
		{name: "json", kind: flag, help: "Print one JSON object: installed, failed, on_path (the rest goes to stderr)"}},
	nargs: exactly(0), run: runInstall,
}

// chooseAgents is the agents named with -a (checked: supported, and in this build), or, with none named, the
// ones the person picks from those found on this machine. It never guesses.
func (r *inv) chooseAgents(named []string, verb string) ([]adapters.Adapter, int) {
	var out []adapters.Adapter
	seen := map[string]bool{}
	add := func(a adapters.Adapter) {
		if !seen[a.Name()] {
			seen[a.Name()] = true
			out = append(out, a)
		}
	}
	var leftOut []string // experimental agents found by -a all: set up only when named
	for _, n := range named {
		if n == "all" {
			for _, a := range adapters.All() {
				switch {
				case !a.Detect():
				case adapters.IsExperimental(a.Name()):
					leftOut = append(leftOut, a.Name())
				default:
					add(a)
				}
			}
			continue
		}
		if !adapters.IsSupported(n) {
			return nil, usageFail(r, verb, fmt.Sprintf("unknown agent '%s'; supported: %s", n, strings.Join(adapters.Supported, ", ")))
		}
		a, ok := adapters.Get(n)
		if !ok {
			return nil, r.fail(n + " is not available in this build yet")
		}
		if adapters.IsExperimental(n) && !seen[n] {
			fmt.Fprintf(r.errw, "%s: %s\n", n, adapters.ExperimentalNote)
		}
		add(a)
	}
	leftOut = slices.DeleteFunc(leftOut, func(n string) bool { return seen[n] })
	if len(leftOut) > 0 {
		fmt.Fprintf(r.errw, "not set up by -a all: %s (%s; name it with -a %s)\n", strings.Join(leftOut, ", "),
			adapters.ExperimentalNote, leftOut[0])
	}
	if len(named) > 0 {
		if len(out) == 0 {
			return nil, r.fail("no supported agent was found on this machine")
		}
		return out, 0
	}
	var found []adapters.Adapter
	var experimental []string
	for _, a := range adapters.All() {
		switch {
		case !a.Detect():
		case adapters.IsExperimental(a.Name()): // an experimental agent only when named
			experimental = append(experimental, a.Name())
		default:
			found = append(found, a)
		}
	}
	if len(found) == 0 && len(experimental) > 0 {
		return nil, r.fail("only experimental agents were found on this machine (" + strings.Join(experimental, ", ") +
			": " + adapters.ExperimentalNote + "); to set one up anyway, name it: -a " + experimental[0])
	}
	if len(found) == 0 {
		return nil, r.fail("no supported agent was found on this machine (supported: " + strings.Join(adapters.Supported, ", ") + ")")
	}
	if !isTerminal() {
		return nil, r.fail("say which agents to set up: -a <agent> (repeatable), or -a all")
	}
	fmt.Fprintln(r.errw, "Agents found on this machine:")
	for i, a := range found {
		fmt.Fprintf(r.errw, "  %d) %s\n", i+1, a.Name())
	}
	fmt.Fprint(r.errw, "Which ones should Teamflow be set up in? (numbers, separated by spaces; all: a): ")
	line, _ := r.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "a" || line == "all" {
		return found, 0
	}
	for _, f := range strings.FieldsFunc(line, func(c rune) bool { return c == ' ' || c == ',' }) {
		i, err := strconv.Atoi(f)
		if err != nil || i < 1 || i > len(found) {
			return nil, r.fail(fmt.Sprintf("not an agent number: %s", f))
		}
		add(found[i-1])
	}
	if len(out) == 0 {
		return nil, r.fail("no agent chosen: nothing set up")
	}
	return out, 0
}

// executable is this binary's path (os.Executable; a test sets its own).
var executable = os.Executable

// onPath says whether `teamflow`, run by name, is this binary (the same file, through links too). found is what
// the PATH gives ("" when nothing).
func onPath(bin string) (ok bool, found string) {
	found, err := exec.LookPath("teamflow")
	if err != nil {
		return false, ""
	}
	if abs, err := filepath.Abs(found); err == nil {
		found = abs
	}
	a, aerr := os.Stat(found)
	b, berr := os.Stat(bin)
	return aerr == nil && berr == nil && os.SameFile(a, b), found
}

// pathHint is how to put dir on the PATH, for zsh and bash ($HOME for the home folder, never the person's path).
func pathHint(dir string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			dir = "$HOME/" + rel
			if rel == "." {
				dir = "$HOME"
			}
		}
	}
	return "Add its folder to your PATH, then open a new terminal:\n" +
		"  zsh:  echo 'export PATH=\"" + dir + ":$PATH\"' >> ~/.zshrc\n" +
		"  bash: echo 'export PATH=\"" + dir + ":$PATH\"' >> ~/.bashrc\n" +
		"Your agents' hooks call teamflow by its full path and work as they are; you, the /teamf command and the " +
		"agents' own `teamflow ...` commands run it by name and need it on the PATH.\n"
}

// pathWarning is the loud warning when `teamflow` on the PATH is not bin ("" when it is).
func pathWarning(bin string) (string, bool) {
	ok, found := onPath(bin)
	if ok {
		return "", true
	}
	head := "WARNING: teamflow is not on your PATH (" + bin + ").\n"
	if found != "" {
		head = "WARNING: the teamflow on your PATH is " + found + ", not this one (" + bin + ").\n"
	}
	return "\n" + head + pathHint(filepath.Dir(bin)), false
}

// binary is how agents call this CLI: an absolute path, so hooks work from any folder.
func binary() string {
	exe, err := executable()
	if err != nil {
		exe = os.Args[0]
	}
	if abs, err := filepath.Abs(exe); err == nil {
		return abs
	}
	return exe
}

func runInstall(r *inv, a *parsed) int {
	if !a.has("json") {
		code, _ := install(r, a)
		return code
	}
	// --json: the human lines go to stderr; stdout is one JSON object (what was set up, and on_path).
	stdout := r.out
	r.out = r.errw
	code, answer := install(r, a)
	r.out = stdout
	if answer == nil {
		return code
	}
	r.printJSON(answer)
	return code
}

// install signs in and sets up the chosen agents. It returns the exit code and, once agents were set up (or
// refused), the answer install --json prints: installed, failed, on_path.
func install(r *inv, a *parsed) (int, ojson.Obj) {
	email := a.get("email")
	if email == "" {
		return usageFail(r, "install", "Missing option '-e' / '--email'"), nil
	}
	chosen, code := r.chooseAgents(a.lists["agent"], "install")
	if chosen == nil {
		return code, nil
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err), nil
	}
	if !c.SignedIn() || !strings.EqualFold(c.Cfg.Email, email) || (a.get("server") != "" && store.NormalizeServer(a.get("server")) != store.NormalizeServer(c.Cfg.Server)) {
		if code := r.signIn(serverFor(a.get("server"), c.Cfg), email, a.has("password-stdin")); code != 0 {
			return code, nil
		}
	} else {
		r.printf("signed in as %s\n", c.Cfg.Email)
	}
	bin := binary()
	// Each agent on its own: one refusing (a malformed or read-only file of its own, checked before it writes
	// anything) leaves it untouched and the others set up.
	var failed, done []string
	for _, ad := range chosen {
		lines, err := ad.Install(bin)
		r.printf("%s:\n", ad.Name())
		for _, l := range lines {
			r.printf("  %s\n", l)
		}
		if err != nil {
			fmt.Fprintf(r.errw, "%s: not set up: %v\n", ad.Name(), err)
			failed = append(failed, ad.Name())
			continue
		}
		done = append(done, ad.Name())
	}
	// The agents that were set up get the background sync even when another refused.
	if len(failed) < len(chosen) && adapters.StartSync != nil {
		if err := adapters.StartSync(); err != nil {
			fmt.Fprintf(r.errw, "the background sync did not start: %v\n", err)
		}
	}
	// Still installed when teamflow is not on the PATH (the hooks use its full path): said loudly, never a failure.
	warning, on := pathWarning(bin)
	fmt.Fprint(r.errw, warning)
	answer := ojson.Obj{{Key: "installed", Value: append([]string{}, done...)},
		{Key: "failed", Value: append([]string{}, failed...)}, {Key: "on_path", Value: on}}
	if len(failed) > 0 {
		return r.fail("not set up in " + strings.Join(failed, ", ") + "; fix what is said above, then run the install again"), answer
	}
	r.println("Done. In a session, ask your agent which Teamflow project it works on (or run /teamf project).")
	return 0, answer
}

var uninstallCmd = &command{
	name: "uninstall", help: "Remove what install added from your agents; your own settings stay as they were.",
	flags: []flagSpec{agentFlag, {name: "purge", kind: flag, help: "Also delete the Teamflow home (keys, replica, spool)"}},
	nargs: exactly(0), run: runUninstall,
}

func runUninstall(r *inv, a *parsed) int {
	var chosen []adapters.Adapter
	if named := a.lists["agent"]; len(named) > 0 {
		for _, n := range named {
			if n == "all" {
				chosen = append(chosen, adapters.All()...)
				continue
			}
			if !adapters.IsSupported(n) {
				return usageFail(r, "uninstall", fmt.Sprintf("unknown agent '%s'; supported: %s", n, strings.Join(adapters.Supported, ", ")))
			}
			ad, ok := adapters.Get(n)
			if !ok {
				return r.fail(n + " is not available in this build yet")
			}
			chosen = append(chosen, ad)
		}
	} else {
		for _, ad := range adapters.All() {
			if ad.Installed() {
				chosen = append(chosen, ad)
			}
		}
	}
	var failed []string
	for _, ad := range chosen {
		lines, err := ad.Uninstall()
		r.printf("%s:\n", ad.Name())
		for _, l := range lines {
			r.printf("  %s\n", l)
		}
		if err != nil {
			fmt.Fprintf(r.errw, "%s: not removed: %v\n", ad.Name(), err)
			failed = append(failed, ad.Name())
		}
	}
	if len(failed) > 0 {
		return r.fail("not removed from " + strings.Join(failed, ", ") + "; fix what is said above, then run the uninstall again")
	}
	if a.has("purge") {
		dir := home.Dir()
		if r.client != nil {
			r.client.Close()
			r.client = nil
		}
		if _, err := os.Stat(dir); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				return r.failErr(err)
			}
			r.printf("  deleted %s\n", home.Display(dir))
		}
	}
	r.println("Done.")
	return 0
}
