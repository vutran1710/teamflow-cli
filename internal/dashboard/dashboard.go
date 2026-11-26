// Package dashboard is the project at a glance, as `teamflow project status` and /teamf status show it: the plan
// and Deliverables versions, progress, the milestones, the actions by status, what needs you, what is due soon,
// the approvals waiting for you and the open drifts. Plain ASCII, 64 columns: names are cut with "..." to fit their
// column, and columns are aligned with spaces.
//
// It is built from the replica's data and a clock (Build), so the same inputs give the same bytes. Dates are the
// clock's local dates: "in N days", "tomorrow", "today" and LATE count from the clock's day, and a time is shown
// in the clock's zone. An action's due date is its own (the Deliverables model gives every action one); its
// milestone's due date stands in only for an action that has none.
package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Width is the dashboard's width: its rule, and where the header's right side ends.
const Width = 64

// SoonDays: an action due within this many days (or late) is due soon.
const SoonDays = 7

// UrgentDays: an action of yours due within this many days (or late) is marked "!".
const UrgentDays = 2

// The empty states, one line each.
const (
	NoDeliverables = "No Deliverables yet: ask your agent to publish them from the plan."
	NoMilestones   = "No milestones yet: ask your agent to add them from the plan."
	Archived       = "The Deliverables are archived: nothing changes in them until the initiator restores them."
	NothingNeeded  = "Nothing needs you right now."
	NothingSoon    = "Nothing is due in the next 7 days."
)

// Input is what the dashboard is built from: one project as the replica holds it after a sync.
type Input struct {
	ProjectID string
	Project   string            // the project's name
	Org       string            // its org's name ("": unknown)
	You       string            // the signed-in member's id on the project ("": unknown)
	Names     map[string]string // member id -> name
	Plan      int64             // the plan's version (0: none yet)
	// The Deliverables in force, each action's done state folded in from its results (nil: none yet), and each
	// action's status (an action not listed: from its done state).
	Deliverables *protocol.Deliverables
	Statuses     map[string]protocol.ActionStatus
	Approvals    int    // approval requests waiting for you
	Drifts       int    // open drifts
	Session      string // the topics this session follows when it works on this project ("": none)
	Offline      bool   // the sync could not reach the server: this is the replica as of SyncedAt
	SyncedAt     time.Time
	AutoUpdate   bool   // the background sync updates teamflow on its own
	UpdatedTo    string // the release the background sync just updated teamflow to, told once ("": none)
}

// Milestone states.
const (
	StateDone     = "done"
	StateLate     = "late"
	StateCurrent  = "current"  // the first milestone not done and not late, by due date
	StateUpcoming = "upcoming" // any other
)

// Milestone is one row of the milestones table.
type Milestone struct {
	ID, Name, Due string
	Days          *int // days from today to its due date (nil: the date can't be read)
	Done, Total   int
	State         string
}

// Item is one action in Needs you or Due soon.
type Item struct {
	ID, Name, Due, Milestone string
	Days                     *int
	Status                   protocol.ActionStatus
	Owner                    string // "you", a member's name, or "unowned"
	Yours, Late, Urgent      bool
}

// Counts are actions by status: open is every one not DONE and not BLOCKED.
type Counts struct{ Total, Done, Open, Blocked int }

// Board is the dashboard, built.
type Board struct {
	ProjectID, Project, Org, You string
	Today                        string // the clock's local date
	Plan                         int64
	HasDeliverables, Archived    bool
	Version                      int64
	UpdatedAt                    string // when the Deliverables were published, local (YYYY-MM-DD HH:MM; "": unknown)
	UpdatedBy                    string
	MilestonesDone               int
	Milestones                   []Milestone
	Everyone, Yours              Counts
	NeedsYou, DueSoon            []Item
	Approvals, Drifts            int
	Session                      string
	Offline                      bool
	SyncedAt                     string // local (YYYY-MM-DD HH:MM; "": never)
	AutoUpdate                   bool
	UpdatedTo                    string
	knowsYou                     bool
	updatedAt, syncedAt          time.Time // local; zero: unknown
}

