package adapters

// What every adapter's install and uninstall share: the install lock, the manifest of what install created (so
// uninstall takes out exactly that), Teamflow's entry in an agent's JSON map, allow and deny rules added to
// an agent's list, the Teamflow home as commands and paths name it, and /teamf as a skill.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// InstallLock serializes installs and uninstalls on this machine (a file lock in the Teamflow home).
func InstallLock() (func(), error) {
	dir, err := home.Ensure()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "install.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

// ShellQuote quotes s for a POSIX shell when it needs it.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// HookCommand is how an agent runs Teamflow's hook for one of its events.
func HookCommand(binary, agent, event string) string {
	return ShellQuote(binary) + " hook " + agent + " " + event
}

// ------------------------------------------------------------------ the manifest

// Manifest records what install created or added that is not Teamflow's by its look alone: containers it made
// ("file", "file:key", "file:key.sub"), and the deny rules it added. Uninstall takes out exactly that.
type Manifest struct {
	Created []string
	Deny    []string
	Allow   []string
	// Original is each edited file's bytes before Teamflow first wrote it, so uninstall gives the person's file
	// back byte for byte (their layout too) when nothing but Teamflow's entries changed. Kept 0600, beside it.
	Original map[string]string
	Found    bool // the manifest file existed
	path     string
}

// LoadManifest reads the manifest at path (none: an empty one). A malformed manifest is refused.
func LoadManifest(path string) (Manifest, error) {
	doc, err := LoadJSON(path)
	if err != nil {
		return Manifest{}, err
	}
	b, _ := ReadFile(path)
	m := Manifest{Found: len(bytes.TrimSpace(b)) > 0, path: path}
	m.Created, m.Deny, m.Allow = Strings(get(doc, "created")), Strings(get(doc, "deny")), Strings(get(doc, "allow"))
	m.Original = map[string]string{}
	if o, ok := Obj(get(doc, "original")); ok {
		for _, kv := range o {
			if s, ok := kv.Value.(string); ok {
				m.Original[kv.Key] = s
			}
		}
	}
	return m, nil
}

// Save writes the manifest (0600: it may hold copies of the person's files).
func (m *Manifest) Save() error {
	slices.Sort(m.Created)
	m.Created = slices.Compact(m.Created)
	doc := ojson.Obj{{Key: "created", Value: anyList(m.Created)}, {Key: "deny", Value: anyList(m.Deny)}}
	if len(m.Allow) > 0 {
		doc = append(doc, ojson.KV{Key: "allow", Value: anyList(m.Allow)})
	}
	if len(m.Original) > 0 {
		var orig ojson.Obj
		keys := make([]string, 0, len(m.Original))
		for k := range m.Original {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			orig = append(orig, ojson.KV{Key: k, Value: m.Original[k]})
		}
		doc = append(doc, ojson.KV{Key: "original", Value: orig})
	}
	return SaveJSON(m.path, doc, 0o600)
}

// Remember keeps the file's bytes as they are before Teamflow first writes it (once: a later install keeps the
// first copy; a file Teamflow made is not kept).
func (m *Manifest) Remember(name, path string) {
	if m.Original == nil {
		m.Original = map[string]string{}
	}
	if _, ok := m.Original[name]; ok || m.Made(name) {
		return
	}
	if b, err := ReadFile(path); err == nil && b != nil {
		m.Original[name] = string(b)
	}
}

// Restored is what to write for doc at uninstall: the person's original bytes when doc is the same document,
// else doc in the agent's layout.
func (m Manifest) Restored(name string, doc ojson.Obj) []byte {
	enc := EncodeJSON(doc)
	if orig, ok := m.Original[name]; ok {
		if o, err := ParseJSON([]byte(orig), name); err == nil && bytes.Equal(EncodeJSON(o), enc) {
			return []byte(orig)
		}
	}
	return enc
}

// Remove deletes the manifest file.
func (m Manifest) Remove() error {
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Made says whether install created the container c.
func (m Manifest) Made(c string) bool { return slices.Contains(m.Created, c) }

// Mark records that install created the container c.
func (m *Manifest) Mark(c string) { m.Created = append(m.Created, c) }

// MarkFileIfNew records the file as created by install when it does not exist yet.
func (m *Manifest) MarkFileIfNew(name, path string) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		m.Mark(name)
	}
}

// MarkDirs records each folder (relative to base, "." for base itself) that does not exist yet, so uninstall can
// take it out again when it is left empty.
func (m *Manifest) MarkDirs(base string, rels ...string) {
	for _, rel := range rels {
		if _, err := os.Lstat(filepath.Join(base, rel)); errors.Is(err, fs.ErrNotExist) {
			m.Mark("dir:" + rel)
		}
	}
}

