package deliverables

// The shared Deliverables vectors (protocol/deliverables/vectors.json), recorded from the Python tests: every
// one is run through the Go port and its answer compared with Python's.
//
// Ids the function makes (a new action or milestone in Resolve) are random here; the vectors number them in
// order of first appearance after the ids the arguments hold. The Go answer is renumbered the same way before
// it is compared, so the test checks which items got new ids and where they are referenced, not the ids.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

var vectorsFile = filepath.Join("..", "..", "protocol", "deliverables", "vectors.json")

type vector struct {
	Fn     string                     `json:"fn"`
	Args   map[string]json.RawMessage `json:"args"`
	Result json.RawMessage            `json:"result"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	b, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	return f.Vectors
}

func arg[T any](t *testing.T, v vector, name string) T {
	t.Helper()
	raw, ok := v.Args[name]
	if !ok {
		t.Fatalf("%s: no argument %s", v.Fn, name)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: argument %s: %v", v.Fn, name, err)
	}
	return out
}

// ordered reads a JSON object's pairs in their written order (Python dicts keep insertion order, and the
// answers depend on it).
func ordered(t *testing.T, raw json.RawMessage) [][2]string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var out [][2]string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{k.(string), v})
	}
	return out
}

func day(t *testing.T, v vector, name string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, arg[string](t, v, name))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func run(t *testing.T, v vector) any {
	t.Helper()
	switch v.Fn {
	case "action_status":
		return ActionStatus(arg[protocol.Action](t, v, "action"), arg[[]protocol.StoredSignal](t, v, "results"))
	case "is_late":
		return IsLate(arg[protocol.Action](t, v, "action"), arg[protocol.ActionStatus](t, v, "status"), day(t, v, "today"))
	case "milestone_late":
		return MilestoneLate(arg[protocol.Milestone](t, v, "m"), day(t, v, "today"))
	case "resolve":
		var members []Member
		for _, p := range ordered(t, v.Args["members"]) {
			members = append(members, Member{ID: p[0], Name: p[1]})
		}
		ms, problems := Resolve(arg[[]protocol.MilestoneSpec](t, v, "spec"), arg[*protocol.Deliverables](t, v, "old"),
			arg[[]protocol.Topic](t, v, "topics"), members)
		return []any{ms, problems}
	case "check_structure":
		set := map[string]bool{}
		for _, m := range arg[[]string](t, v, "members") {
			set[m] = true
		}
		return CheckStructure(arg[[]protocol.Milestone](t, v, "milestones"), arg[*protocol.Deliverables](t, v, "old"),
			arg[[]protocol.Topic](t, v, "topics"), set, day(t, v, "published_on"))
	case "merge_milestone":
		specs, problems := MergeMilestone(arg[protocol.Deliverables](t, v, "old"), arg[protocol.MilestoneSpec](t, v, "spec"))
		return []any{specs, problems}
	case "apply_renames":
		var names []Rename
		for _, p := range ordered(t, v.Args["names"]) {
			names = append(names, Rename{ID: p[0], Name: p[1]})
		}
		specs, problems := ApplyRenames(arg[protocol.Deliverables](t, v, "old"), names)
		return []any{specs, problems}
	}
	t.Fatalf("a vector for %q, which this test does not run: add it", v.Fn)
	return nil
}

var newIDPattern = regexp.MustCompile(`(ms|ai)_[0-9A-HJKMNP-TV-Z]{26}`)

// renumber gives the ids in got that the arguments do not hold the next placeholder numbers of their kind, in
// order of first appearance, as the vectors were recorded.
func renumber(args map[string]json.RawMessage, got []byte) []byte {
	known := map[string]bool{}
	next := map[string]int{}
	for _, raw := range args {
		for _, id := range newIDPattern.FindAllString(string(raw), -1) {
			known[id] = true
			if n, err := strconv.Atoi(strings.TrimLeft(id[3:], "0")); err == nil && n > next[id[:3]] {
				next[id[:3]] = n
			}
		}
	}
	given := map[string]string{}
	return newIDPattern.ReplaceAllFunc(got, func(b []byte) []byte {
		id := string(b)
		if known[id] {
			return b
		}
		if p, ok := given[id]; ok {
			return []byte(p)
		}
		next[id[:3]]++
		p := fmt.Sprintf("%s%026d", id[:3], next[id[:3]])
		given[id] = p
		return []byte(p)
	})
}

func TestEveryVectorMatches(t *testing.T) {
	ran := map[string]int{}
	for i, v := range loadVectors(t) {
		got, err := json.Marshal(run(t, v))
		if err != nil {
			t.Fatalf("vector %d (%s): %v", i, v.Fn, err)
		}
		got = renumber(v.Args, got)
		var g, w any
		if err := json.Unmarshal(got, &g); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(v.Result, &w); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("vector %d (%s):\n got  %s\n want %s", i, v.Fn, got, v.Result)
		}
		ran[v.Fn]++
	}
	for _, fn := range []string{"action_status", "is_late", "milestone_late", "resolve", "check_structure",
		"merge_milestone", "apply_renames"} {
		if ran[fn] == 0 {
			t.Errorf("no vector ran %s", fn)
		}
	}
}

// TestRenumberIsNotVacuous: a wrong id in an answer still fails the comparison after renumbering.
func TestRenumberIsNotVacuous(t *testing.T) {
	args := map[string]json.RawMessage{"x": json.RawMessage(`"ai_00000000000000000000000001"`)}
	got := renumber(args, []byte(`["ai_00000000000000000000000001","ai_01HZZZZZZZZZZZZZZZZZZZZZZZ","ai_01HZZZZZZZZZZZZZZZZZZZZZZZ"]`))
	if string(got) != `["ai_00000000000000000000000001","ai_00000000000000000000000002","ai_00000000000000000000000002"]` {
		t.Fatalf("renumbered %s", got)
	}
}