// Build makes the board of in as of now (its zone is the person's).
func Build(in Input, now time.Time) Board {
	today := civil(now)
	b := Board{ProjectID: in.ProjectID, Project: clean(in.Project), Org: clean(in.Org), Today: today.Format(time.DateOnly),
		Plan: in.Plan, Approvals: in.Approvals, Drifts: in.Drifts, Session: clean(in.Session), Offline: in.Offline,
		AutoUpdate: in.AutoUpdate, UpdatedTo: clean(in.UpdatedTo), knowsYou: in.You != ""}
	if in.You != "" {
		b.You = nameOf(in.Names, in.You)
	}
	if !in.SyncedAt.IsZero() {
		b.syncedAt = in.SyncedAt.In(now.Location())
		b.SyncedAt = b.syncedAt.Format("2006-01-02 15:04")
	}
	d := in.Deliverables
	if d == nil {
		return b
	}
	b.HasDeliverables, b.Archived, b.Version = true, d.Archived, d.Version
	if at, err := time.Parse(time.RFC3339Nano, d.PublishedAt); err == nil {
		b.updatedAt = at.In(now.Location())
		b.UpdatedAt = b.updatedAt.Format("2006-01-02 15:04")
	}
	if d.PublishedBy != nil && *d.PublishedBy != "" {
		b.UpdatedBy = nameOf(in.Names, *d.PublishedBy)
	}
	if d.Archived { // frozen: nothing in them is acted on until they are restored
		return b
	}
	status := func(a protocol.Action) protocol.ActionStatus {
		if s, ok := in.Statuses[a.ID]; ok {
			return s
		}
		if a.DoneBy != nil {
			return protocol.ActionStatusDone
		}
		return protocol.ActionStatusNotStarted
	}
	ms := append([]protocol.Milestone(nil), d.Milestones...)
	sort.SliceStable(ms, func(i, j int) bool { return before(ms[i].Due, ms[i].Name, ms[i].ID, ms[j].Due, ms[j].Name, ms[j].ID) })
	current := false
	for _, m := range ms {
		row := Milestone{ID: m.ID, Name: clean(m.Name), Due: m.Due, Days: daysTo(m.Due, today), Total: len(m.Actions)}
		for _, a := range m.Actions {
			st := status(a)
			due := a.Due
			if due == "" {
				due = m.Due
			}
			days := daysTo(due, today)
			late := st != protocol.ActionStatusDone && days != nil && *days < 0
			mine := in.You != "" && a.Owner != nil && *a.Owner == in.You
			count(&b.Everyone, st)
			if mine {
				count(&b.Yours, st)
			}
			if st == protocol.ActionStatusDone {
				row.Done++
				continue
			}
			it := Item{ID: a.ID, Name: clean(a.Name), Due: due, Milestone: clean(m.Name), Days: days, Status: st,
				Owner: ownerOf(a.Owner, in.You, in.Names), Yours: mine, Late: late}
			it.Urgent = late || days != nil && *days <= UrgentDays
			if mine {
				b.NeedsYou = append(b.NeedsYou, it)
			}
			if late || days != nil && *days >= 0 && *days <= SoonDays {
				b.DueSoon = append(b.DueSoon, it)
			}
		}
		done := row.Total > 0 && row.Done == row.Total
		late := !done && row.Days != nil && *row.Days < 0
		switch {
		case done:
			row.State = StateDone
			b.MilestonesDone++
		case late:
			row.State = StateLate
		case !current:
			row.State, current = StateCurrent, true
		default:
			row.State = StateUpcoming
		}
		b.Milestones = append(b.Milestones, row)
	}
	sort.SliceStable(b.NeedsYou, func(i, j int) bool { return itemBefore(b.NeedsYou[i], b.NeedsYou[j]) })
	sort.SliceStable(b.DueSoon, func(i, j int) bool {
		x, y := b.DueSoon[i], b.DueSoon[j]
		if x.Yours != y.Yours {
			return x.Yours
		}
		return itemBefore(x, y)
	})
	return b
}

