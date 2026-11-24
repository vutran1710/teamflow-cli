package deliverables

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Specs reads a structure as written (JSON) into submissions, or nil and every shape problem, each with where it
// is: "milestone 1, action 2, due: Field required". It is the Python client's milestone_specs (a pydantic
// TypeAdapter): the messages are pydantic's for the cases an agent meets (a missing or empty field, a bad date,
// an id of the wrong form, a list too long or empty, an unknown field). A date is kept as YYYY-MM-DD.
func Specs(raw []byte) ([]protocol.MilestoneSpec, []string) {
	s := &shaper{}
	var items []json.RawMessage
	if !json.Valid(raw) {
		return nil, []string{"the structure: Invalid JSON"}
	}
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, []string{"the structure: Input should be a valid list"}
	}
	out := make([]protocol.MilestoneSpec, len(items))
	for i, item := range items {
		out[i] = s.milestone(fmt.Sprintf("milestone %d", i+1), item)
	}
	if len(s.problems) > 0 {
		return nil, s.problems
	}
	return out, nil
}

// shapeProblems checks typed specs (built in Go, not read from JSON) against the same constraints.
func shapeProblems(spec []protocol.MilestoneSpec) []string {
	raw, err := json.Marshal(spec)
	if err != nil {
		return []string{"the structure: " + err.Error()}
	}
	_, problems := Specs(raw)
	return problems
}

type shaper struct{ problems []string }

func (s *shaper) add(where, msg string) { s.problems = append(s.problems, where+": "+msg) }

// object reads a JSON object's fields in their written order; ok is false when raw is not an object.
func object(raw json.RawMessage) (keys []string, vals map[string]json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	vals = map[string]json.RawMessage{}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		key := k.(string)
		if _, dup := vals[key]; !dup {
			keys = append(keys, key)
		}
		vals[key] = v
	}
	return keys, vals, true
}

var (
	milestoneFields = []string{"id", "name", "due", "actions"}
	actionFields    = []string{"id", "name", "topic", "due", "owner", "after"}
	milestoneID     = regexp.MustCompile(`^ms_[0-9A-HJKMNP-TV-Z]{26}$`)
	actionID        = regexp.MustCompile(`^ai_[0-9A-HJKMNP-TV-Z]{26}$`)
)

