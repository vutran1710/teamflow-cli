//go:build darwin || linux

package selfupdate

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// setupScript stands in for teamflow once it is verified and in place: it says how it was run (its arguments,
// whether stdin and stdout are the terminal) and leaves a mark; SETUP_EXIT is its exit code.
const setupScript = "#!/bin/sh\necho \"setup ran: $*\"\n" +
	"if [ -t 0 ]; then echo 'stdin is the terminal'; fi\nif [ -t 1 ]; then echo 'stdout is the terminal'; fi\n" +
	": > \"$SETUP\"\nexit \"${SETUP_EXIT:-0}\"\n"

// runOnTTY runs install.sh as `curl … | sh` runs it at a terminal: stdin a pipe (the script), stdout and stderr
// the terminal, which is the controlling one (/dev/tty). keys are typed once the question shows.
func (s *shRun) runOnTTY(setup, keys string, extra ...string) {
	s.t.Helper()
	ptm, tty, err := pty.Open()
	if err != nil {
		s.t.Skipf("no pseudo-terminal here: %v", err)
	}
	defer ptm.Close()
	cmd := exec.Command(filepath.Join(s.tools, "sh"), s.script)
	cmd.Env = append([]string{"HOME=" + s.home, "PATH=" + s.tools, "MARK=" + s.mark, "SETUP=" + setup,
		"TEAMFLOW_RELEASES_URL=" + s.release.srv.URL}, extra...)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	tty.Close()
	var mu sync.Mutex
	var screen bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			mu.Lock()
			screen.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	if keys != "" {
		deadline := time.Now().Add(20 * time.Second)
		for {
			mu.Lock()
			asked := strings.Contains(screen.String(), "Set up teamflow now? [Y/n]")
			mu.Unlock()
			if asked {
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				s.t.Fatalf("never asked:\n%s", screen.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = io.WriteString(ptm, keys)
	}
	err = cmd.Wait()
	s.code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		s.code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	select {
	case <-read:
	case <-time.After(2 * time.Second): // macOS: the terminal's last bytes are read; the reader may block on
	}
	mu.Lock()
	s.out = strings.ReplaceAll(screen.String(), "\r\n", "\n")
	mu.Unlock()
	if _, err := os.Stat(s.mark); err == nil {
		s.t.Fatalf("install.sh ran the downloaded binary\n%s", s.out)
	}
}

// At a terminal: install.sh asks, and yes (or just enter) runs `teamflow install` on the terminal (stdin too,
// though the script came through a pipe), after the binary is verified and in place; no next-step lines then.
func TestInstallShOffersSetupAtATerminal(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShReleaseOf(t, sk, runtime.GOOS, runtime.GOARCH, setupScript)
	for _, answer := range []string{"y\r", "\r", "YES\r"} {
		s := newShRun(t, pub, rel)
		setup := filepath.Join(t.TempDir(), "setup")
		s.runOnTTY(setup, answer)
		s.wantOK()
		for _, want := range []string{"Installed teamflow v0.2.0", "Set up teamflow now? [Y/n]", "setup ran: install\n",
			"stdin is the terminal", "stdout is the terminal"} {
			if !strings.Contains(s.out, want) {
				t.Fatalf("answer %q: output lacks %q\n%s", answer, want, s.out)
			}
		}
		if strings.Contains(s.out, "Next: teamflow install") {
			t.Fatalf("the next step said after the setup ran\n%s", s.out)
		}
		if strings.Index(s.out, "Installed teamflow") > strings.Index(s.out, "setup ran") {
			t.Fatal("setup ran before the binary was in place")
		}
	}
}

// Sad paths at a terminal: no (nothing runs; the next step is said), TEAMFLOW_NO_SETUP=1 and CI (not even
// asked), the setup failing or stopped (said kindly, the next step, exit 0: the binary is installed), and a bad
// signature (refused: the setup never runs).
func TestInstallShSetupDeclinedOrOff(t *testing.T) {
	sk, pub := throwawayKey(t)
	rel := newShReleaseOf(t, sk, runtime.GOOS, runtime.GOARCH, setupScript)
	cases := []struct {
		keys  string
		env   []string
		asked bool
		ran   bool
		say   string
	}{
		{"n\r", nil, true, false, ""},
		{"no\r", nil, true, false, ""},
		{"", []string{"TEAMFLOW_NO_SETUP=1"}, false, false, ""},
		{"", []string{"CI=true"}, false, false, ""},
		{"y\r", []string{"SETUP_EXIT=1"}, true, true, "Setup didn't finish; nothing is lost."},
	}
	for _, c := range cases {
		s := newShRun(t, pub, rel)
		setup := filepath.Join(t.TempDir(), "setup")
		s.runOnTTY(setup, c.keys, c.env...)
		s.wantOK()
		_, err := os.Stat(setup)
		if asked := strings.Contains(s.out, "Set up teamflow now?"); asked != c.asked || (err == nil) != c.ran {
			t.Fatalf("%q %v: asked %v, ran %v\n%s", c.keys, c.env, asked, err == nil, s.out)
		}
		if !strings.Contains(s.out, "Next: teamflow install") || !strings.Contains(s.out, c.say) {
			t.Fatalf("%q %v: no next step\n%s", c.keys, c.env, s.out)
		}
	}

	s := newShRun(t, pub, rel)
	s.fake("minisign", "#!/bin/sh\nexit 1\n")
	setup := filepath.Join(t.TempDir(), "setup")
	s.runOnTTY(setup, "")
	s.wantRefused("signature check failed")
	if _, err := os.Stat(setup); err == nil || strings.Contains(s.out, "Set up teamflow now?") {
		t.Fatalf("set up after a failed check\n%s", s.out)
	}
}