func count(c *Counts, s protocol.ActionStatus) {
	c.Total++
	switch s {
	case protocol.ActionStatusDone:
		c.Done++
	case protocol.ActionStatusBlocked:
		c.Blocked++
	default:
		c.Open++
	}
}

// before orders by due date, then name, then id.
func before(due1, name1, id1, due2, name2, id2 string) bool {
	switch {
	case due1 != due2:
		return due1 < due2
	case name1 != name2:
		return name1 < name2
	}
	return id1 < id2
}

func itemBefore(x, y Item) bool { return before(x.Due, x.Name, x.ID, y.Due, y.Name, y.ID) }

// civil is t's local date, as midnight UTC (days between two of them are whole).
func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// daysTo is the days from today to a due date (YYYY-MM-DD); nil when it can't be read.
func daysTo(due string, today time.Time) *int {
	t, err := time.Parse(time.DateOnly, due)
	if err != nil {
		return nil
	}
	n := int(t.Sub(today).Hours() / 24)
	return &n
}

func nameOf(names map[string]string, id string) string {
	if n, ok := names[id]; ok && n != "" {
		return clean(n)
	}
	return id
}

func ownerOf(owner *string, you string, names map[string]string) string {
	switch {
	case owner == nil || *owner == "":
		return "unowned"
	case you != "" && *owner == you:
		return "you"
	}
	return nameOf(names, *owner)
}

// clean makes a teammate's text one line of plain text: control characters become spaces.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return ' '
		}
		return r
	}, s))
}

// fit cuts s to n characters, ending it with "..." when cut.
func fit(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 3 {
		return string(r[:max(n, 0)])
	}
	return string(r[:n-3]) + "..."
}

// pad fills s with spaces to n characters.
func pad(s string, n int) string {
	if k := utf8.RuneCountInString(s); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}

// relative is how far a due date is: "today", "tomorrow", "in N days", or "LATE".
func relative(days *int) string {
	switch {
	case days == nil:
		return ""
	case *days < 0:
		return "LATE"
	case *days == 0:
		return "today"
	case *days == 1:
		return "tomorrow"
	}
	return fmt.Sprintf("in %d days", *days)
}

// ------------------------------------------------------------------ as text

// Text is the dashboard as the person reads it (ending with a newline).
func (b Board) Text() string {
	var w strings.Builder
	line := func(s string) { w.WriteString(strings.TrimRight(s, " ") + "\n") }
	if b.Offline {
		as := "never synced"
		if b.SyncedAt != "" {
			as = "as of " + b.SyncedAt
		}
		line("(offline: " + as + ")")
	}
	line(b.header())
	line(strings.Repeat("=", Width))
	line(b.versions())
	switch {
	case !b.HasDeliverables:
		line("")
		line(NoDeliverables)
	case b.Archived:
		line("")
		line(Archived)
	case len(b.Milestones) == 0:
		line("")
		line(NoMilestones)
	default:
		b.tables(line)
	}
	line("")
	line(fmt.Sprintf("Waiting for your approval: %d    Open drifts: %d", b.Approvals, b.Drifts))
	if b.Session != "" {
		line("This session: " + b.Session)
	}
	if !b.AutoUpdate {
		line("auto-update: off")
	}
	if b.UpdatedTo != "" {
		line("teamflow was updated to " + b.UpdatedTo + ".")
	}
	return w.String()
}