func (s *shaper) extras(where string, keys []string, known []string) {
	for _, k := range keys {
		if !contains(known, k) {
			s.add(where+", "+k, "Extra inputs are not permitted")
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func (s *shaper) milestone(where string, raw json.RawMessage) protocol.MilestoneSpec {
	var m protocol.MilestoneSpec
	keys, vals, ok := object(raw)
	if !ok {
		s.add(where, "Input should be a valid dictionary or instance of MilestoneSpec")
		return m
	}
	m.ID = s.id(where+", id", vals, milestoneID)
	m.Name = s.text(where+", name", vals["name"], true, 1, 120)
	m.Due = s.date(where+", due", vals["due"])
	m.Actions = []protocol.ActionSpec{}
	switch items, ok := list(vals["actions"]); {
	case vals["actions"] == nil:
		s.add(where+", actions", "Field required")
	case !ok:
		s.add(where+", actions", "Input should be a valid list")
	case len(items) == 0:
		s.add(where+", actions", "List should have at least 1 item after validation, not 0")
	default:
		for j, item := range items {
			m.Actions = append(m.Actions, s.action(fmt.Sprintf("%s, action %d", where, j+1), item))
		}
	}
	s.extras(where, keys, milestoneFields)
	return m
}

func (s *shaper) action(where string, raw json.RawMessage) protocol.ActionSpec {
	a := protocol.ActionSpec{After: []string{}}
	keys, vals, ok := object(raw)
	if !ok {
		s.add(where, "Input should be a valid dictionary or instance of ActionSpec")
		return a
	}
	a.ID = s.id(where+", id", vals, actionID)
	a.Name = s.text(where+", name", vals["name"], true, 1, 120)
	a.Topic = s.text(where+", topic", vals["topic"], true, 1, 60)
	a.Due = s.date(where+", due", vals["due"])
	if raw, ok := vals["owner"]; ok && !isNull(raw) {
		o := s.text(where+", owner", raw, true, 1, 80)
		a.Owner = &o
	}
	if raw, ok := vals["after"]; ok {
		items, isList := list(raw)
		switch {
		case !isList:
			s.add(where+", after", "Input should be a valid list")
		case len(items) > protocol.MaxAfter:
			s.add(where+", after", fmt.Sprintf("List should have at most %d items after validation, not %d",
				protocol.MaxAfter, len(items)))
		default:
			for j, item := range items {
				a.After = append(a.After, s.text(fmt.Sprintf("%s, after %d", where, j+1), item, true, 1, 120))
			}
		}
	}
	s.extras(where, keys, actionFields)
	return a
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func list(raw json.RawMessage) ([]json.RawMessage, bool) {
	if raw == nil || isNull(raw) {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	return items, true
}

func (s *shaper) id(where string, vals map[string]json.RawMessage, pattern *regexp.Regexp) *string {
	raw, ok := vals["id"]
	if !ok || isNull(raw) {
		return nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		s.add(where, "Input should be a valid string")
		return nil
	}
	if !pattern.MatchString(v) {
		s.add(where, fmt.Sprintf("String should match pattern '%s'", pattern))
		return nil
	}
	return &v
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// text is a required string field of lo to hi characters (counted as Python counts them: code points).
func (s *shaper) text(where string, raw json.RawMessage, required bool, lo, hi int) string {
	if raw == nil {
		if required {
			s.add(where, "Field required")
		}
		return ""
	}
	var v string
	if isNull(raw) || json.Unmarshal(raw, &v) != nil {
		s.add(where, "Input should be a valid string")
		return ""
	}
	switch n := utf8.RuneCountInString(v); {
	case n < lo:
		s.add(where, "String should have at least "+plural(lo, "character"))
	case n > hi:
		s.add(where, "String should have at most "+plural(hi, "character"))
	}
	return v
}

var (
	timePart = regexp.MustCompile(`^(\d{2}):(\d{2})(?::(\d{2})(?:[.,](\d{1,9}))?)?(Z|z|[+-]\d{2}(?::?\d{2})?)?$`)
	epoch    = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Unix times pydantic takes for a date: 0000-01-01 to 9999-12-31 23:59:59 (year 0 itself is refused).
const (
	maxUnix   = 253402300799
	minUnix   = -62167219200
	year1Unix = -62135596800
)

const (
	badDate  = "Input should be a valid date"
	zeroTime = "Datetimes provided to dates should have zero time - e.g. be exact dates"
	dateOr   = "Input should be a valid date or datetime, "
)

// date reads a date as pydantic does for the inputs an agent gives: YYYY-MM-DD; a datetime at midnight; a
// Unix time (seconds, or milliseconds past 2e10) on a day's start. Anything else is a problem.
func (s *shaper) date(where string, raw json.RawMessage) string {
	if raw == nil {
		s.add(where, "Field required")
		return ""
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		s.add(where, badDate)
		return ""
	}
	switch x := v.(type) {
	case json.Number:
		return s.timestamp(where, string(x))
	case string:
		if _, err := strconv.ParseFloat(x, 64); err == nil && strings.Trim(x, "0123456789.-+eE") == "" {
			return s.timestamp(where, x)
		}
		d, problem := dateString(x)
		if problem != "" {
			s.add(where, problem)
		}
		return d
	}
	s.add(where, badDate)
	return ""
}

func (s *shaper) timestamp(where, n string) string {
	f, err := strconv.ParseFloat(n, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		s.add(where, badDate)
		return ""
	}
	if math.Abs(f) > 2e10 {
		f /= 1000
	}
	switch {
	case f > maxUnix:
		s.add(where, dateOr+"dates after 9999 are not supported as unix timestamps")
		return ""
	case f < minUnix:
		s.add(where, dateOr+"dates before 0000 are not supported as unix timestamps")
		return ""
	case f != math.Trunc(f) || math.Mod(f, 86400) != 0:
		s.add(where, zeroTime)
		return ""
	case f < year1Unix:
		s.add(where, "Input should be a valid date in the format YYYY-MM-DD, year 0 is out of range")
		return ""
	}
	return epoch.AddDate(0, 0, int(f/86400)).Format(time.DateOnly)
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dateString is a date from a string, or the problem with it (pydantic's wording).
func dateString(x string) (string, string) {
	if len(x) < 10 {
		return "", dateOr + "input is too short"
	}
	switch {
	case !digits(x[0:4]):
		return "", dateOr + "invalid character in year"
	case x[4] != '-':
		return "", dateOr + "invalid date separator, expected `-`"
	case !digits(x[5:7]):
		return "", dateOr + "invalid character in month"
	case x[7] != '-':
		return "", dateOr + "invalid date separator, expected `-`"
	case !digits(x[8:10]):
		return "", dateOr + "invalid character in day"
	}
	y, _ := strconv.Atoi(x[0:4])
	mo, _ := strconv.Atoi(x[5:7])
	d, _ := strconv.Atoi(x[8:10])
	if y == 0 {
		return "", "Input should be a valid date in the format YYYY-MM-DD, year 0 is out of range"
	}
	if mo < 1 || mo > 12 {
		return "", dateOr + "month value is outside expected range of 1-12"
	}
	if d < 1 || d > time.Date(y, time.Month(mo)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return "", dateOr + "day value is outside expected range"
	}
	day := x[0:10]
	if len(x) == 10 {
		return day, ""
	}
	if !strings.ContainsRune("Tt_ ", rune(x[10])) {
		return "", dateOr + "invalid datetime separator, expected `T`, `t`, `_` or space"
	}
	m := timePart.FindStringSubmatch(x[11:])
	if m == nil {
		return "", dateOr + "input is too short"
	}
	hh, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	ss, _ := strconv.Atoi("0" + m[3])
	switch {
	case hh > 23:
		return "", dateOr + "hour value is outside expected range of 0-23"
	case mm > 59:
		return "", dateOr + "minute value is outside expected range of 0-59"
	case ss > 59:
		return "", dateOr + "second value is outside expected range of 0-59"
	}
	if tz := strings.ReplaceAll(m[5], ":", ""); len(tz) >= 3 {
		oh, _ := strconv.Atoi(tz[1:3])
		om := 0
		if len(tz) >= 5 {
			om, _ = strconv.Atoi(tz[3:5])
		}
		if oh*60+om >= 24*60 {
			return "", dateOr + "timezone offset must be less than 24 hours"
		}
	}
	if strings.Trim(m[1]+m[2]+m[3]+m[4], "0") != "" {
		return "", zeroTime
	}
	return day, ""
}

// pyRepr is Python's repr of a str: the problems quote names with it.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError:
			b.WriteString(`�`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
