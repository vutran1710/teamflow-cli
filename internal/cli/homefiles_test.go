package cli

// No command reads a file in the Teamflow home (the sign-in and keys) for an agent, or writes one there, whatever
// way the path names it: the shell has expanded it already, so the check is on the resolved path, by identity.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// homeForms are the ways a path can reach the home's config.json: directly, through `..`, through a link to the
// file, through a link to the home, and a case variant (only where the disk ignores case).
func homeForms(t *testing.T, m *machine) map[string]string {
	t.Helper()
	cfg := filepath.Join(m.dir, "config.json")
	out := map[string]string{
		"direct":   cfg,
		"dotdot":   filepath.Join(m.dir, "..", filepath.Base(m.dir), "config.json"),
		"the home": m.dir,
	}
	links := t.TempDir()
	must(t, os.Symlink(cfg, filepath.Join(links, "notes.md")))
	must(t, os.Symlink(m.dir, filepath.Join(links, "dir")))
	out["a link to the file"] = filepath.Join(links, "notes.md")
	out["a link to the home"] = filepath.Join(links, "dir", "config.json")
	upper := filepath.Join(filepath.Dir(m.dir), strings.ToUpper(filepath.Base(m.dir)), "config.json")
	if a, err := os.Stat(upper); err == nil {
		if b, err := os.Stat(cfg); err == nil && os.SameFile(a, b) {
			out["a case variant"] = upper
		}
	}
	return out
}

// project plan --plan-file / --instruction-file and deliverables validate refuse every form; nothing is sent and
// the file's content is never printed.
// Sad paths: direct, .., a link to the file, a link to the home, a case variant, the home folder itself.
func TestFileFlagsRefuseTheHome(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.srv.on("project.get", pricingGet)
	m.srv.on("project.write_state", okAnswer(t, "project.write_state"))
	secret := "tfa_example"
	for name, p := range homeForms(t, m) {
		for _, flag := range []string{"--plan-file", "--instruction-file"} {
			r := m.run("", "project", "plan", "--project", P, flag, p, "--yes")
			wantCode(t, r, 1)
			contains(t, "stderr", r.err, "Teamflow's home")
			if strings.Contains(r.out+r.err, secret) {
				t.Fatalf("%s %s: the key was printed", name, flag)
			}
		}
		r := m.run("", "deliverables", "validate", "--file", p)
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "Teamflow's home")
		if strings.Contains(r.out+r.err, secret) {
			t.Fatalf("%s validate: the key was printed", name)
		}
	}
	noCall(t, m.srv, "project.write_state")
}

// artifact pull -o never writes into the home (it could replace the config), in any form; artifact write and
// profile avatar never read from it.
// Sad paths: -o direct, through a link to the home, the home's own config.json; write and avatar of a file in it.
func TestArtifactPathsRefuseTheHome(t *testing.T) {
	m := artifactMachine(t)
	links := t.TempDir()
	must(t, os.Symlink(m.dir, filepath.Join(links, "dir")))
	before, _ := os.ReadFile(filepath.Join(m.dir, "config.json"))
	for _, out := range []string{filepath.Join(m.dir, "spec.md"), filepath.Join(links, "dir", "spec.md"), filepath.Join(m.dir, "config.json")} {
		r := m.run("", "artifact", "pull", S1, "-o", out)
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "Teamflow's home")
	}
	if _, err := os.Stat(filepath.Join(m.dir, "spec.md")); err == nil {
		t.Fatal("a file was written in the home")
	}
	if after, _ := os.ReadFile(filepath.Join(m.dir, "config.json")); string(after) != string(before) {
		t.Fatal("the config was changed")
	}
	noCall(t, m.srv, "artifact.get")
	// Without -o, run from inside the home: the teammate's name would land there; refused, nothing written.
	m.srv.on("artifact.get", pullAnswer([]byte("# Pricing\nv2\n")))
	t.Chdir(m.dir)
	r := m.run("", "artifact", "pull", S1)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "Teamflow's home")
	if _, err := os.Stat(filepath.Join(m.dir, "spec.md")); err == nil {
		t.Fatal("pull wrote the teammate's file into the home")
	}
	t.Chdir(links)
	r = m.run("", "artifact", "write", filepath.Join(links, "dir", "config.json"), "--topic", "rounding", "--summary", "s", "--project", P)
	wantCode(t, r, 1)
	r = m.run("", "profile", "avatar", filepath.Join(m.dir, "config.json"))
	wantCode(t, r, 1)
	noCall(t, m.srv, "artifact.write")
}