// header is the project (its org) on the left and you on the right, Width wide.
func (b Board) header() string {
	left := b.Project
	if b.Org != "" {
		left += " (" + b.Org + ")"
	}
	if b.You == "" {
		return fit(left, Width)
	}
	right := "you: " + fit(b.You, 24)
	room := Width - utf8.RuneCountInString(right) - 2
	left = fit(left, room)
	return pad(left, Width-utf8.RuneCountInString(right)) + right
}

func (b Board) versions() string {
	parts := []string{"No plan yet"}
	if b.Plan > 0 {
		parts[0] = fmt.Sprintf("Plan v%d", b.Plan)
	}
	if b.HasDeliverables {
		v := fmt.Sprintf("Deliverables v%d", b.Version)
		if b.Archived {
			v += " (archived)"
		}
		parts = append(parts, v)
		if b.UpdatedAt != "" {
			by := ""
			if b.UpdatedBy != "" {
				by = " by " + fit(b.UpdatedBy, 20)
			}
			parts = append(parts, "updated "+b.UpdatedAt+by)
		}
	}
	return fit(strings.Join(parts, " | "), Width)
}

func (b Board) tables(line func(string)) {
	total := len(b.Milestones)
	cells := 20 * b.MilestonesDone / total
	line(fmt.Sprintf("Progress  [%s%s] %3d%%   %d of %d milestones done", strings.Repeat("#", cells),
		strings.Repeat("-", 20-cells), 100*b.MilestonesDone/total, b.MilestonesDone, total))
	line("")
	line(pad("Milestones", 31) + pad("due", 13) + pad("actions", 10) + "state")
	for _, m := range b.Milestones {
		mark, state := "[ ]", ""
		switch m.State {
		case StateDone:
			mark, state = "[x]", "done"
		case StateLate:
			mark, state = "[!]", "LATE"
		case StateCurrent:
			mark = "[>]"
			if rel := relative(m.Days); rel != "" {
				state = "due " + rel
			}
		}
		line("  " + mark + " " + pad(fit(m.Name, 23), 25) + pad(fit(m.Due, 11), 13) +
			pad(fmt.Sprintf("%d/%d", m.Done, m.Total), 10) + state)
	}
	line("")
	line(fmt.Sprintf("%-15s%5s%7s%7s%10s", "Actions", "total", "done", "open", "blocked"))
	row := func(who string, c Counts) {
		line(fmt.Sprintf("%-15s%5d%7d%7d%10d", "  "+who, c.Total, c.Done, c.Open, c.Blocked))
	}
	row("everyone", b.Everyone)
	if b.knowsYou {
		row("you", b.Yours)
	}
	line("")
	if len(b.NeedsYou) == 0 {
		line(NothingNeeded)
	} else {
		line(fmt.Sprintf("Needs you (%d)", len(b.NeedsYou)))
		for _, it := range b.NeedsYou {
			s := "  - " + pad(fit(it.Name, 28), 30) + "due " + fit(it.Due, 11)
			if rel := relative(it.Days); rel != "" {
				s += "  (" + rel + ")"
			}
			if it.Urgent {
				s += "  !"
			}
			line(s)
		}
	}
	line("")
	if len(b.DueSoon) == 0 {
		line(NothingSoon)
		return
	}
	line("Due soon (next 7 days, yours first)")
	for _, it := range b.DueSoon {
		s := "  - " + pad(fit(it.Name, 20), 22) + pad(fit(it.Owner, 7), 8) + fit(it.Due, 11)
		if it.Late {
			s += "  LATE"
		}
		line(s)
	}
}

// ------------------------------------------------------------------ as JSON

