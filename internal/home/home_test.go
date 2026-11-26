package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirHonoursTeamflowHome(t *testing.T) {
	d := t.TempDir()
	t.Setenv("TEAMFLOW_HOME", d)
	if got := Dir(); got != d {
		t.Fatalf("Dir() = %q, want %q", got, d)
	}
}

func TestDirDefaultsToDotTeamflow(t *testing.T) {
	h := t.TempDir()
	t.Setenv("TEAMFLOW_HOME", "")
	t.Setenv("HOME", h)
	if got, want := Dir(), filepath.Join(h, ".teamflow"); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestEnsureCreatesOwnerOnlyFolder(t *testing.T) {
	d := filepath.Join(t.TempDir(), "tf")
	t.Setenv("TEAMFLOW_HOME", d)
	got, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if got != d {
		t.Fatalf("Ensure() = %q, want %q", got, d)
	}
	fi, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o, want 700", fi.Mode().Perm())
	}
	if _, err := Ensure(); err != nil { // a second run accepts its own folder
		t.Fatal(err)
	}
}

func TestHomeRefusesOpenFolder(t *testing.T) {
	for _, mode := range []os.FileMode{0o750, 0o705, 0o777, 0o740} {
		d := filepath.Join(t.TempDir(), "tf")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TEAMFLOW_HOME", d)
		_, err := Ensure()
		if err == nil {
			t.Fatalf("mode %o: Ensure accepted a folder other users can open", mode)
		}
		if !strings.Contains(err.Error(), "chmod 700") {
			t.Fatalf("mode %o: the message should say how to fix it: %v", mode, err)
		}
		fi, _ := os.Stat(d)
		if fi.Mode().Perm() != mode {
			t.Fatalf("Ensure changed the mode itself (%o -> %o); it must refuse, not fix", mode, fi.Mode().Perm())
		}
	}
}

func TestHomeRefusesAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tf")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMFLOW_HOME", f)
	if _, err := Ensure(); err == nil {
		t.Fatal("Ensure accepted a file as the home folder")
	}
}

func TestHomeErrorHidesTheHomePath(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("TEAMFLOW_HOME", "")
	d := filepath.Join(h, ".teamflow")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), h) || !strings.Contains(err.Error(), "~/.teamflow") {
		t.Fatalf("the message should name the folder as ~/.teamflow: %v", err)
	}
}

// Mask writes the user's home folder as ~ wherever it appears; text without it, and a home of "/" (no user
// folder), stay as they are.
func TestMask(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	if got := Mask("cannot write to " + h + "/.local/bin (where teamflow is installed): " + h); got !=
		"cannot write to ~/.local/bin (where teamflow is installed): ~" {
		t.Fatalf("%q", got)
	}
	if got := Mask("/opt/teamflow/bin"); got != "/opt/teamflow/bin" {
		t.Fatalf("%q", got)
	}
	t.Setenv("HOME", "/")
	if got := Mask("/opt/x"); got != "/opt/x" {
		t.Fatalf("%q", got)
	}
}
