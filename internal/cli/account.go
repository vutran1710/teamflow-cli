package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/home"
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
	help: "An agent to set up: " + strings.Join(adapters.Supported, ", ") + ", or all (those found here); repeatable"}

var installCmd = &command{
	name: "install", help: "Sign in, set Teamflow up in your agents, and start the background sync.",
	flags: []flagSpec{agentFlag, emailFlag, serverFlag, passwordStdinFlag}, nargs: exactly(0), run: runInstall,
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
	for _, n := range named {
		if n == "all" {
			for _, a := range adapters.All() {
				if a.Detect() {
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
		add(a)
	}
	if len(named) > 0 {
		if len(out) == 0 {
			return nil, r.fail("no supported agent was found on this machine")
		}
		return out, 0
	}
	var found []adapters.Adapter
	for _, a := range adapters.All() {
		if a.Detect() {
			found = append(found, a)
		}
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

// binary is how agents call this CLI: an absolute path, so hooks work from any folder.
func binary() string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	if abs, err := filepath.Abs(exe); err == nil {
		return abs
	}
	return exe
}

func runInstall(r *inv, a *parsed) int {
	email := a.get("email")
	if email == "" {
		return usageFail(r, "install", "Missing option '-e' / '--email'")
	}
	chosen, code := r.chooseAgents(a.lists["agent"], "install")
	if chosen == nil {
		return code
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	if !c.SignedIn() || !strings.EqualFold(c.Cfg.Email, email) || (a.get("server") != "" && store.NormalizeServer(a.get("server")) != store.NormalizeServer(c.Cfg.Server)) {
		if code := r.signIn(serverFor(a.get("server"), c.Cfg), email, a.has("password-stdin")); code != 0 {
			return code
		}
	} else {
		r.printf("signed in as %s\n", c.Cfg.Email)
	}
	bin := binary()
	// Each agent on its own: one refusing (a malformed or read-only file of its own, checked before it writes
	// anything) leaves it untouched and the others set up.
	var failed []string
	for _, ad := range chosen {
		lines, err := ad.Install(bin)
		r.printf("%s:\n", ad.Name())
		for _, l := range lines {
			r.printf("  %s\n", l)
		}
		if err != nil {
			fmt.Fprintf(r.errw, "%s: not set up: %v\n", ad.Name(), err)
			failed = append(failed, ad.Name())
		}
	}
	// The agents that were set up get the background sync even when another refused.
	if len(failed) < len(chosen) && adapters.StartSync != nil {
		if err := adapters.StartSync(); err != nil {
			fmt.Fprintf(r.errw, "the background sync did not start: %v\n", err)
		}
	}
	if len(failed) > 0 {
		return r.fail("not set up in " + strings.Join(failed, ", ") + "; fix what is said above, then run the install again")
	}
	r.println("Done. In a session, ask your agent which Teamflow project it works on (or run /teamf project).")
	return 0
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
