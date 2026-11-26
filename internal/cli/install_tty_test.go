//go:build darwin || linux

package cli

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// ttyRun is install run on a real pseudo-terminal: stdin and stderr are its tty (raw mode, no-echo password and
// all), stdout a buffer; the test types on the other end.
type ttyRun struct {
	t      *testing.T
	ptm    *os.File
	mu     sync.Mutex
	screen bytes.Buffer
	done   chan struct{}
	code   int
	out    bytes.Buffer
	before *term.State
	tty    *os.File
}

func startTTY(t *testing.T, m *machine, args ...string) *ttyRun {
	t.Helper()
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { ptm.Close(); tty.Close() })
	before, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldErr := os.Stdin, os.Stderr
	os.Stdin, os.Stderr = tty, tty
	t.Cleanup(func() { os.Stdin, os.Stderr = oldIn, oldErr })
	r := &ttyRun{t: t, ptm: ptm, done: make(chan struct{}), before: before, tty: tty}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			r.mu.Lock()
			r.screen.Write(buf[:n])
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(r.done)
		r.code = Main(args, tty, &r.out, tty)
	}()
	return r
}

// await waits until the terminal shows text (after what was shown before), then types keys.
func (r *ttyRun) await(text, keys string) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		got := r.screen.String()
		r.mu.Unlock()
		if strings.Contains(got, text) {
			break
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the terminal never showed %q:\n%q", text, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if keys != "" {
		if _, err := io.WriteString(r.ptm, keys); err != nil {
			r.t.Fatal(err)
		}
	}
}

// wait waits for install to end; the terminal must be as it was before (echo and line mode back).
func (r *ttyRun) wait() (int, string) {
	r.t.Helper()
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		r.t.Fatal("install never ended")
	}
	after, err := term.GetState(int(r.tty.Fd()))
	if err != nil {
		r.t.Fatal(err)
	}
	if !reflect.DeepEqual(r.before, after) {
		r.t.Fatal("the terminal was not put back as it was")
	}
	// What install wrote last may still be in the pty, not yet read off its other end: reading the screen as soon as
	// install ends could miss its last line ("Aborted: …"). A marker written now comes out after all of it (a pty
	// keeps order), so once the marker shows, the screen is complete.
	if _, err := io.WriteString(r.tty, ttyEnd); err != nil {
		r.t.Fatal(err)
	}
	r.await(ttyEnd, "")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code, strings.Replace(r.screen.String(), ttyEnd, "", 1)
}

// ttyEnd marks the end of what install wrote on the terminal.
const ttyEnd = "<end of the test's install run>"

// realTerminal undoes newMachine's stand-ins: the questions see the real (pseudo-)terminal.
func realTerminal(t *testing.T) {
	isT, errT, pw, raw := isTerminal, errTerminal, readPassword, rawMode
	t.Cleanup(func() { isTerminal, errTerminal, readPassword, rawMode = isT, errT, pw, raw })
	isTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	errTerminal = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
	readPassword = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
	rawMode = func() (func(), error) {
		fd := int(os.Stdin.Fd())
		old, err := term.MakeRaw(fd)
		if err != nil {
			return nil, err
		}
		return func() { _ = term.Restore(fd, old) }, nil
	}
}

// On a real terminal: the email typed, the arrow-key list (down, space: Copilot CLI too), the password hidden;
// both agents set up and the terminal put back. Sad path: Ctrl-C in the list sets nothing up and puts the
// terminal back too.
func TestInstallOnARealTerminal(t *testing.T) {
	cc, co, cu, _ := promptAgents(t)
	m := newMachine(t, false)
	m.srv.on("auth.login", "auth.login")
	realTerminal(t)
	r := startTTY(t, m, "install", "-s", m.srv.URL())
	r.await("Email: ", "lan@acme.test\r")
	r.await("enter confirm", "\x1b[B \r")
	r.await("Password: ", "secret-pw\r")
	code, screen := r.wait()
	if code != 0 {
		t.Fatalf("exit %d\n%q\n%s", code, screen, r.out.String())
	}
	if !cc.installed || !co.installed || cu.installed {
		t.Fatalf("installed %v %v %v", cc.installed, co.installed, cu.installed)
	}
	if strings.Contains(screen, "secret-pw") {
		t.Fatal("the password was echoed")
	}
	contains(t, "terminal", screen, pickQuestion+" Claude Code, Copilot CLI")
	contains(t, "stdout", r.out.String(), "  ✓ Claude Code is ready\n  ✓ Copilot CLI is ready\n")

	cc.installed, co.installed = false, false
	m2 := newMachine(t, false)
	realTerminal(t)
	r = startTTY(t, m2, "install", "-s", m2.srv.URL())
	r.await("Email: ", "lan@acme.test\r")
	r.await("enter confirm", "\x1b[B\x03")
	code, screen = r.wait()
	if code != 1 || !strings.Contains(screen, "Aborted: nothing was set up.") {
		t.Fatalf("exit %d\n%q", code, screen)
	}
	nothingDone(t, m2, cc, co, cu)
}

// Keys typed ahead, before the password is asked (they were shown as typed), are thrown away: the password is what
// is typed at its prompt, with nothing shown.
func TestInstallPasswordIgnoresTypeAhead(t *testing.T) {
	promptAgents(t)
	m := newMachine(t, false)
	m.srv.on("auth.login", "auth.login")
	realTerminal(t)
	ahead := make(chan struct{})
	old := beforePassword
	beforePassword = func() { <-ahead } // the moment between the agents list and the password prompt
	t.Cleanup(func() { beforePassword = old })
	r := startTTY(t, m, "install", "-s", m.srv.URL())
	r.await("Email: ", "lan@acme.test\r")
	r.await("enter confirm", "\r")
	r.await(pickQuestion+" Claude Code", "typed-too-soon\r")
	time.Sleep(100 * time.Millisecond) // the keys are in the terminal's queue
	close(ahead)
	r.await("Password: ", "secret-pw\r")
	code, screen := r.wait()
	if code != 0 {
		t.Fatalf("exit %d\n%q\n%s", code, screen, r.out.String())
	}
	if p := params(t, onlyCall(t, m.srv, "auth.login")); p["password"] != "secret-pw" {
		t.Fatalf("signed in with %q", p["password"])
	}
	if strings.Contains(screen, "secret-pw") {
		t.Fatal("the password was echoed")
	}
}
