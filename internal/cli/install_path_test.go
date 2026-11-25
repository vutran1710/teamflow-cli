package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathBinary makes this run's teamflow a file at <tmp>/<name>/teamflow (what os.Executable says), and returns it.
func pathBinary(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	must(t, os.MkdirAll(dir, 0o755))
	bin := filepath.Join(dir, "teamflow")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	old := executable
	executable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executable = old })
	return bin
}

// installJSON runs install --json and reads its answer.
func installJSON(t *testing.T, m *machine) (result, map[string]any) {
	t.Helper()
	r := m.run("", "install", "-a", "claude-code", "-e", "lan@acme.test", "-s", m.srv.URL(), "--json")
	var out map[string]any
	if err := json.Unmarshal([]byte(r.out), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, r.out)
	}
	return r, out
}

// teamflow on the PATH is this binary (directly, or through a link): no warning, on_path true.
func TestInstallOnPath(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.me", lanMe)
	bin := pathBinary(t, "bin")
	t.Setenv("PATH", filepath.Dir(bin))
	r, out := installJSON(t, m)
	wantCode(t, r, 0)
	if out["on_path"] != true {
		t.Fatalf("on_path %v", out["on_path"])
	}
	lacks(t, "stderr", r.err, "not on your PATH")
	// Through a link in another folder on the PATH: the same file.
	links := filepath.Join(t.TempDir(), "links")
	must(t, os.MkdirAll(links, 0o755))
	must(t, os.Symlink(bin, filepath.Join(links, "teamflow")))
	t.Setenv("PATH", links)
	r, out = installJSON(t, m)
	wantCode(t, r, 0)
	if out["on_path"] != true {
		t.Fatalf("through a link: on_path %v\n%s", out["on_path"], r.err)
	}
}

// teamflow is not on the PATH, or another teamflow (an older install, the retired Python one) comes first: the
// install still sets everything up and exits 0, with a loud warning naming the folder to add for zsh and bash;
// --json says on_path false. Sad paths: not found at all, a different file found first, a teamflow that is a
// folder, and the plain (not --json) output.
func TestInstallNotOnPath(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.me", lanMe)
	bin := pathBinary(t, "bin")
	dir := filepath.Dir(bin)
	folder := t.TempDir() // a folder named teamflow on the PATH is not a teamflow
	must(t, os.MkdirAll(filepath.Join(folder, "teamflow"), 0o755))
	t.Setenv("PATH", folder)
	r, out := installJSON(t, m)
	wantCode(t, r, 0)
	if out["on_path"] != false || out["installed"] == nil {
		t.Fatalf("answer %v", out)
	}
	contains(t, "stderr", r.err, "WARNING: teamflow is not on your PATH",
		`zsh:  echo 'export PATH="`+dir+`:$PATH"' >> ~/.zshrc`,
		`bash: echo 'export PATH="`+dir+`:$PATH"' >> ~/.bashrc`)
	if !strings.Contains(r.err, "claude-code") { // what was set up is still said
		t.Fatalf("stderr %s", r.err)
	}

	// Another teamflow comes first.
	other := filepath.Join(t.TempDir(), "old")
	must(t, os.MkdirAll(other, 0o755))
	must(t, os.WriteFile(filepath.Join(other, "teamflow"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", other+string(os.PathListSeparator)+dir)
	r, out = installJSON(t, m)
	wantCode(t, r, 0)
	if out["on_path"] != false {
		t.Fatalf("shadowed: on_path %v", out["on_path"])
	}
	contains(t, "stderr", r.err, "WARNING: the teamflow on your PATH is "+filepath.Join(other, "teamflow")+", not this one ("+bin+")",
		`export PATH="`+dir+`:$PATH"`)

	// Plain output: the same warning, exit 0, the human lines on stdout.
	t.Setenv("PATH", t.TempDir())
	r = m.run("", "install", "-a", "claude-code", "-e", "lan@acme.test", "-s", m.srv.URL())
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "claude-code:\n", "Done.")
	contains(t, "stderr", r.err, "WARNING: teamflow is not on your PATH")
}

// Under the home folder the instruction names $HOME, never the person's own path.
func TestInstallPathHintUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := pathHint(filepath.Join(home, ".local", "bin"))
	if !strings.Contains(got, `export PATH="$HOME/.local/bin:$PATH"`) || strings.Contains(got, home) {
		t.Fatalf("hint %q", got)
	}
}
