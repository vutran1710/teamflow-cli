package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
)

// The terminal for install's questions, replaced in tests.
var (
	// errTerminal says whether stderr (where the questions are drawn) is a terminal.
	errTerminal = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
	// rawMode puts the terminal in raw mode (keys arrive one by one) and gives back how to restore it; an error:
	// this terminal can't, and the agents are asked for by number.
	rawMode = func() (func(), error) {
		fd := int(os.Stdin.Fd())
		old, err := term.MakeRaw(fd)
		if err != nil {
			return nil, err
		}
		return func() { _ = term.Restore(fd, old) }, nil
	}
	// catchInterrupt makes Ctrl-C while install asks (the email, the password) leave the terminal as it was
	// and say nothing was set up; the returned func stops catching.
	catchInterrupt = func(w io.Writer) func() {
		fd := int(os.Stdin.Fd())
		old, err := term.GetState(fd)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		done := make(chan struct{})
		go func() {
			select {
			case <-ch:
				if err == nil {
					_ = term.Restore(fd, old)
				}
				fmt.Fprint(w, "\n"+abortedMsg+"\n")
				os.Exit(130)
			case <-done:
			}
		}()
		return func() { signal.Stop(ch); close(done) }
	}
)

// errAborted is the person stopping install's questions (Ctrl-C, or the end of input): nothing was set up.
var errAborted = errors.New("aborted")

const abortedMsg = "Aborted: nothing was set up."

// emailPattern is a plausible email: something, an @, a domain with a dot; no spaces.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

// askEmail asks for the account's email until a plausible one is given; def (the account signed in here, if any)
// is taken on an empty answer.
func (r *inv) askEmail(def string) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(r.errw, "Email [%s]: ", def)
		} else {
			fmt.Fprint(r.errw, "Email: ")
		}
		line, err := r.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(r.errw)
			return "", errAborted
		}
		email := strings.TrimSpace(line)
		if email == "" {
			email = def
		}
		if emailPattern.MatchString(email) {
			return email, nil
		}
		if email == "" {
			fmt.Fprintln(r.errw, "Your email, please (the one you sign in to Teamflow with).")
			continue
		}
		fmt.Fprintf(r.errw, "%q doesn't look like an email; try again.\n", email)
	}
}

// agentOption is one agent in the list install offers.
type agentOption struct {
	ad     adapters.Adapter
	found  bool
	chosen bool
}

func (o agentOption) label() string {
	switch {
	case adapters.IsExperimental(o.ad.Name()):
		return "experimental"
	case o.found:
		return "found on this machine"
	}
	return ""
}

// agentOptions are this build's agents, the verified ones first; those found here (never an experimental one)
// come chosen.
func agentOptions() []agentOption {
	var out, later []agentOption
	for _, a := range adapters.All() {
		exp := adapters.IsExperimental(a.Name())
		o := agentOption{ad: a, found: a.Detect()}
		o.chosen = o.found && !exp
		if exp {
			later = append(later, o)
			continue
		}
		out = append(out, o)
	}
	return append(out, later...)
}

// pickAgents asks which agents to set up: a list moved with the arrow keys, space choosing and enter confirming;
// by number when the terminal can't send keys one by one. At least one must be chosen.
func (r *inv) pickAgents() ([]adapters.Adapter, error) {
	opts := agentOptions()
	if len(opts) == 0 {
		return nil, errors.New("no agent can be set up by this build")
	}
	var err error
	if restore, rerr := rawMode(); rerr == nil {
		err = r.pickByKeys(opts)
		restore()
	} else {
		err = r.pickByNumber(opts)
	}
	if err != nil {
		return nil, err
	}
	var out []adapters.Adapter
	for _, o := range opts {
		if o.chosen {
			out = append(out, o.ad)
			if adapters.IsExperimental(o.ad.Name()) {
				fmt.Fprintf(r.errw, "%s: %s\n", o.ad.Name(), adapters.ExperimentalNote)
			}
		}
	}
	return out, nil
}

const pickQuestion = "Set up Teamflow in which agents?"

func anyChosen(opts []agentOption) bool {
	return slices.ContainsFunc(opts, func(o agentOption) bool { return o.chosen })
}

