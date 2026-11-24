// Package deliverables is the Go port of tf_models.deliverables: an action's status, what is late, and the
// check every Deliverables submission goes through. The server runs the same check and is the authority; the
// client runs it first, so an invalid structure is never sent. Problems are the Python sentences word for word
// (protocol/deliverables/vectors.json holds Python's answers, and the tests run every one).
package deliverables

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Member is one project member for Resolve: an owner is named by id or by this name. The order is kept: it is
// the order names are listed in a problem.
type Member struct{ ID, Name string }

// Rename is one new name for ApplyRenames, by milestone or action id. The order is kept for the problems.
type Rename struct{ ID, Name string }

const (
	prefixMember    = "mb_"
	prefixMilestone = "ms_"
	prefixAction    = "ai_"
)

// ------------------------------------------------------------------ status

// ActionStatus is an action's status, inferred from its results: results are its ACTION_RESULT and
// ACTION_REVOKE signals, in seq order.
func ActionStatus(a protocol.Action, results []protocol.StoredSignal) protocol.ActionStatus {
	if a.DoneBy != nil {
		return protocol.ActionStatusDone
	}
	for i := len(results) - 1; i >= 0; i-- {
		s := results[i].Signal
		if body, ok := s.Body.(protocol.ActionResultBody); ok && s.Kind == protocol.SignalKindActionResult {
			if body.Outcome == protocol.ActionOutcomeBlocked {
				return protocol.ActionStatusBlocked
			}
			break
		}
	}
	if len(results) > 0 {
		return protocol.ActionStatusInProgress
	}
	return protocol.ActionStatusNotStarted
}

func date(t time.Time) string { return t.Format(time.DateOnly) }

// IsLate: not done, and due before today (UTC dates).
func IsLate(a protocol.Action, status protocol.ActionStatus, today time.Time) bool {
	return status != protocol.ActionStatusDone && a.Due < date(today)
}

// MilestoneLate: due before today with actions not done.
func MilestoneLate(m protocol.Milestone, today time.Time) bool {
	return m.Due < date(today) && m.Done < m.Total
}

// Recount is the milestone with its progress counted from its actions.
func Recount(m protocol.Milestone) protocol.Milestone {
	m.Done, m.Total = 0, int64(len(m.Actions))
	for _, a := range m.Actions {
		if a.DoneBy != nil {
			m.Done++
		}
	}
	return m
}

// MilestonesComplete is how many milestones are complete (every action done).
func MilestonesComplete(ms []protocol.Milestone) int {
	n := 0
	for _, m := range ms {
		if m.Total > 0 && m.Done == m.Total {
			n++
		}
	}
	return n
}

// ------------------------------------------------------------------ a submission

var idPatterns = map[string]*regexp.Regexp{}

func is(prefix, ref string) bool {
	re, ok := idPatterns[prefix]
	if !ok {
		re = regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "[0-9A-HJKMNP-TV-Z]{26}$")
		idPatterns[prefix] = re
	}
	return re.MatchString(ref)
}

func init() {
	for _, p := range []string{prefixMember, prefixMilestone, prefixAction} {
		is(p, "") // compiled once, before any concurrent use
	}
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID is a fresh id with the prefix: a ULID (48 bits of milliseconds, 80 random bits), as Python's new_id.
var newID = func(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err) // crypto/rand never fails on the supported platforms
	}
	var out [26]byte
	// 128 bits as 26 base-32 digits, the first holding the top 3 bits
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 | uint64(b[4])<<24 |
		uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 | uint64(b[12])<<24 |
		uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return prefix + string(out[:])
}

// topicRef is a topic by id or by name (ignoring case).
func topicRef(ref string, topics []protocol.Topic) (string, bool) {
	for _, t := range topics {
		if ref == t.ID || lower(ref) == lower(t.Name) {
			return t.ID, true
		}
	}
	return "", false
}

