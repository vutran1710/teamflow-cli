package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

// A config file the agent wrote (JSON.stringify(v, null, 2)) reads and writes back byte for byte: key order,
// numbers as written, non-ASCII and HTML characters unescaped, empty objects and arrays.
func TestJSONDocRoundTrip(t *testing.T) {
	src := `{
  "zeta": 1,
  "alpha": {
    "nested": [
      1.50,
      -2e3,
      true,
      null,
      "đ <b>&amp;</b> \"q\" \\ \n\t\u0001"
    ],
    "empty": {},
    "none": []
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "my-guard --strict",
            "timeout": 30
          }
        ]
      }
    ]
  }
}
`
	doc, err := ParseJSON([]byte(src), "settings.json")
	must(t, err)
	if got := string(EncodeJSON(doc)); got != src {
		t.Fatalf("not byte for byte:\n%s", got)
	}
}

// Sad paths: malformed JSON, a top level that is not an object, and trailing data are refused with the file's
// name, never treated as empty (and so never overwritten); an empty or blank file is an empty object.
func TestJSONDocRefusals(t *testing.T) {
	for name, src := range map[string]string{
		"malformed":         `{"hooks": {`,
		"an array":          `[1, 2]`,
		"a string":          `"x"`,
		"trailing data":     `{} {}`,
		"trailing comma":    `{"a": 1,}`,
		"comments (JSON5)":  "{\n// mine\n\"a\": 1}",
		"a duplicated key":  `{"a": 1, "a": 2}`,
		"not UTF-8":         "{\"a\": \"\xff\"}",
		"a nested bad item": `{"a": [1, }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseJSON([]byte(src), "settings.json")
			if err == nil || !strings.Contains(err.Error(), "settings.json") {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	for _, blank := range []string{"", "  \n"} {
		doc, err := ParseJSON([]byte(blank), "x.json")
		if err != nil || len(doc) != 0 {
			t.Fatalf("blank file: %v %v", doc, err)
		}
	}
}

// LoadJSON reads a config file (missing: empty); a malformed one is refused and left as it was.
func TestLoadJSON(t *testing.T) {
	dir := t.TempDir()
	doc, err := LoadJSON(filepath.Join(dir, "missing.json"))
	if err != nil || len(doc) != 0 {
		t.Fatalf("missing file: %v %v", doc, err)
	}
	bad := filepath.Join(dir, "bad.json")
	must(t, os.WriteFile(bad, []byte("{nope"), 0o600))
	if _, err := LoadJSON(bad); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("malformed: %v", err)
	}
	doc = ojson.Obj{{Key: "a", Value: ojson.Obj{}}}
	must(t, SaveJSON(filepath.Join(dir, "new.json"), doc, 0o600))
	if fi, _ := os.Stat(filepath.Join(dir, "new.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if got := read(t, filepath.Join(dir, "new.json")); got != "{\n  \"a\": {}\n}\n" {
		t.Fatalf("%q", got)
	}
}