// JSON is the same data, structured.
func (b Board) JSON() ojson.Obj {
	var you any
	if b.You != "" {
		you = b.You
	}
	var org any
	if b.Org != "" {
		org = b.Org
	}
	out := ojson.Obj{
		{Key: "project", Value: ojson.Obj{{Key: "id", Value: b.ProjectID}, {Key: "name", Value: b.Project}, {Key: "org", Value: org}}},
		{Key: "you", Value: you},
		{Key: "today", Value: b.Today},
		{Key: "plan_version", Value: orNil(b.Plan > 0, b.Plan)},
	}
	var deliverables any
	if b.HasDeliverables {
		deliverables = ojson.Obj{{Key: "version", Value: b.Version}, {Key: "archived", Value: b.Archived},
			{Key: "updated_at", Value: stamp(b.updatedAt)},
			{Key: "updated_by", Value: orNil(b.UpdatedBy != "", b.UpdatedBy)}}
	}
	out = append(out, ojson.KV{Key: "deliverables", Value: deliverables})
	milestones := []ojson.Obj{}
	var progress any
	if len(b.Milestones) > 0 {
		progress = ojson.Obj{{Key: "done", Value: b.MilestonesDone}, {Key: "total", Value: len(b.Milestones)},
			{Key: "percent", Value: 100 * b.MilestonesDone / len(b.Milestones)}}
		for _, m := range b.Milestones {
			milestones = append(milestones, ojson.Obj{{Key: "id", Value: m.ID}, {Key: "name", Value: m.Name},
				{Key: "due", Value: m.Due}, {Key: "days", Value: days(m.Days)}, {Key: "done", Value: m.Done},
				{Key: "total", Value: m.Total}, {Key: "state", Value: m.State}})
		}
	}
	counts := ojson.Obj{{Key: "everyone", Value: countsObj(b.Everyone)}}
	if b.knowsYou {
		counts = append(counts, ojson.KV{Key: "you", Value: countsObj(b.Yours)})
	}
	needs, soon := []ojson.Obj{}, []ojson.Obj{}
	for _, it := range b.NeedsYou {
		needs = append(needs, itemObj(it, false))
	}
	for _, it := range b.DueSoon {
		soon = append(soon, itemObj(it, true))
	}
	return append(out,
		ojson.KV{Key: "progress", Value: progress},
		ojson.KV{Key: "milestones", Value: milestones},
		ojson.KV{Key: "counts", Value: counts},
		ojson.KV{Key: "needs_you", Value: needs},
		ojson.KV{Key: "due_soon", Value: soon},
		ojson.KV{Key: "approvals", Value: b.Approvals},
		ojson.KV{Key: "drifts", Value: b.Drifts},
		ojson.KV{Key: "session", Value: orNil(b.Session != "", b.Session)},
		ojson.KV{Key: "offline", Value: b.Offline},
		ojson.KV{Key: "synced_at", Value: stamp(b.syncedAt)},
		ojson.KV{Key: "auto_update", Value: b.AutoUpdate},
		ojson.KV{Key: "updated_to", Value: orNil(b.UpdatedTo != "", b.UpdatedTo)},
		ojson.KV{Key: "text", Value: b.Text()},
	)
}

func countsObj(c Counts) ojson.Obj {
	return ojson.Obj{{Key: "total", Value: c.Total}, {Key: "done", Value: c.Done}, {Key: "open", Value: c.Open},
		{Key: "blocked", Value: c.Blocked}}
}

func itemObj(it Item, owner bool) ojson.Obj {
	o := ojson.Obj{{Key: "id", Value: it.ID}, {Key: "name", Value: it.Name}, {Key: "milestone", Value: it.Milestone},
		{Key: "due", Value: it.Due}, {Key: "days", Value: days(it.Days)}, {Key: "status", Value: string(it.Status)}}
	if owner {
		o = append(o, ojson.KV{Key: "owner", Value: it.Owner}, ojson.KV{Key: "yours", Value: it.Yours})
	}
	return append(o, ojson.KV{Key: "late", Value: it.Late}, ojson.KV{Key: "urgent", Value: it.Urgent})
}

func days(d *int) any {
	if d == nil {
		return nil
	}
	return *d
}

func orNil(ok bool, v any) any {
	if !ok {
		return nil
	}
	return v
}

// stamp is a time in the person's zone, RFC 3339 (nil: unknown).
func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}
