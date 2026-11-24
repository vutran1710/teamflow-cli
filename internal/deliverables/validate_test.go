package deliverables

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

const (
	tpRounding = "tp_00000000000000000000000001"
	tpClosed   = "tp_00000000000000000000000002"
	mbLan      = "mb_00000000000000000000000001"
	mbHuy      = "mb_00000000000000000000000002"
)

func ctx() Context {
	return Context{
		Topics: []protocol.Topic{
			{ID: tpRounding, Name: "Rounding"},
			{ID: tpClosed, Name: "Old", Closed: true},
		},
		Members: []Member{{ID: mbLan, Name: "Lan"}, {ID: mbHuy, Name: "Huy"}},
	}
}

func TestValidateAnswersLikePython(t *testing.T) {
	ok := ValidateJSON(json.RawMessage(`[{"name": "Design", "due": "2026-02-01", "actions": [
		{"name": "Draft rules", "topic": "rounding", "due": "2026-01-20", "owner": "huy"},
		{"name": "Review", "topic": "`+tpRounding+`", "due": "2026-01-25", "after": ["draft rules"]}]}]`), ctx())
	b, _ := json.Marshal(ok)
	if string(b) != `{"valid":true,"problems":[]}` {
		t.Fatalf("valid answer %s", b)
	}

	bad := ValidateJSON(json.RawMessage(`[{"name": "Design", "due": "2026-02-01", "actions": [
		{"name": "Draft", "topic": "Old", "due": "2026-03-01", "owner": "Kim"},
		{"name": "Ship", "topic": "Rounding", "due": "2026-01-25", "after": ["Nope"]}]}]`), ctx())
	b, _ = json.Marshal(bad)
	if bad.Valid || !strings.HasPrefix(string(b), `{"valid":false,"problems":[`) {
		t.Fatalf("invalid answer %s", b)
	}
	want := []string{
		"action 'Draft': no member 'Kim' in this project (members: Lan, Huy)",
		"action 'Ship' depends on 'Nope': no action has that name or id",
	}
	if strings.Join(bad.Problems, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(bad.Problems, "\n"), strings.Join(want, "\n"))
	}

	// A closed topic and a late action are check_structure's.
	late := Validate([]protocol.MilestoneSpec{{Name: "M", Due: "2026-02-01", Actions: []protocol.ActionSpec{
		{Name: "A", Topic: "Old", Due: "2026-03-01", After: []string{}}}}}, ctx())
	if late.Valid || strings.Join(late.Problems, "\n") != "action 'A' is in topic 'Old', which is closed\n"+
		"action 'A' is due 2026-03-01, after its milestone 'M' (2026-02-01)" {
		t.Fatalf("problems %q", late.Problems)
	}
}