// RemoveDirs removes the folders install made under base, deepest first, each only when it is empty.
func (m Manifest) RemoveDirs(base string) {
	var dirs []string
	for _, c := range m.Created {
		if rel, ok := strings.CutPrefix(c, "dir:"); ok {
			dirs = append(dirs, filepath.Clean(rel))
		}
	}
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
	for _, rel := range dirs {
		if rel == "." {
			_ = os.Remove(base)
			continue
		}
		_ = os.Remove(filepath.Join(base, rel))
	}
}

// ------------------------------------------------------------------ editing an agent's JSON file

func get(o ojson.Obj, key string) any {
	v, _ := o.Get(key)
	return v
}

// Field is o's key (nil when absent).
func Field(o ojson.Obj, key string) any { return get(o, key) }

// Strings are v's strings when v is a list.
func Strings(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func anyList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// SetEntry is doc with doc[key][name] = entry (in place when it was there); key created when missing (recorded in m
// as "<file>:<key>"). Refused when doc[key] is not an object.
func SetEntry(doc ojson.Obj, file, key, name string, entry any, m *Manifest) (ojson.Obj, error) {
	v, has := doc.Get(key)
	if !has {
		v = ojson.Obj{}
		m.Mark(file + ":" + key)
	}
	o, ok := v.(ojson.Obj)
	if !ok {
		return nil, fmt.Errorf("%s: %s is not an object; fix it by hand first", file, key)
	}
	return doc.Set(key, o.Set(name, entry)), nil
}

// DropEntry is doc without doc[key][name], and without doc[key] when that leaves it empty and install made it
// (or there is no manifest: an empty map of Teamflow's). Whether it changed.
func DropEntry(doc ojson.Obj, file, key, name string, m Manifest) (ojson.Obj, bool) {
	o, ok := Obj(get(doc, key))
	if !ok {
		return doc, false
	}
	o, had := Without(o, name)
	if !had {
		return doc, false
	}
	if len(o) == 0 && (m.Made(file+":"+key) || !m.Found) {
		doc, _ = Without(doc, key)
		return doc, true
	}
	return doc.Set(key, o), true
}

// CheckShape refuses doc when doc[key] exists and is not an object, or doc[key][list] exists and is not a list.
func CheckShape(doc ojson.Obj, file, key, list string) error {
	v, has := doc.Get(key)
	if !has {
		return nil
	}
	o, ok := v.(ojson.Obj)
	if !ok {
		return fmt.Errorf("%s: %s is not an object; fix it by hand first", file, key)
	}
	if list == "" {
		return nil
	}
	if l, has := o.Get(list); has {
		if _, ok := l.([]any); !ok {
			return fmt.Errorf("%s: %s.%s is not a list; fix it by hand first", file, key, list)
		}
	}
	return nil
}

// AddRules is doc with want in doc[key][list] (deny rules for the Teamflow home, allow rules for teamflow
// commands): a rule the person already had is never recorded as Teamflow's, one an earlier install added and no
// longer wanted goes. *ours (m.Deny or m.Allow) becomes the rules Teamflow added.
func AddRules(doc ojson.Obj, file, key, list string, want []string, ours *[]string, m *Manifest) (ojson.Obj, error) {
	if err := CheckShape(doc, file, key, list); err != nil {
		return nil, err
	}
	v, has := doc.Get(key)
	if !has {
		v = ojson.Obj{}
		m.Mark(file + ":" + key)
	}
	o := v.(ojson.Obj)
	lv, has := o.Get(list)
	if !has {
		lv = []any{}
		m.Mark(file + ":" + key + "." + list)
	}
	rules := lv.([]any)
	var kept []any
	for _, r := range rules {
		if rs, ok := r.(string); ok && slices.Contains(*ours, rs) && !slices.Contains(want, rs) {
			continue
		}
		kept = append(kept, r)
	}
	var added []string
	for _, r := range want {
		present := slices.ContainsFunc(kept, func(x any) bool { return x == r })
		if slices.Contains(*ours, r) {
			added = append(added, r)
			if !present {
				kept = append(kept, r)
			}
			continue
		}
		if present {
			continue // the person's own rule: never taken out
		}
		kept = append(kept, r)
		added = append(added, r)
	}
	if kept == nil {
		kept = []any{}
	}
	*ours = added
	return doc.Set(key, o.Set(list, kept)), nil
}

// StripRules takes the rules install added (ours) out of doc[key][list], and the containers it made when left
// empty.
func StripRules(doc ojson.Obj, file, key, list string, ours []string, m Manifest) (ojson.Obj, bool) {
	o, ok := Obj(get(doc, key))
	if !ok {
		return doc, false
	}
	rules, ok := get(o, list).([]any)
	if !ok {
		return doc, false
	}
	var kept []any
	for _, r := range rules {
		if rs, ok := r.(string); ok && slices.Contains(ours, rs) {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == len(rules) && !(len(rules) == 0 && m.Made(file+":"+key+"."+list)) {
		return doc, false
	}
	if len(kept) == 0 && m.Made(file+":"+key+"."+list) {
		o, _ = Without(o, list)
	} else {
		if kept == nil {
			kept = []any{}
		}
		o = o.Set(list, kept)
	}
	if len(o) == 0 && m.Made(file+":"+key) {
		doc, _ = Without(doc, key)
		return doc, true
	}
	return doc.Set(key, o), true
}

// SaveOrRemove writes doc to path at uninstall (the person's original bytes when nothing else changed), or
// removes the file when doc is empty and install made the file.
func SaveOrRemove(path, file string, doc ojson.Obj, m Manifest, perm os.FileMode) error {
	if len(doc) == 0 && m.Made(file) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return WriteFile(path, m.Restored(file, doc), perm)
}

// ------------------------------------------------------------------ the Teamflow home in commands and paths

// HomeForms are the Teamflow home's absolute path and the ways a command can name it (~/…, $HOME/…, ${HOME}/…).
func HomeForms() (abs string, texts []string) {
	abs, _ = filepath.Abs(home.Dir())
	h, _ := os.UserHomeDir()
	texts = []string{abs}
	if rel, ok := strings.CutPrefix(abs, h+string(filepath.Separator)); ok && h != "" && h != "/" {
		texts = append(texts, "~/"+rel, "$HOME/"+rel, "${HOME}/"+rel)
	}
	return abs, texts
}

// BinaryInHome says whether binary lives in the Teamflow home or under a .teamflow/ path: rules matching commands
// that name the home would then block /teamf.
func BinaryInHome(binary string) bool {
	abs, _ := HomeForms()
	return strings.HasPrefix(binary, abs+string(filepath.Separator)) || strings.Contains(binary, ".teamflow/")
}

// NamesHome says whether a tool call's text (a command, a path, its arguments as JSON) names the Teamflow home,
// where the person's sign-in and keys live. The binary's own path (in /teamf) does not count. Like any text rule it
// stops only what names the folder: a command reaching it without naming it is not stopped.
func NamesHome(text, binary string) bool {
	if binary != "" {
		text = strings.ReplaceAll(text, binary, "")
	}
	_, forms := HomeForms()
	for _, f := range append(forms, ".teamflow/", "TEAMFLOW_HOME") {
		if strings.Contains(text, f) {
			return true
		}
	}
	return false
}

// HomeRefusal is what a hook tells the agent when it refuses a tool call naming the Teamflow home.
const HomeRefusal = "Teamflow keeps the person's sign-in and keys in its home folder; agents do not read or change it. " +
	"Use the teamflow commands or /teamf instead."

// ------------------------------------------------------------------ /teamf as a skill

// TeamfMark is in every /teamf Teamflow writes (as a command or a skill), so uninstall knows its own.
const TeamfMark = " team --relay "

// TeamfSkill is /teamf as an agent skill (SKILL.md): name and description frontmatter (plus extra lines), then the
// one /teamf text with the binary filled in and the person's words standing for the arguments.
func TeamfSkill(binary string, extra ...string) string {
	body := strings.NewReplacer("{TEAMFLOW}", ShellQuote(binary),
		"{ARGUMENTS}", "<the words the person typed after /teamf>").Replace(protocol.AgentTeamf)
	desc, _, _ := strings.Cut(protocol.AgentTeamf, "\n\n")
	// Quoted: the text has ": " in it, which a plain YAML value can't hold (Copilot CLI skips such a skill).
	d, _ := json.Marshal(strings.Join(strings.Fields(desc), " ") + " Use only when the person types /teamf.")
	fm := "---\nname: teamf\ndescription: " + string(d) + "\n"
	for _, e := range extra {
		fm += e + "\n"
	}
	return fm + "---\n" + body
}

// RemoveIfOurs removes the file at path when it holds mark (Teamflow's own), and, with dirToo, its folder when that
// is left empty. Whether it removed it.
func RemoveIfOurs(path, mark string, dirToo bool) (bool, error) {
	b, err := ReadFile(path)
	if err != nil || b == nil || !strings.Contains(string(b), mark) {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	if dirToo {
		_ = os.Remove(filepath.Dir(path)) // only when empty
	}
	return true, nil
}

// ------------------------------------------------------------------ the instruction file

// PlaceInstructions puts the one instruction text in the Teamflow block of path (recorded in m as made when the
// file did not exist).
func PlaceInstructions(path, name string, m *Manifest) error {
	m.MarkFileIfNew(name, path)
	return PlaceBlock(path, protocol.AgentInstructions)
}

// RemoveInstructions takes the block out of path; a file install made, left empty, goes too (one the person had,
// or wrote into since, stays). Whether it removed a block.
func RemoveInstructions(path, name string, m Manifest) (bool, error) {
	removed, err := RemoveBlock(path)
	if err != nil || !removed {
		return removed, err
	}
	if b, err := ReadFile(path); err == nil && len(b) == 0 && m.Made(name) {
		if err := os.Remove(path); err != nil {
			return true, err
		}
	}
	return true, nil
}

// CheckWritable refuses the install when any of paths can't be written (read-only, not a file, a link to nothing).
func CheckWritable(paths ...string) error {
	for _, p := range paths {
		if err := Writable(p); err != nil {
			return err
		}
	}
	return nil
}

// UpdateJSON rewrites a JSON file the agent itself may be writing: read, change, and written only if the file is
// still as it was read (else read again, a few times). A new file is made with perm.
func UpdateJSON(path string, perm os.FileMode, change func(ojson.Obj) (ojson.Obj, error)) error {
	return UpdateJSONWith(path, perm, change, func(d ojson.Obj) []byte { return EncodeJSON(d) })
}

// UpdateJSONWith is UpdateJSON writing the changed document as encode gives it.
func UpdateJSONWith(path string, perm os.FileMode, change func(ojson.Obj) (ojson.Obj, error), encode func(ojson.Obj) []byte) error {
	for range 5 {
		before, err := ReadFile(path)
		if err != nil {
			return err
		}
		doc, err := ParseJSON(before, home.Display(path))
		if err != nil {
			return err
		}
		doc, err = change(doc)
		if err != nil {
			return err
		}
		now, err := ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(before, now) {
			continue
		}
		if before == nil && len(doc) == 0 { // no file, and nothing to put in one
			return nil
		}
		if doc == nil { // the change asks for the file to go
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		return WriteFile(path, encode(doc), perm)
	}
	return fmt.Errorf("%s keeps changing (the agent is writing it): try again", home.Display(path))
}

// WorkCommands are the teamflow commands an agent may run with no prompt. Every other one (SetupCommands) runs
// only with the agent's own permission prompt. A CLI test keeps the two lists complete.
var WorkCommands = []string{"projects", "status", "whoami", "inbox", "stop", "approve", "reject", "team", "topic",
	"signal", "artifact", "deliverables", "action", "drift", "project", "org", "profile", "tasks", "log"}

// SetupCommands change where Teamflow is set up, who is signed in, or the binary itself: never auto-allowed. sync
// is here until the background sync is built (Task 10), so no spelling of `sync --stop` can slip through.
// outbox is here: dropping queued signals is the person's decision.
var SetupCommands = []string{"login", "logout", "install", "uninstall", "self-update", "hook", "debug", "sync", "outbox"}

// notPlain are the characters that make a command more than one plain teamflow run: chaining, pipes, redirects,
// substitution and expansion (variables, globs, braces, ~), escapes and line breaks.
const notPlain = ";&|<>`$()\\\n\r*?[]{}~"

// TeamflowCommand classifies a shell command: teamflow says its first word is teamflow (by name, or binary's path,
// quoted or not); auto says it may run with no prompt: one plain run (none of notPlain), of a work command, its
// subcommand first (no option before it). Where an agent's allow rule is a prefix, a hook
// answers "ask" for teamflow && !auto. Strict on purpose: the CLI refuses files in the Teamflow home too.
func TeamflowCommand(cmd, binary string) (teamflow, auto bool) {
	cmd = strings.TrimSpace(cmd)
	rest, ok := "", false
	for _, head := range []string{"teamflow", binary, ShellQuote(binary), "'" + binary + "'", `"` + binary + `"`} {
		if head == "" {
			continue
		}
		if r, cut := strings.CutPrefix(cmd, head); cut && (r == "" || r[0] == ' ' || r[0] == '\t') {
			rest, ok = r, true
			break
		}
	}
	if !ok {
		return false, false
	}
	if strings.ContainsAny(cmd, notPlain) {
		return true, false
	}
	words := strings.Fields(rest)
	for i := range words {
		words[i] = strings.Trim(words[i], `'"`)
	}
	if len(words) == 0 || !slices.Contains(WorkCommands, words[0]) {
		return true, false
	}
	return true, true
}
