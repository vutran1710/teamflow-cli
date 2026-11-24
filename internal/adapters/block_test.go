package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const text = "# Teamflow\n\nRecord as you go.\n"

// The instruction text goes in verbatim inside one delimited block; the person's own content outside it stays byte
// for byte, placing twice changes nothing, and removing restores the file exactly.
func TestInstructionBlock(t *testing.T) {
	for name, before := range map[string]string{
		"no file":              "",
		"ends with a newline":  "# Mine\n\nKeep tabs\tand  spaces.\n",
		"no final newline":     "# Mine\nno newline at the end",
		"crlf and unicode":     "line one\r\nđ ü «x»\r\n",
		"just a newline":       "\n",
		"a block-like comment": "<!-- teamflow: notes of my own -->\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sub", "CLAUDE.md")
			if before != "" {
				must(t, os.MkdirAll(filepath.Dir(path), 0o755))
				must(t, os.WriteFile(path, []byte(before), 0o640))
			}
			must(t, PlaceBlock(path, text))
			once := read(t, path)
			if !strings.HasPrefix(once, before) || !strings.Contains(once, BlockBegin+"\n"+text+BlockEnd+"\n") {
				t.Fatalf("placed:\n%q", once)
			}
			must(t, PlaceBlock(path, text))
			if twice := read(t, path); twice != once {
				t.Fatalf("placing twice changed the file:\n%q\n%q", once, twice)
			}
			if before != "" {
				if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
					t.Fatalf("mode changed: %v", fi.Mode())
				}
			}
			has, err := HasBlock(path)
			if err != nil || !has {
				t.Fatalf("HasBlock %v %v", has, err)
			}
			removed, err := RemoveBlock(path)
			must(t, err)
			if !removed || read(t, path) != before {
				t.Fatalf("not restored: %q", read(t, path))
			}
			if removed, err := RemoveBlock(path); err != nil || removed {
				t.Fatalf("removing again: %v %v", removed, err)
			}
		})
	}
}

// A newer text replaces the block in place; the person's lines before and after it are kept.
func TestInstructionBlockUpdatesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	must(t, os.WriteFile(path, []byte("before\n"), 0o644))
	must(t, PlaceBlock(path, "old text\n"))
	must(t, os.WriteFile(path, []byte(read(t, path)+"after, mine\n"), 0o644))
	must(t, PlaceBlock(path, text))
	got := read(t, path)
	if strings.Contains(got, "old text") || !strings.HasPrefix(got, "before\n") || !strings.HasSuffix(got, BlockEnd+"\nafter, mine\n") {
		t.Fatalf("updated:\n%q", got)
	}
	_, err := RemoveBlock(path)
	must(t, err)
	if got := read(t, path); got != "before\nafter, mine\n" {
		t.Fatalf("removed: %q", got)
	}
}

// Sad paths: a damaged block (a begin with no end, two blocks, an end before its begin) is refused, never guessed
// at; a text holding a marker is refused; a folder at the path, and a read-only file, are refused untouched.
func TestInstructionBlockRefusals(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"begin without end": "x\n" + BlockBegin + "\nhalf\n",
		"end without begin": "x\n" + BlockEnd + "\n",
		"two blocks":        BlockBegin + "\na\n" + BlockEnd + "\n" + BlockBegin + "\nb\n" + BlockEnd + "\n",
		"end before begin":  BlockEnd + "\n" + BlockBegin + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".md")
			must(t, os.WriteFile(path, []byte(content), 0o644))
			if err := PlaceBlock(path, text); err == nil || !strings.Contains(err.Error(), "Teamflow block") {
				t.Fatalf("placed into a damaged block: %v", err)
			}
			if _, err := RemoveBlock(path); err == nil {
				t.Fatal("removed from a damaged block")
			}
			if read(t, path) != content {
				t.Fatal("a damaged file was changed")
			}
		})
	}
	path := filepath.Join(dir, "ok.md")
	if err := PlaceBlock(path, "smuggled "+BlockEnd+"\n"); err == nil {
		t.Fatal("a text holding a marker was placed")
	}
	if err := PlaceBlock(dir, text); err == nil {
		t.Fatal("placed into a folder")
	}
	ro := filepath.Join(dir, "ro.md")
	must(t, os.WriteFile(ro, []byte("mine\n"), 0o444))
	if err := PlaceBlock(ro, text); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a read-only file: %v", err)
	}
	if read(t, ro) != "mine\n" {
		t.Fatal("a read-only file was changed")
	}
}

// A file reached through a link (dotfiles kept elsewhere) is written where the link points; the link stays a link.
func TestInstructionBlockThroughALink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "CLAUDE.md")
	must(t, os.MkdirAll(filepath.Dir(real), 0o755))
	must(t, os.WriteFile(real, []byte("mine\n"), 0o644))
	link := filepath.Join(dir, "CLAUDE.md")
	must(t, os.Symlink(real, link))
	must(t, PlaceBlock(link, text))
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if !strings.Contains(read(t, real), text) {
		t.Fatal("the linked file was not written")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}
