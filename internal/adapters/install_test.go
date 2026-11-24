package adapters

// The helpers every adapter's install shares: the manifest's byte-for-byte restore, deny rules, the Teamflow home
// in tool calls, the folders install made.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

func instHome(t *testing.T) (user, tf string) {
	t.Helper()
	user = filepath.Join(t.TempDir(), "user")
	tf = filepath.Join(user, ".teamflow")
	if err := os.MkdirAll(tf, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", user)
	t.Setenv("TEAMFLOW_HOME", tf)
	return user, tf
}

// NamesHome: every form of the home's path, a .teamflow/ path and TEAMFLOW_HOME count; the binary's own path, the
// project's .teamflow share file and ordinary text do not. A home outside HOME is matched by its absolute path.
func TestNamesHome(t *testing.T) {
	_, tf := instHome(t)
	bin := filepath.Join(tf, "bin", "teamflow") // a binary in the home: its own path never counts
	for _, s := range []string{"cat ~/.teamflow/config.json", "ls $HOME/.teamflow", "ls ${HOME}/.teamflow", tf,
		"x/.teamflow/store.db", "echo $TEAMFLOW_HOME", `{"path":"` + tf + `/config.json"}`} {
		if !NamesHome(s, "") {
			t.Fatalf("%q not caught", s)
		}
	}
	for _, s := range []string{"ls", "cat .teamflow", "git log", bin + " team --relay status"} {
		if NamesHome(s, bin) {
			t.Fatalf("%q caught", s)
		}
	}
	elsewhere := filepath.Join(t.TempDir(), "tf")
	t.Setenv("TEAMFLOW_HOME", elsewhere)
	if !NamesHome("cat "+elsewhere+"/config.json", "") || NamesHome("cat ~/notes", "") {
		t.Fatal("a home outside HOME")
	}
}

// AddRules never records the person's own rule as Teamflow's, replaces rules an earlier install added and no
// longer wants, and StripRules takes out only Teamflow's, and the containers it made.
// Sad paths: the person's rule equal to one of Teamflow's; a moved home; a wrong shape.
func TestRules(t *testing.T) {
	m := Manifest{}
	doc := ojson.Obj{{Key: "permissions", Value: ojson.Obj{{Key: "deny", Value: []any{"mine", "B"}}}}}
	doc, err := AddRules(doc, "f", "permissions", "deny", []string{"A", "B"}, &m.Deny, &m)
	if err != nil || !slices.Equal(m.Deny, []string{"A"}) {
		t.Fatalf("added %v %v", m.Deny, err)
	}
	doc, _ = AddRules(doc, "f", "permissions", "deny", []string{"C", "B"}, &m.Deny, &m) // the home moved: A goes
	perms, _ := Obj(Field(doc, "permissions"))
	if got := Strings(Field(perms, "deny")); !slices.Equal(got, []string{"mine", "B", "C"}) {
		t.Fatalf("after a move: %v", got)
	}
	doc, changed := StripRules(doc, "f", "permissions", "deny", m.Deny, m)
	perms, _ = Obj(Field(doc, "permissions"))
	if !changed || !slices.Equal(Strings(Field(perms, "deny")), []string{"mine", "B"}) {
		t.Fatalf("strip: %v", Field(perms, "deny"))
	}
	fresh := Manifest{}
	d2, _ := AddRules(ojson.Obj{}, "f", "permissions", "deny", []string{"A"}, &fresh.Deny, &fresh)
	if d2, _ = StripRules(d2, "f", "permissions", "deny", fresh.Deny, fresh); len(d2) != 0 {
		t.Fatalf("containers install made are left: %v", d2)
	}
	if _, err := AddRules(ojson.Obj{{Key: "permissions", Value: []any{}}}, "f", "permissions", "deny", nil, new([]string), &Manifest{}); err == nil {
		t.Fatal("a wrong shape taken")
	}
}

// Restored gives the person's original bytes when only Teamflow's entries changed, and the agent's layout when
// the person changed the file meanwhile.
func TestManifestRestored(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	if err := os.WriteFile(f, []byte(`{"a": 1,   "b": [1,2]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(filepath.Join(dir, "m.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Remember("c.json", f)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "m.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode %v", fi.Mode())
	}
	m, _ = LoadManifest(filepath.Join(dir, "m.json"))
	same := ojson.Obj{{Key: "a", Value: 1}, {Key: "b", Value: []any{1, 2}}}
	if got := string(m.Restored("c.json", same)); got != `{"a": 1,   "b": [1,2]}` {
		t.Fatalf("not the original bytes: %q", got)
	}
	changed := ojson.Obj{{Key: "a", Value: 2}}
	if got := string(m.Restored("c.json", changed)); got != "{\n  \"a\": 2\n}\n" {
		t.Fatalf("changed: %q", got)
	}
	m.Remember("c.json", filepath.Join(dir, "other")) // a later install keeps the first copy
	if m.Original["c.json"] != `{"a": 1,   "b": [1,2]}` {
		t.Fatal("the first copy was replaced")
	}
}

// RemoveDirs removes only the folders install made, and only when empty.
func TestRemoveDirs(t *testing.T) {
	base := filepath.Join(t.TempDir(), "agent")
	m := Manifest{}
	m.MarkDirs(base, ".", "a", "a/b")
	if err := os.MkdirAll(filepath.Join(base, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "a", "mine"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m.RemoveDirs(base)
	if _, err := os.Stat(filepath.Join(base, "a", "b")); err == nil {
		t.Fatal("an empty folder install made is left")
	}
	if _, err := os.Stat(filepath.Join(base, "a", "mine")); err != nil {
		t.Fatal("a folder holding the person's file was removed")
	}
}

// The /teamf skill's frontmatter is valid YAML: the description is quoted (it holds ": "), so agents that parse it
// strictly (Copilot CLI skipped the skill otherwise) load it.
func TestTeamfSkillFrontmatter(t *testing.T) {
	s := TeamfSkill("/opt/my bin/teamflow", "allowed-tools: shell(teamflow:*)")
	fm, _, _ := strings.Cut(strings.TrimPrefix(s, "---\n"), "\n---\n")
	for _, line := range strings.Split(fm, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok || strings.Contains(k, " ") {
			t.Fatalf("not a key: %q", line)
		}
		if strings.Contains(v, ": ") && !strings.HasPrefix(v, `"`) {
			t.Fatalf("an unquoted value holding ': ': %q", line)
		}
	}
	if !strings.Contains(s, "'/opt/my bin/teamflow' team --relay ") {
		t.Fatal("the binary is not quoted in the body")
	}
}