func TestShapeProblemsSayWhere(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"not a list":    {`"nope"`, []string{"the structure: Input should be a valid list"}},
		"not JSON":      {`[`, []string{"the structure: Invalid JSON"}},
		"not an object": {`[5]`, []string{"milestone 1: Input should be a valid dictionary or instance of MilestoneSpec"}},
		"an empty milestone": {`[{"name": "M", "due": "2026-01-01", "actions": []}]`,
			[]string{"milestone 1, actions: List should have at least 1 item after validation, not 0"}},
		"missing fields": {`[{"actions": [{"name": "a"}]}]`, []string{
			"milestone 1, name: Field required",
			"milestone 1, due: Field required",
			"milestone 1, action 1, topic: Field required",
			"milestone 1, action 1, due: Field required",
		}},
		"bad values": {`[{"id": "ms_x", "name": "", "due": "2026-13-01", "actions": [{"id": "ai_bad", "name": "` +
			strings.Repeat("a", 121) + `", "topic": "t", "due": "x", "owner": "", "after": ["", "", "", "", "", "", "", "", "", "", ""], "extra": 1}]}]`,
			[]string{
				"milestone 1, id: String should match pattern '^ms_[0-9A-HJKMNP-TV-Z]{26}$'",
				"milestone 1, name: String should have at least 1 character",
				"milestone 1, due: Input should be a valid date or datetime, month value is outside expected range of 1-12",
				"milestone 1, action 1, id: String should match pattern '^ai_[0-9A-HJKMNP-TV-Z]{26}$'",
				"milestone 1, action 1, name: String should have at most 120 characters",
				"milestone 1, action 1, due: Input should be a valid date or datetime, input is too short",
				"milestone 1, action 1, owner: String should have at least 1 character",
				"milestone 1, action 1, after: List should have at most 10 items after validation, not 11",
				"milestone 1, action 1, extra: Extra inputs are not permitted",
			}},
		"wrong types": {`[{"name": 5, "due": null, "actions": [5, {"name": "a", "topic": "t", "due": "2026-01-01", "after": "x"}]}]`,
			[]string{
				"milestone 1, name: Input should be a valid string",
				"milestone 1, due: Input should be a valid date",
				"milestone 1, action 1: Input should be a valid dictionary or instance of ActionSpec",
				"milestone 1, action 2, after: Input should be a valid list",
			}},
		"items checked when the list is not too long": {`[{"name": "M", "due": "2026-01-01", "actions": [{"name": "a", "topic": "t", "due": "2026-01-01", "after": ["", 5]}]}]`,
			[]string{"milestone 1, action 1, after 1: String should have at least 1 character",
				"milestone 1, action 1, after 2: Input should be a valid string"}},
		"a day out of range": {`[{"name": "M", "due": "2026-02-30", "actions": [{"name": "a", "topic": "t", "due": "2026-01-01"}]}]`,
			[]string{"milestone 1, due: Input should be a valid date or datetime, day value is outside expected range"}},
		"a time that is not midnight": {`[{"name": "M", "due": 1767571201, "actions": [{"name": "a", "topic": "t", "due": "2026-01-05T10:00:00"}]}]`,
			[]string{"milestone 1, due: Datetimes provided to dates should have zero time - e.g. be exact dates",
				"milestone 1, action 1, due: Datetimes provided to dates should have zero time - e.g. be exact dates"}},
		"an action list that is an object": {`[{"name": "M", "due": "2026-01-01", "actions": {}}]`,
			[]string{"milestone 1, actions: Input should be a valid list"}},
		"a name counted in characters, not bytes": {`[{"name": "` + strings.Repeat("é", 120) + `", "due": "2026-01-01", "actions": [{"name": "a", "topic": "t", "due": "2026-01-01"}]}]`, nil},
	}
	for name, c := range cases {
		specs, problems := Specs(json.RawMessage(c.in))
		if strings.Join(problems, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got  %q\n want %q", name, problems, c.want)
		}
		if len(c.want) > 0 && specs != nil {
			t.Errorf("%s: specs returned with problems", name)
		}
		if len(c.want) == 0 && len(specs) != 1 {
			t.Errorf("%s: specs %v", name, specs)
		}
	}
	// A shape problem is the whole answer: resolve and the structure check are not run on a broken structure.
	r := ValidateJSON(json.RawMessage(`[{"name": "M", "due": "2026-01-01", "actions": []}]`), ctx())
	if r.Valid || len(r.Problems) != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestDatesAsPydanticTakesThem(t *testing.T) {
	for in, want := range map[string]string{
		`"2026-01-05"`: "2026-01-05", `"2026-01-05T00:00:00"`: "2026-01-05", `"2026-01-05T00:00:00Z"`: "2026-01-05",
		`"2026-01-05T00:00:00.000+00:00"`: "2026-01-05", `1767571200`: "2026-01-05",
	} {
		specs, problems := Specs(json.RawMessage(`[{"name": "M", "due": ` + in + `, "actions": [{"name": "a", "topic": "t", "due": "2026-01-01"}]}]`))
		if len(problems) > 0 || specs[0].Due != want {
			t.Errorf("%s: %v %v", in, specs, problems)
		}
	}
	for _, in := range []string{`"2026-1-5"`, `"2026-02-30"`, `"2026-01-05T10:00:00"`, `"yesterday"`, `1767571201`, `1.5`, `true`, `""`} {
		if _, problems := Specs(json.RawMessage(`[{"name": "M", "due": ` + in + `, "actions": [{"name": "a", "topic": "t", "due": "2026-01-01"}]}]`)); len(problems) != 1 ||
			!strings.HasPrefix(problems[0], "milestone 1, due: ") {
			t.Errorf("%s accepted: %v", in, problems)
		}
	}
}

