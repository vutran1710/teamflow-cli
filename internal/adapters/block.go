package adapters

// The one instruction text in an agent's always-read instruction file, inside a delimited Teamflow block: placed
// word for word, replaced in place when it changes, and removed leaving the person's own content byte for byte.

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/home"
)

// The block's markers: each on a line of its own.
const (
	BlockBegin = "<!-- teamflow:begin (written by `teamflow install`; `teamflow uninstall` removes it) -->"
	BlockEnd   = "<!-- teamflow:end -->"
)

// span finds the block: the start of its begin line and the end of its end line (after the newline). A damaged
// block (a marker without the other, two blocks, out of order) is an error: never guessed at.
func span(content []byte, path string) (start, end int, found bool, err error) {
	damaged := fmt.Errorf("the Teamflow block in %s is damaged (a marker is missing, doubled or out of order): "+
		"fix or delete it by hand, then run this again", home.Display(path))
	b, e := bytes.Count(content, []byte(BlockBegin)), bytes.Count(content, []byte(BlockEnd))
	if b == 0 && e == 0 {
		return 0, 0, false, nil
	}
	if b != 1 || e != 1 {
		return 0, 0, false, damaged
	}
	start, endMark := bytes.Index(content, []byte(BlockBegin)), bytes.Index(content, []byte(BlockEnd))
	if endMark < start {
		return 0, 0, false, damaged
	}
	end = endMark + len(BlockEnd)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return start, end, true, nil
}

func blockOf(text string) string {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return BlockBegin + "\n" + text + BlockEnd + "\n"
}

// PlaceBlock puts text in path's Teamflow block: in place when there is one, else at the end (after a newline
// when the file has content). The file is made when missing.
func PlaceBlock(path, text string) error {
	if strings.Contains(text, BlockBegin) || strings.Contains(text, BlockEnd) {
		return fmt.Errorf("the instruction text holds a Teamflow block marker")
	}
	if err := Writable(path); err != nil {
		return err
	}
	content, err := ReadFile(path)
	if err != nil {
		return err
	}
	start, end, found, err := span(content, path)
	if err != nil {
		return err
	}
	var out []byte
	switch {
	case found:
		out = append(append(append(out, content[:start]...), blockOf(text)...), content[end:]...)
	case len(content) == 0:
		out = []byte(blockOf(text))
	default: // one newline of ours before the block, removed with it
		out = append(append(append(out, content...), '\n'), blockOf(text)...)
	}
	return WriteFile(path, out, 0o644)
}

// HasBlock says whether path holds a Teamflow block.
func HasBlock(path string) (bool, error) {
	content, err := ReadFile(path)
	if err != nil {
		return false, err
	}
	_, _, found, err := span(content, path)
	return found, err
}

// RemoveBlock takes the Teamflow block out of path, with the newline placed before it; false when there was none.
func RemoveBlock(path string) (bool, error) {
	content, err := ReadFile(path)
	if err != nil {
		return false, err
	}
	start, end, found, err := span(content, path)
	if err != nil || !found {
		return false, err
	}
	if start > 0 && content[start-1] == '\n' {
		start--
	}
	out := append(append([]byte{}, content[:start]...), content[end:]...)
	return true, WriteFile(path, out, 0o644)
}