// memberRef is (member id, problem): by id or by name (ignoring case); a name two members share needs the id.
func memberRef(ref string, members []Member) (string, string) {
	for _, m := range members {
		if m.ID == ref {
			return ref, ""
		}
	}
	if is(prefixMember, ref) {
		return ref, "" // an id outside the project is CheckStructure's to report
	}
	var hits, names []string
	for _, m := range uniqueMembers(members) {
		names = append(names, m.Name)
		if lower(m.Name) == lower(ref) {
			hits = append(hits, m.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], ""
	case 0:
		return "", fmt.Sprintf("no member %s in this project (members: %s)", pyRepr(ref), strings.Join(names, ", "))
	}
	return "", fmt.Sprintf("several members are called %s: use the id (%s)", pyRepr(ref), strings.Join(hits, ", "))
}

// uniqueMembers is members as a Python dict holds them: the first position of an id, its last name.
func uniqueMembers(members []Member) []Member {
	var out []Member
	at := map[string]int{}
	for _, m := range members {
		if i, ok := at[m.ID]; ok {
			out[i].Name = m.Name
			continue
		}
		at[m.ID] = len(out)
		out = append(out, m)
	}
	return out
}

type place struct {
	topic string
	owner *string
}

// Resolve turns a submission into records: fresh ms_/ai_ ids for new items; topics, owners and `after` names
// mapped to ids; done carried over for kept actions. It returns them and the problems found. An action whose
// topic or owner cannot be resolved is left out (with the dependencies on it), so CheckStructure can still be
// run on the rest without reporting problems that are not there.
func Resolve(spec []protocol.MilestoneSpec, old *protocol.Deliverables, topics []protocol.Topic,
	members []Member) ([]protocol.Milestone, []string) {
	problems := []string{}
	oldMilestones := map[string]bool{}
	oldActions := map[string]protocol.Action{}
	if old != nil {
		for _, m := range old.Milestones {
			oldMilestones[m.ID] = true
			for _, a := range m.Actions {
				oldActions[a.ID] = a
			}
		}
	}

	// Ids first, so `after` can name any action, earlier or later.
	ids := make([][]string, len(spec))
	byName := map[string]string{}
	byLower := map[string][]string{}
	known := map[string]bool{}
	for i, ms := range spec {
		for _, a := range ms.Actions {
			if a.ID != nil {
				if _, ok := oldActions[*a.ID]; !ok {
					problems = append(problems, fmt.Sprintf("no action %s in the current version (a new action has no id)", *a.ID))
				}
			}
			aid := ""
			if a.ID != nil {
				aid = *a.ID
			}
			if aid == "" {
				aid = newID(prefixAction)
			}
			if _, ok := byName[a.Name]; !ok {
				byName[a.Name] = aid
			}
			byLower[lower(a.Name)] = append(byLower[lower(a.Name)], aid)
			ids[i] = append(ids[i], aid)
			known[aid] = true
		}
	}

	// Topics and owners; what cannot be resolved is left out.
	where := map[string]place{}
	topicNames := make([]string, len(topics))
	for i, t := range topics {
		topicNames[i] = t.Name
	}
	for i, ms := range spec {
		for j, a := range ms.Actions {
			aid := ids[i][j]
			topic, topicOK := topicRef(a.Topic, topics)
			var owner *string
			ownerProblem := ""
			if a.Owner != nil {
				id, p := memberRef(*a.Owner, members)
				ownerProblem = p
				if p == "" {
					owner = &id
				}
			}
			if !topicOK {
				problems = append(problems, fmt.Sprintf("action %s: no topic %s in this project (topics: %s)",
					pyRepr(a.Name), pyRepr(a.Topic), strings.Join(topicNames, ", ")))
			}
			if ownerProblem != "" {
				problems = append(problems, fmt.Sprintf("action %s: %s", pyRepr(a.Name), ownerProblem))
			}
			if topicOK && ownerProblem == "" {
				where[aid] = place{topic, owner}
			}
		}
	}

	target := func(a protocol.ActionSpec, ref string) (string, bool) {
		if known[ref] {
			return ref, true
		}
		if id, ok := byName[ref]; ok {
			return id, true
		}
		if hits := byLower[lower(ref)]; len(hits) == 1 {
			return hits[0], true
		}
		if is(prefixAction, ref) {
			return ref, true // not in this version: CheckStructure says so
		}
		problems = append(problems, fmt.Sprintf("action %s depends on %s: no action has that name or id",
			pyRepr(a.Name), pyRepr(ref)))
		return "", false
	}

	out := []protocol.Milestone{}
	for i, ms := range spec {
		if ms.ID != nil && !oldMilestones[*ms.ID] {
			problems = append(problems, fmt.Sprintf("no milestone %s in the current version (a new milestone has no id)", *ms.ID))
		}
		actions := []protocol.Action{}
		for j, a := range ms.Actions {
			aid := ids[i][j]
			var after []string
			for _, ref := range a.After {
				if t, ok := target(a, ref); ok {
					after = append(after, t)
				}
			}
			w, ok := where[aid]
			if !ok {
				continue
			}
			kept := []string{}
			for _, t := range unique(after) {
				if _, resolved := where[t]; resolved || !known[t] {
					kept = append(kept, t)
				}
			}
			act := protocol.Action{ID: aid, Name: a.Name, Topic: w.topic, Due: a.Due, Owner: w.owner, After: kept}
			if k, ok := oldActions[aid]; ok {
				act.DoneBy, act.DoneAt, act.DoneSignal = k.DoneBy, k.DoneAt, k.DoneSignal
			}
			actions = append(actions, act)
		}
		if len(actions) > 0 {
			id := ""
			if ms.ID != nil {
				id = *ms.ID
			}
			if id == "" {
				id = newID(prefixMilestone)
			}
			out = append(out, Recount(protocol.Milestone{ID: id, Name: ms.Name, Due: ms.Due, Actions: actions}))
		}
	}
	return out, problems
}

// SpecOf is a stored milestone as a submission (by ids), e.g. to merge one edited milestone into the rest.
func SpecOf(m protocol.Milestone) protocol.MilestoneSpec {
	id := m.ID
	s := protocol.MilestoneSpec{ID: &id, Name: m.Name, Due: m.Due, Actions: []protocol.ActionSpec{}}
	for _, a := range m.Actions {
		aid := a.ID
		s.Actions = append(s.Actions, protocol.ActionSpec{ID: &aid, Name: a.Name, Topic: a.Topic, Due: a.Due,
			Owner: a.Owner, After: slices.Clone(a.After)})
	}
	return s
}

// MergeMilestone is the whole structure with one milestone (by id) replaced by spec and the others kept as they
// are; or nil and the problems. An action can neither come from nor go to another milestone this way.
func MergeMilestone(old protocol.Deliverables, spec protocol.MilestoneSpec) ([]protocol.MilestoneSpec, []string) {
	if spec.ID == nil {
		return []protocol.MilestoneSpec{}, []string{"the milestone's id is required (a new milestone needs a whole publish)"}
	}
	at := -1
	for i, m := range old.Milestones {
		if m.ID == *spec.ID {
			at = i
			break
		}
	}
	if at < 0 {
		return []protocol.MilestoneSpec{}, []string{fmt.Sprintf("no milestone %s in the current version", *spec.ID)}
	}
	elsewhere := map[string]string{}
	for i, m := range old.Milestones {
		if i == at {
			continue
		}
		for _, a := range m.Actions {
			elsewhere[a.ID] = m.Name
		}
	}
	taken := []string{}
	for _, a := range spec.Actions {
		if a.ID == nil {
			continue
		}
		if name, ok := elsewhere[*a.ID]; ok {
			taken = append(taken, fmt.Sprintf("action %s belongs to milestone %s: moving actions between milestones "+
				"needs a whole publish", pyRepr(a.Name), pyRepr(name)))
		}
	}
	if len(taken) > 0 {
		return []protocol.MilestoneSpec{}, taken
	}
	out := make([]protocol.MilestoneSpec, len(old.Milestones))
	for i, m := range old.Milestones {
		if i == at {
			out[i] = spec
			continue
		}
		out[i] = SpecOf(m)
	}
	return out, []string{}
}

// ApplyRenames is the whole structure with milestones and actions renamed; or nil and the problems (an id not in
// the current version, a name that is empty or too long). A Python dict keeps a repeated id at its first place,
// with its last name; so does this.
func ApplyRenames(old protocol.Deliverables, names []Rename) ([]protocol.MilestoneSpec, []string) {
	var order []string
	to := map[string]string{}
	for _, r := range names {
		if _, ok := to[r.ID]; !ok {
			order = append(order, r.ID)
		}
		to[r.ID] = r.Name
	}
	known := map[string]bool{}
	for _, m := range old.Milestones {
		known[m.ID] = true
		for _, a := range m.Actions {
			known[a.ID] = true
		}
	}
	problems := []string{}
	for _, k := range order {
		if !known[k] {
			problems = append(problems, fmt.Sprintf("no item %s in the current version", k))
		}
	}
	for _, k := range order {
		if n := utf8.RuneCountInString(to[k]); known[k] && (n < 1 || n > 120) {
			problems = append(problems, fmt.Sprintf("the new name for %s must be 1 to 120 characters", k))
		}
	}
	if len(problems) > 0 {
		return []protocol.MilestoneSpec{}, problems
	}
	out := []protocol.MilestoneSpec{}
	for _, m := range old.Milestones {
		s := SpecOf(m)
		if n, ok := to[m.ID]; ok {
			s.Name = n
		}
		for i, a := range s.Actions {
			if n, ok := to[*a.ID]; ok {
				s.Actions[i].Name = n
			}
		}
		out = append(out, s)
	}
	return out, []string{}
}

// ------------------------------------------------------------------ the check

// cycle is the names along one dependency cycle (first and last the same), or nil when there is none: a
// depth-first search with three colours (unseen, on the current path, finished).
func cycle(actions []protocol.Action) []string {
	deps := map[string][]string{}
	names := map[string]string{}
	for _, a := range actions {
		deps[a.ID] = a.After
		names[a.ID] = a.Name
	}
	onPath := map[string]bool{}
	finished := map[string]bool{}
	var path []string
	var visit func(n string) []string
	visit = func(n string) []string {
		onPath[n] = true
		path = append(path, n)
		for _, d := range deps[n] {
			if onPath[d] {
				at := slices.Index(path, d)
				return append(slices.Clone(path[at:]), d)
			}
			if _, ok := deps[d]; ok && !finished[d] {
				if found := visit(d); len(found) > 0 {
					return found
				}
			}
		}
		path = path[:len(path)-1]
		delete(onPath, n)
		finished[n] = true
		return nil
	}
	for _, a := range actions {
		if !finished[a.ID] {
			if found := visit(a.ID); len(found) > 0 {
				out := make([]string, len(found))
				for i, x := range found {
					out[i] = names[x]
				}
				return out
			}
		}
	}
	return nil
}

// CheckStructure lists every problem with a structure, each a short sentence an agent can act on; empty when it
// is valid. Whether a topic is open and an owner a project member is checked only for actions added, or whose
// topic or owner changed, since old. publishedOn is the day of the submission (UTC); no rule depends on it yet.
func CheckStructure(milestones []protocol.Milestone, old *protocol.Deliverables, topics []protocol.Topic,
	members map[string]bool, publishedOn time.Time) []string {
	_ = publishedOn
	problems := []string{}
	var actions []protocol.Action
	for _, m := range milestones {
		actions = append(actions, m.Actions...)
	}
	if len(milestones) > protocol.MaxMilestones {
		problems = append(problems, fmt.Sprintf("%d milestones: at most %d", len(milestones), protocol.MaxMilestones))
	}
	if len(actions) > protocol.MaxActions {
		problems = append(problems, fmt.Sprintf("%d actions: at most %d", len(actions), protocol.MaxActions))
	}
	msNames := make([]string, len(milestones))
	msIDs := make([]string, len(milestones))
	for i, m := range milestones {
		msNames[i], msIDs[i] = m.Name, m.ID
	}
	aNames := make([]string, len(actions))
	aIDs := make([]string, len(actions))
	for i, a := range actions {
		aNames[i], aIDs[i] = a.Name, a.ID
	}
	for _, g := range []struct {
		what  string
		names []string
	}{{"milestones", msNames}, {"actions", aNames}} {
		var order []string
		spellings := map[string][]string{} // unique ignoring case: an agent names an action without minding case
		for _, n := range g.names {
			l := lower(n)
			if _, ok := spellings[l]; !ok {
				order = append(order, l)
			}
			spellings[l] = append(spellings[l], n)
		}
		for _, l := range order {
			same := spellings[l]
			if len(same) < 2 {
				continue
			}
			quoted := []string{}
			for _, s := range unique(same) {
				quoted = append(quoted, pyRepr(s))
			}
			problems = append(problems, fmt.Sprintf("%d %s are named %s: %s names must be unique (ignoring case)",
				len(same), g.what, strings.Join(quoted, ", "), strings.TrimSuffix(g.what, "s")))
		}
	}
	for _, g := range []struct {
		what string
		ids  []string
	}{{"milestone", msIDs}, {"action", aIDs}} {
		count := map[string]int{}
		var order []string
		for _, id := range g.ids {
			if count[id] == 0 {
				order = append(order, id)
			}
			count[id]++
		}
		for _, id := range order {
			if count[id] > 1 {
				problems = append(problems, fmt.Sprintf("%s %s appears %d times", g.what, id, count[id]))
			}
		}
	}

	oldActions := map[string]protocol.Action{}
	if old != nil {
		for _, m := range old.Milestones {
			for _, a := range m.Actions {
				oldActions[a.ID] = a
			}
		}
	}
	byID := map[string]protocol.Action{}
	position := map[string]int{}
	for i, m := range milestones {
		for _, a := range m.Actions {
			byID[a.ID] = a
			position[a.ID] = i
		}
	}
	topicByID := map[string]protocol.Topic{}
	for _, t := range topics {
		topicByID[t.ID] = t
	}
	for i, m := range milestones {
		if len(m.Actions) == 0 {
			problems = append(problems, fmt.Sprintf("milestone %s has no actions", pyRepr(m.Name)))
		}
		if i > 0 {
			if prev := milestones[i-1]; m.Due <= prev.Due {
				problems = append(problems, fmt.Sprintf("milestone %s is due %s, not after the previous milestone %s (%s)",
					pyRepr(m.Name), m.Due, pyRepr(prev.Name), prev.Due))
			}
		}
		for _, a := range m.Actions {
			was, existed := oldActions[a.ID]
			if len(a.After) > protocol.MaxAfter {
				problems = append(problems, fmt.Sprintf("action %s has %d dependencies: at most %d", pyRepr(a.Name),
					len(a.After), protocol.MaxAfter))
			}
			if !existed || was.Topic != a.Topic {
				t, ok := topicByID[a.Topic]
				if !ok {
					problems = append(problems, fmt.Sprintf("action %s: no topic %s in this project", pyRepr(a.Name), a.Topic))
				}
				if ok && t.Closed {
					problems = append(problems, fmt.Sprintf("action %s is in topic %s, which is closed", pyRepr(a.Name), pyRepr(t.Name)))
				}
			}
			if a.Owner != nil && (!existed || !sameOwner(was.Owner, a.Owner)) && !members[*a.Owner] {
				problems = append(problems, fmt.Sprintf("action %s: its owner %s is not a member of this project",
					pyRepr(a.Name), *a.Owner))
			}
			if a.Due > m.Due {
				problems = append(problems, fmt.Sprintf("action %s is due %s, after its milestone %s (%s)",
					pyRepr(a.Name), a.Due, pyRepr(m.Name), m.Due))
			}
			for _, dep := range a.After {
				d, ok := byID[dep]
				if !ok {
					problems = append(problems, fmt.Sprintf("action %s depends on %s: no action with that id in this version",
						pyRepr(a.Name), dep))
					continue
				}
				if position[dep] > i {
					problems = append(problems, fmt.Sprintf("action %s depends on %s, which is in a later milestone",
						pyRepr(a.Name), pyRepr(d.Name)))
				}
				if a.Due < d.Due {
					problems = append(problems, fmt.Sprintf("action %s (due %s) is due before %s (%s), which it depends on",
						pyRepr(a.Name), a.Due, pyRepr(d.Name), d.Due))
				}
			}
		}
	}
	if c := cycle(actions); len(c) > 0 {
		problems = append(problems, "the dependencies form a cycle: "+strings.Join(c, " -> "))
	}
	return problems
}

func sameOwner(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// unique keeps the first of each value, in order (Python's dict.fromkeys).
func unique(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// lower is Python's str.lower: Unicode's full lower-case mapping, which differs from strings.ToLower in two
// places: İ (U+0130) becomes "i̇" (i and a combining dot), and Σ ending a word (a cased letter before it, none
// after, case-ignorable characters skipped) becomes ς.
func lower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r == 'İ':
			b.WriteString("i\u0307")
		case r == 'Σ' && finalSigma(rs, i):
			b.WriteRune('ς')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// finalSigma is Unicode's Final_Sigma condition at rs[i], as Python applies it.
func finalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && caseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !cased(rs[j]) {
		return false
	}
	j = i + 1
	for j < len(rs) && caseIgnorable(rs[j]) {
		j++
	}
	return j == len(rs) || !cased(rs[j])
}

func cased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// caseIgnorable: Mn, Me, Cf, Lm, Sk, and the word-break MidLetter, MidNumLet and Single_Quote characters.
func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '·', '\u0387', '\u05f4', '\u2018', '\u2019', '\u2024', '\u2027', '\ufe13', '\ufe52',
		'\ufe55', '\uff07', '\uff0e', '\uff1a', '\u055f', '\u00ad':
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

// ------------------------------------------------------------------ validate

// Context is what a structure is checked against: the current version (nil when none is published), the
// project's topics and its members (id and display name), all from the replica.
type Context struct {
	Old     *protocol.Deliverables
	Topics  []protocol.Topic
	Members []Member
}

// Result is the deliverables_validate answer: {"valid": …, "problems": […]}.
type Result struct {
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
}

func result(problems []string) Result {
	if problems == nil {
		problems = []string{}
	}
	return Result{Valid: len(problems) == 0, Problems: problems}
}

// Problems is the shared check of a submission, as the server runs it: Resolve, then CheckStructure on what it
// resolved (today's UTC date as the day of publishing). It returns the resolved milestones too.
func Problems(spec []protocol.MilestoneSpec, c Context) ([]protocol.Milestone, []string) {
	ms, problems := Resolve(spec, c.Old, c.Topics, c.Members)
	set := map[string]bool{}
	for _, m := range c.Members {
		set[m.ID] = true
	}
	return ms, append(problems, CheckStructure(ms, c.Old, c.Topics, set, time.Now().UTC())...)
}

// Validate checks a whole structure as a publish on the current version: its shape first (as Specs), then the
// shared check. Every problem, or valid.
func Validate(spec []protocol.MilestoneSpec, c Context) Result {
	if problems := shapeProblems(spec); len(problems) > 0 {
		return result(problems)
	}
	_, problems := Problems(spec, c)
	return result(problems)
}

// ValidateJSON is Validate on a structure as written (the tool's or the file's JSON).
func ValidateJSON(raw []byte, c Context) Result {
	spec, problems := Specs(raw)
	if len(problems) > 0 {
		return result(problems)
	}
	_, problems = Problems(spec, c)
	return result(problems)
}