func TestPyRepr(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "'plain'", "it's": `"it's"`, `say "hi"`: `'say "hi"'`, `both ' and "`: `'both \' and "'`,
		"back\\slash": `'back\\slash'`, "line\nbreak": `'line\nbreak'`, "tab\t": `'tab\t'`, "\x01": `'\x01'`,
		"é ü 中": "'é ü 中'", "\u200b": `'\u200b'`, "\U000e0001": `'\U000e0001'`, "": "''", "\x7f": `'\x7f'`,
	} {
		if got := pyRepr(in); got != want {
			t.Errorf("pyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}

var ulid = regexp.MustCompile(`^(ms|ai)_[0-9A-HJKMNP-TV-Z]{26}$`)

func TestNewIDsAreWellFormedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		for _, p := range []string{"ms_", "ai_"} {
			id := newID(p)
			if !ulid.MatchString(id) || seen[id] {
				t.Fatalf("id %q (seen before: %v)", id, seen[id])
			}
			seen[id] = true
		}
	}
}

// TestLowerIsPythons: names compare as Python's str.lower compares them.
func TestLowerIsPythons(t *testing.T) {
	for in, want := range map[string]string{
		"İstanbul": "i̇stanbul", "ΟΔΟΣ": "οδος", "Σ": "σ", "ΑΣ.": "ας.", "ΑΣ'Β": "ασ'β", "ΑΣ́": "ας́",
		"ΑΣ1": "ας1", "1Σ": "1σ", "ABC": "abc", "": "",
	} {
		if got := lower(in); got != want {
			t.Errorf("lower(%q) = %q, want %q", in, got, want)
		}
	}
	m := func(names ...string) protocol.Milestone {
		ms := protocol.Milestone{ID: "ms_00000000000000000000000001", Name: "M", Due: "2026-01-10"}
		for i, n := range names {
			ms.Actions = append(ms.Actions, protocol.Action{ID: fmt.Sprintf("ai_%026d", i+1), Name: n,
				Topic: tpRounding, Due: "2026-01-10", After: []string{}})
		}
		return ms
	}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	topics := ctx().Topics
	if p := CheckStructure([]protocol.Milestone{m("İstanbul", "Istanbul")}, nil, topics, nil, day); len(p) != 0 {
		t.Errorf("İstanbul and Istanbul are two names in Python: %q", p)
	}
	if p := CheckStructure([]protocol.Milestone{m("ΟΔΟΣ", "οδος")}, nil, topics, nil, day); len(p) != 1 ||
		!strings.Contains(p[0], "2 actions are named") {
		t.Errorf("ΟΔΟΣ and οδος are one name in Python: %q", p)
	}
}

func TestDateBounds(t *testing.T) {
	for in, want := range map[string]string{
		`1e20`:                        "Input should be a valid date or datetime, dates after 9999 are not supported as unix timestamps",
		`253402300800000`:             "Input should be a valid date or datetime, dates after 9999 are not supported as unix timestamps",
		`-1e20`:                       "Input should be a valid date or datetime, dates before 0000 are not supported as unix timestamps",
		`-62135683200000`:             "Input should be a valid date in the format YYYY-MM-DD, year 0 is out of range",
		`"2026-01-05T00:00:00+24:00"`: "Input should be a valid date or datetime, timezone offset must be less than 24 hours",
		`"2026-01-05T00:00:00-24:00"`: "Input should be a valid date or datetime, timezone offset must be less than 24 hours",
		`"2026-01-05T00:00+2400"`:     "Input should be a valid date or datetime, timezone offset must be less than 24 hours",
		`"2026-01-05T24:00:00"`:       "Input should be a valid date or datetime, hour value is outside expected range of 0-23",
		`"2026-01-05T00:60"`:          "Input should be a valid date or datetime, minute value is outside expected range of 0-59",
	} {
		_, problems := Specs(json.RawMessage(`[{"name": "M", "due": ` + in + `, "actions": [{"name": "a", "topic": "t", "due": "2026-01-01"}]}]`))
		if len(problems) != 1 || problems[0] != "milestone 1, due: "+want {
			t.Errorf("%s: %q", in, problems)
		}
	}
	for in, want := range map[string]string{`253402214400000`: "9999-12-31", `-62135596800000`: "0001-01-01",
		`"2026-01-05T00:00:00+23:59"`: "2026-01-05"} {
		specs, problems := Specs(json.RawMessage(`[{"name": "M", "due": ` + in + `, "actions": [{"name": "a", "topic": "t", "due": "2026-01-01"}]}]`))
		if len(problems) > 0 || specs[0].Due != want {
			t.Errorf("%s: %v %q", in, specs, problems)
		}
	}
}