func chosenTitles(opts []agentOption) string {
	var t []string
	for _, o := range opts {
		if o.chosen {
			t = append(t, adapters.Title(o.ad.Name()))
		}
	}
	return strings.Join(t, ", ")
}

// pickByKeys draws the list on stderr in raw mode (lines end in \r\n) and reads keys from stdin.
func (r *inv) pickByKeys(opts []agentOption) error {
	w, at, note := r.errw, 0, ""
	draw := func(first bool) {
		if !first {
			fmt.Fprintf(w, "\x1b[%dA", len(opts)+1) // back to the first agent's line
		}
		for i, o := range opts {
			cur, box := "  ", "[ ]"
			if i == at {
				cur = "\x1b[36m❯\x1b[0m "
			}
			if o.chosen {
				box = "[\x1b[32mx\x1b[0m]"
			}
			line := fmt.Sprintf("%s%s %-16s", cur, box, adapters.Title(o.ad.Name()))
			if l := o.label(); l != "" {
				line += "\x1b[2m" + l + "\x1b[0m"
			}
			fmt.Fprintf(w, "\r\x1b[2K%s\r\n", line)
		}
		fmt.Fprintf(w, "\r\x1b[2K\x1b[33m%s\x1b[0m\r\n", note)
	}
	fmt.Fprintf(w, "%s \x1b[2m(↑/↓ move, space choose, enter confirm)\x1b[0m\r\n\x1b[?25l", pickQuestion)
	draw(true)
	finish := func(answer string) {
		fmt.Fprintf(w, "\x1b[%dA\r\x1b[J%s %s\r\n\x1b[?25h", len(opts)+2, pickQuestion, answer)
	}
	for {
		b, err := r.in.ReadByte()
		if err != nil {
			finish("")
			return errAborted
		}
		switch b {
		case 3, 4: // Ctrl-C, Ctrl-D
			finish("")
			return errAborted
		case '\r', '\n':
			if !anyChosen(opts) {
				note = "Choose at least one agent: move to it and press space."
				draw(false)
				continue
			}
			finish(chosenTitles(opts))
			return nil
		case ' ', 'x':
			opts[at].chosen = !opts[at].chosen
		case 'k':
			at = (at + len(opts) - 1) % len(opts)
		case 'j':
			at = (at + 1) % len(opts)
		case 0x1b: // ESC [ A / ESC O A: up; B: down
			if p, err := r.in.ReadByte(); err != nil || (p != '[' && p != 'O') {
				continue
			}
			k, err := r.in.ReadByte()
			if err != nil {
				continue
			}
			switch k {
			case 'A':
				at = (at + len(opts) - 1) % len(opts)
			case 'B':
				at = (at + 1) % len(opts)
			}
		default:
			continue
		}
		note = ""
		draw(false)
	}
}

// pickByNumber lists the agents numbered and reads a line of numbers ("1,2"); empty takes those already chosen.
func (r *inv) pickByNumber(opts []agentOption) error {
	fmt.Fprintln(r.errw, pickQuestion)
	var def []string
	for i, o := range opts {
		line := fmt.Sprintf("  %d) %s", i+1, adapters.Title(o.ad.Name()))
		if l := o.label(); l != "" {
			line += " (" + l + ")"
		}
		fmt.Fprintln(r.errw, line)
		if o.chosen {
			def = append(def, strconv.Itoa(i+1))
		}
	}
	for {
		if len(def) > 0 {
			fmt.Fprintf(r.errw, "Numbers, separated by commas [%s]: ", strings.Join(def, ","))
		} else {
			fmt.Fprint(r.errw, "Numbers, separated by commas: ")
		}
		line, err := r.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(r.errw)
			return errAborted
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if len(def) == 0 {
				fmt.Fprintln(r.errw, "Choose at least one agent.")
				continue
			}
			return nil
		}
		picked, bad := map[int]bool{}, ""
		for _, f := range strings.FieldsFunc(line, func(c rune) bool { return c == ' ' || c == ',' }) {
			i, err := strconv.Atoi(f)
			if err != nil || i < 1 || i > len(opts) {
				bad = f
				break
			}
			picked[i-1] = true
		}
		if bad != "" {
			fmt.Fprintf(r.errw, "Not an agent number: %s; choose from 1 to %d.\n", bad, len(opts))
			continue
		}
		for i := range opts {
			opts[i].chosen = picked[i]
		}
		return nil
	}
}
