package dashboard

// Golden tests of the dashboard: testdata/<case>.txt is the text byte for byte, testdata/<case>.json the --json
// shape. `go test ./internal/dashboard -update` rewrites them (read the diff before committing it); example.txt was
// written by hand from the approved layout and is never rewritten.
//
// Sad paths covered: no Deliverables, archived ones, no milestones, nothing needing you, nothing due soon, no plan,
// long names (cut with "..."), LATE / today / tomorrow / the "!" and the 7-day edges, a clock set back, an action
// with no owner, offline (with a last sync, and never synced), the member unknown, a due date that can't be read,
// an action with no due date of its own, control characters in a teammate's name, and the same inputs in another
// order giving the same bytes.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata (never example.txt)")

const (
	vu  = "mb_00000000000000000000000001"
	lan = "mb_00000000000000000000000002"
	huy = "mb_00000000000000000000000003"
)

// ict is the person's zone in these tests (UTC+7): times are shown in it, dates are its dates.
var ict = time.FixedZone("ICT", 7*3600)

// now is the tests' clock: 2026-11-27 10:00 in the person's zone.
var now = time.Date(2026, 11, 27, 10, 0, 0, 0, ict)

func ptr(s string) *string { return &s }

func action(id, name, due string, owner string) protocol.Action {
	a := protocol.Action{ID: "ai_" + id, Name: name, Topic: "tp_00000000000000000000000001", Due: due}
	if owner != "" {
		a.Owner = ptr(owner)
	}
	return a
}

// example is the approved example's project, made consistent with the rules (a milestone due before the previous
// one can't be published, a LATE milestone is past its due date, and the counts add up): see the report.
func example() Input {
	ms := []protocol.Milestone{
		{ID: "ms_1", Name: "Store listing", Due: "2026-11-25", Actions: []protocol.Action{
			action("10", "Submit store listing", "2026-11-25", ""),
		}},
		{ID: "ms_2", Name: "Rounding v1", Due: "2026-12-10", Actions: []protocol.Action{
			action("01", "Round prices", "2026-11-20", vu),
			action("02", "Rounding tests", "2026-12-10", huy),
		}},
		{ID: "ms_3", Name: "Release iHertz", Due: "2026-12-20", Actions: []protocol.Action{
			action("03", "Build release", "2026-12-15", vu),
			action("04", "Review round_price", "2026-12-01", vu),
			action("05", "Write release notes", "2026-11-28", vu),
			action("06", "Store screenshots", "2026-11-30", huy),
		}},
		{ID: "ms_4", Name: "Onboard first users", Due: "2027-01-15", Actions: []protocol.Action{
			action("07", "Invite beta users", "2027-01-05", lan),
			action("08", "Collect feedback", "2027-01-15", huy),
			action("09", "Fix onboarding issues", "2027-01-15", lan),
		}},
	}
	return Input{
		ProjectID: "pj_00000000000000000000000001", Project: "iHertz", Org: "1FlowCorp", You: vu,
		Names: map[string]string{vu: "Vu Tran", lan: "Lan", huy: "Huy"},
		Plan:  3,
		Deliverables: &protocol.Deliverables{Project: "pj_00000000000000000000000001", Version: 2,
			PublishedBy: ptr(lan), PublishedAt: "2026-11-26T08:28:00Z", Milestones: ms},
		Statuses: map[string]protocol.ActionStatus{
			"ai_01": protocol.ActionStatusDone, "ai_02": protocol.ActionStatusDone, "ai_03": protocol.ActionStatusDone,
			"ai_04": protocol.ActionStatusInProgress, "ai_07": protocol.ActionStatusBlocked,
		},
		Approvals: 1, Drifts: 2, Session: "topics Development, Plan (built-in, always followed)", AutoUpdate: true,
	}
}

// golden compares got with testdata/name (rewriting it with -update, except the hand-written example). Every
// line of a text golden fits Width.
func golden(t *testing.T, name, got string) {
	t.Helper()
	if strings.HasSuffix(name, ".txt") {
		fits(t, got)
	}
	path := filepath.Join("testdata", name)
	if *update && name != "example.txt" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs\n--- got\n%s--- want\n%s", name, got, want)
	}
}

// fits fails when a line of text is wider than Width.
func fits(t *testing.T, text string) {
	t.Helper()
	for i, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if n := width(l); n > Width {
			t.Errorf("line %d is %d wide (more than %d): %q", i+1, n, Width, l)
		}
	}
}

func goldenJSON(t *testing.T, name string, b Board) {
	t.Helper()
	text, err := ojson.Indent(b.JSON())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, name, text+"\n")
}

// ------------------------------------------------------------------ the full example

// The full example, byte for byte (a file written by hand from the approved layout), and its --json shape.
func TestTheFullExample(t *testing.T) {
	b := Build(example(), now)
	golden(t, "example.txt", b.Text())
	goldenJSON(t, "example.json", b)
	for i, l := range strings.Split(strings.TrimSuffix(b.Text(), "\n"), "\n") {
		for _, r := range l {
			if r > 0x7e || r < 0x20 {
				t.Fatalf("line %d is not plain ASCII: %q", i+1, l)
			}
		}
	}
	if first := strings.SplitN(b.Text(), "\n", 2)[0]; len(first) != Width {
		t.Fatalf("the header is %d wide, not %d: %q", len(first), Width, first)
	}
}

// The --json shape: every field the brief names, each of the right kind, and the text the person reads.
func TestJSONShape(t *testing.T) {
	raw, err := ojson.Marshal(Build(example(), now).JSON())
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"project", "you", "today", "plan_version", "deliverables", "progress", "milestones",
		"counts", "needs_you", "due_soon", "approvals", "drifts", "session", "offline", "synced_at", "auto_update",
		"updated_to", "text"} {
		if _, ok := o[k]; !ok {
			t.Errorf("no %q in %s", k, raw)
		}
	}
	if ms := o["milestones"].([]any); len(ms) != 4 || ms[0].(map[string]any)["state"] != "late" ||
		ms[2].(map[string]any)["state"] != "current" || ms[2].(map[string]any)["days"] != float64(23) {
		t.Fatalf("milestones: %v", o["milestones"])
	}
	counts := o["counts"].(map[string]any)
	if counts["everyone"].(map[string]any)["open"] != float64(6) || counts["you"].(map[string]any)["done"] != float64(2) {
		t.Fatalf("counts: %v", counts)
	}
	if n := o["needs_you"].([]any); len(n) != 2 || n[0].(map[string]any)["urgent"] != true {
		t.Fatalf("needs_you: %v", n)
	}
	soon := o["due_soon"].([]any)
	if len(soon) != 4 || soon[2].(map[string]any)["owner"] != "unowned" || soon[2].(map[string]any)["late"] != true {
		t.Fatalf("due_soon: %v", soon)
	}
	if o["approvals"] != float64(1) || o["drifts"] != float64(2) || o["text"] != Build(example(), now).Text() {
		t.Fatalf("approvals, drifts, text: %v %v", o["approvals"], o["drifts"])
	}
}

// ------------------------------------------------------------------ empty states

func TestEmptyStates(t *testing.T) {
	t.Run("no Deliverables yet", func(t *testing.T) {
		in := example()
		in.Deliverables = nil
		b := Build(in, now)
		golden(t, "no-deliverables.txt", b.Text())
		goldenJSON(t, "no-deliverables.json", b)
		if strings.Contains(b.Text(), "Milestones") || strings.Contains(b.Text(), "Actions") {
			t.Fatal("tables shown with no Deliverables")
		}
	})
	t.Run("archived", func(t *testing.T) {
		in := example()
		in.Deliverables.Archived = true
		b := Build(in, now)
		golden(t, "archived.txt", b.Text())
		if strings.Contains(b.Text(), "Needs you") || len(b.NeedsYou) != 0 || b.Everyone.Total != 0 {
			t.Fatal("an archived Deliverables' actions shown")
		}
	})
	t.Run("no milestones", func(t *testing.T) {
		in := example()
		in.Deliverables.Milestones = nil
		golden(t, "no-milestones.txt", Build(in, now).Text())
	})
	t.Run("nothing needs you, nothing due soon, no plan", func(t *testing.T) {
		in := example()
		in.Plan = 0
		for i := range in.Deliverables.Milestones {
			for j := range in.Deliverables.Milestones[i].Actions {
				a := &in.Deliverables.Milestones[i].Actions[j]
				if a.Owner != nil && *a.Owner == vu || a.Due < "2026-12-05" {
					in.Statuses[a.ID] = protocol.ActionStatusDone
				}
			}
		}
		b := Build(in, now)
		golden(t, "nothing-needs-you.txt", b.Text())
		for _, want := range []string{"\n" + NothingNeeded + "\n", "\n" + NothingSoon + "\n", "No plan yet | Deliverables v2"} {
			if !strings.Contains(b.Text(), want) {
				t.Fatalf("no %q in\n%s", want, b.Text())
			}
		}
	})
}

// ------------------------------------------------------------------ names, dates, owners

// Long names are cut with "..." to fit their column, and the columns stay aligned.
func TestLongNamesTruncated(t *testing.T) {
	in := example()
	in.Project = "The project with a name far too long to fit on one line beside you"
	in.Org = "An Org With A Long Name Too"
	in.Names[vu] = "Vu Tran Nguyen Minh Hoang the Third"
	in.Names[huy] = "Huy Nguyen"
	in.Names[lan] = "Lan the publisher with a very long name indeed"
	d := in.Deliverables
	d.Milestones[2].Name = "Release iHertz to the whole wide world"
	d.Milestones[2].Actions[2].Name = "Write the release notes for every store and every language"
	d.Milestones[2].Actions[3].Name = "Store screenshots in every size"
	b := Build(in, now)
	text := b.Text()
	golden(t, "long-names.txt", text)
	for _, want := range []string{
		"The project with a name far to...  you: Vu Tran Nguyen Minh H...\n",
		"  [>] Release iHertz t...  2026-12-20   1/4       due in 23 days\n",
		"  - Write the release notes f...  due 2026-11-28  (tomorrow)  !\n",
		"  - Write the release...  you     2026-11-28\n",
		"  - Store screenshots...  Huy ... 2026-11-30\n",
		"| updated 2026-11-26 15:28 by Lan t...\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
}

// Lines that would pass 64 columns: a long current-milestone state takes room from the name column (every row
// alike, so the columns stay aligned), then is cut; the session's topics wrap onto indented lines (a topic name
// too long for a line is cut); the progress line and Needs you's dates give way too. Every line fits 64.
func TestEveryLineFits64(t *testing.T) {
	in := example()
	in.Session = "topics Development, Design, Marketing and launch, Customer support, Infrastructure, " +
		"A-topic-name-with-no-space-in-it-that-is-far-too-long-for-any-line-of-the-dashboard, Plan (built-in, always followed)"
	d := in.Deliverables
	d.Milestones[2].Name = "Release iHertz to the whole wide world"
	d.Milestones[2].Due = "2026-12-20"
	back := time.Date(1999, 1, 1, 9, 0, 0, 0, ict) // a clock set back by decades: "due in 10215 days"
	b := Build(in, back)
	golden(t, "overflow.txt", b.Text())
	text := b.Text()
	for _, want := range []string{
		"Milestones              due          actions   state\n",
		"  [>] Store listing     2026-11-25   0/1       due in 10190 days\n", // the name column gave 7
		"  [ ] Release iHert...  2026-12-20   1/4\n",
		"  - Write release notes          due 2026-11-28  (in 10193 days)\n", // the name column gave 1
		"\nThis session:\n  topics Development, Design, Marketing and launch, Customer\n  support, Infrastructure,\n",
		"\n  A-topic-name-with-no-space-in-it-that-is-far-too-long-for-a...\n",
		"\n  Plan (built-in, always followed)\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
	// The widest state there can be (a clock so far back the days saturate) still fits; so does a progress line of
	// a thousand milestones (its last word goes).
	in = example()
	in.Deliverables.Milestones = []protocol.Milestone{{ID: "ms_1", Name: "M", Due: "2026-12-01"}}
	text = Build(in, time.Date(-200000, 1, 1, 0, 0, 0, 0, time.UTC)).Text()
	fits(t, text)
	if !strings.Contains(text, "  [>] M                2026-12-01   0/0       due in 106751 days\n") {
		t.Fatalf("%s", text)
	}
	many := example()
	var ms []protocol.Milestone
	for i := range 1000 {
		ms = append(ms, protocol.Milestone{ID: fmt.Sprintf("ms_%04d", i), Name: "M", Due: "2027-01-01",
			Actions: []protocol.Action{action(fmt.Sprintf("%04d", i), "A", "2027-01-01", "")}})
	}
	many.Deliverables.Milestones, many.Statuses = ms, map[string]protocol.ActionStatus{}
	for i := range 999 {
		many.Statuses[fmt.Sprintf("ai_%04d", i)] = protocol.ActionStatusDone
	}
	text = Build(many, now).Text()
	fits(t, text)
	if !strings.Contains(text, "Progress  [###################-]  99%   999 of 1000 done\n") {
		t.Fatalf("%s", strings.SplitN(text, "\n\n", 3)[0])
	}
}

// LATE, today, tomorrow, the "!" (within 2 days or late) and the 7-day window, each on its edge.
func TestDateEdges(t *testing.T) {
	in := example()
	in.Deliverables.Milestones = []protocol.Milestone{
		{ID: "ms_1", Name: "Today", Due: "2026-11-27", Actions: []protocol.Action{
			action("01", "Late by one day", "2026-11-26", vu),
			action("02", "Due today", "2026-11-27", vu),
		}},
		{ID: "ms_2", Name: "Later", Due: "2026-12-31", Actions: []protocol.Action{
			action("03", "Due tomorrow", "2026-11-28", vu),
			action("04", "Due in two days", "2026-11-29", vu),
			action("05", "Due in three days", "2026-11-30", vu),
			action("06", "Due in seven days", "2026-12-04", lan),
			action("07", "Due in eight days", "2026-12-05", lan),
		}},
	}
	in.Statuses = nil
	b := Build(in, now)
	golden(t, "edges.txt", b.Text())
	text := b.Text()
	for _, want := range []string{
		"  [>] Today                    2026-11-27   0/2       due today",
		"  - Late by one day               due 2026-11-26  (LATE)  !",
		"  - Due today                     due 2026-11-27  (today)  !",
		"  - Due tomorrow                  due 2026-11-28  (tomorrow)  !",
		"  - Due in two days               due 2026-11-29  (in 2 days)  !",
		"  - Due in three days             due 2026-11-30  (in 3 days)\n",
		"  - Due in seven days     Lan     2026-12-04\n",
		"  - Late by one day       you     2026-11-26  LATE\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "Due in eight days     Lan") {
		t.Fatal("8 days out is not due soon")
	}
	// Tomorrow's milestone, and one late before it: the current one is the first neither done nor late.
	in.Deliverables.Milestones[0].Due, in.Deliverables.Milestones[0].Actions[1].Due = "2026-11-26", "2026-11-26"
	in.Deliverables.Milestones[1].Due = "2026-11-28"
	for i := range in.Deliverables.Milestones[1].Actions {
		in.Deliverables.Milestones[1].Actions[i].Due = "2026-11-28"
	}
	text = Build(in, now).Text()
	for _, want := range []string{"  [!] Today                  2026-11-26   0/2       LATE", // "due tomorrow": the names give 2
		"  [>] Later                  2026-11-28   0/5       due tomorrow"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
}

// A clock set back (before anything was published): nothing is LATE, every date is ahead, and the board is still
// drawn as it is (no negative counts, no crash).
func TestClockSetBack(t *testing.T) {
	back := time.Date(2025, 1, 1, 8, 0, 0, 0, ict)
	b := Build(example(), back)
	golden(t, "clock-set-back.txt", b.Text())
	if strings.Contains(b.Text(), "LATE") || strings.Contains(b.Text(), "[!]") {
		t.Fatalf("LATE with the clock set back:\n%s", b.Text())
	}
}

// An action with no owner is "unowned" in Due soon, and never anyone's in Needs you.
func TestNoOwner(t *testing.T) {
	in := example()
	for i := range in.Deliverables.Milestones {
		for j := range in.Deliverables.Milestones[i].Actions {
			in.Deliverables.Milestones[i].Actions[j].Owner = nil
		}
	}
	b := Build(in, now)
	golden(t, "no-owner.txt", b.Text())
	if len(b.NeedsYou) != 0 || b.Yours.Total != 0 || !strings.Contains(b.Text(), "  - Write release notes   unowned 2026-11-28") {
		t.Fatalf("unowned actions:\n%s", b.Text())
	}
}

// Offline: drawn from the replica, with the last sync first (or "never synced").
func TestOffline(t *testing.T) {
	in := example()
	in.Offline, in.SyncedAt = true, time.Date(2026, 11, 27, 2, 12, 30, 0, time.UTC)
	b := Build(in, now)
	golden(t, "offline.txt", b.Text())
	goldenJSON(t, "offline.json", b)
	if !strings.HasPrefix(b.Text(), "(offline: as of 2026-11-27 09:12)\niHertz (1FlowCorp)") {
		t.Fatalf("offline:\n%s", b.Text())
	}
	in.SyncedAt = time.Time{}
	if !strings.HasPrefix(Build(in, now).Text(), "(offline: never synced)\n") {
		t.Fatalf("never synced:\n%s", Build(in, now).Text())
	}
}

// The footer: auto-update off, and the release the background sync just moved to.
func TestFooter(t *testing.T) {
	in := example()
	in.AutoUpdate, in.UpdatedTo, in.Session = false, "v0.1.2", ""
	text := Build(in, now).Text()
	if !strings.HasSuffix(text, "Waiting for your approval: 1    Open drifts: 2\nauto-update: off\nteamflow was updated to v0.1.2.\n") {
		t.Fatalf("footer:\n%s", text)
	}
	if strings.Contains(Build(example(), now).Text(), "auto-update") {
		t.Fatal("auto-update named while on")
	}
}

// The member unknown (no session on the project, no key): no "you", no row of yours, nothing claimed as yours.
func TestYouUnknown(t *testing.T) {
	in := example()
	in.You = ""
	b := Build(in, now)
	golden(t, "you-unknown.txt", b.Text())
	if strings.Contains(b.Text(), "you:") || strings.Contains(b.Text(), "\n  you ") || len(b.NeedsYou) != 0 {
		t.Fatalf("you unknown:\n%s", b.Text())
	}
}

// Bad data from the replica: a date that can't be read (shown as it is, never LATE, no "in N days"), an action
// with no due date of its own (its milestone's), and control characters in a teammate's name.
func TestBadData(t *testing.T) {
	in := example()
	d := in.Deliverables
	d.Milestones[0].Due = "someday"
	d.Milestones[0].Actions[0].Due = "someday"
	d.Milestones[2].Actions[2].Due = "" // Write release notes: the milestone's 2026-12-20
	d.Milestones[2].Actions[3].Name = "Store\nscreen\x1b[31mshots"
	in.Names[huy] = "Huy\r\n"
	in.Deliverables.PublishedAt = "yesterday"
	b := Build(in, now)
	golden(t, "bad-data.txt", b.Text())
	text := b.Text()
	for _, want := range []string{"  [ ] Store listing        someday      0/1\n",
		"  - Write release notes           due 2026-12-20  (in 23 days)\n", "  - Store screen [31m...  Huy     2026-11-30\n",
		"Plan v3 | Deliverables v2\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\r") {
		t.Fatalf("a control character got through:\n%q", text)
	}
}

// The same inputs, in another order, give the same bytes.
func TestDeterministic(t *testing.T) {
	a := Build(example(), now).Text()
	in := example()
	ms := in.Deliverables.Milestones
	for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
		ms[i], ms[j] = ms[j], ms[i]
	}
	for i := range ms {
		acts := ms[i].Actions
		for x, y := 0, len(acts)-1; x < y; x, y = x+1, y-1 {
			acts[x], acts[y] = acts[y], acts[x]
		}
	}
	for range 20 {
		if got := Build(in, now).Text(); got != a {
			t.Fatalf("another order, other bytes:\n%s\nvs\n%s", got, a)
		}
	}
	// Equal names and dates: the id decides.
	in = example()
	in.Deliverables.Milestones[3].Actions[1].Name = "Fix onboarding issues"
	in.Deliverables.Milestones[3].Actions[1].Owner = ptr(vu)
	in.Deliverables.Milestones[3].Actions[2].Owner = ptr(vu)
	b := Build(in, now)
	if b.NeedsYou[2].ID != "ai_08" || b.NeedsYou[3].ID != "ai_09" {
		t.Fatalf("ties: %+v", b.NeedsYou)
	}
}

func TestFit(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{{"abc", 3, "abc"}, {"abcd", 3, "abc"}, {"abcdef", 5, "ab..."}, {"Nguyễn Văn An", 8, "Nguyễ..."}, {"x", 0, ""}} {
		if got := fit(c.in, c.n); got != c.want {
			t.Errorf("fit(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// Display width, not characters: East Asian wide characters and emoji take two columns, combining marks none; the
// columns stay aligned and every line fits 64 columns on a terminal. A bidirectional override or a zero-width
// character in a name is a space.
func TestDisplayWidth(t *testing.T) {
	in := example()
	d := in.Deliverables
	d.Milestones[2].Name = "发布 iHertz 正式版本给所有用户"                              // wide
	d.Milestones[2].Actions[2].Name = "Viết ghi chú phát hành 🚀🚀 cho bản mới" // combining-free Vietnamese, emoji
	d.Milestones[2].Actions[3].Name = "Ảnh chụp\u202e màn hình\u200b"         // an RTL override, a zero-width space
	d.Milestones[3].Name = "Onboard first users"
	in.Names[huy] = "Huy\u202e Nguyễn"
	b := Build(in, now)
	golden(t, "wide.txt", b.Text())
	text := b.Text()
	if strings.ContainsAny(text, "\u202e\u200b") {
		t.Fatalf("an override or a zero-width character got through:\n%q", text)
	}
	col := -1 // the due column starts at the same display column in every milestone row
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "  [") {
			at := width(l[:strings.Index(l, "202")])
			if col >= 0 && at != col {
				t.Fatalf("the due column moved (%d, then %d):\n%s", col, at, text)
			}
			col = at
		}
	}
	for _, c := range []struct {
		s    string
		want int
	}{{"abc", 3}, {"发布", 4}, {"🚀", 2}, {"Nguyễn", 6}, {"Nguye\u0302\u0303n", 6}, {"✓", 1}, {"日本語abc", 9}} {
		if got := width(c.s); got != c.want {
			t.Errorf("width(%q) = %d, want %d", c.s, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{{"发布版本说明", 7, "发布..."}, {"发布版本说明", 12, "发布版本说明"}, {"ab发布", 4, "a..."}, {"🚀🚀🚀", 5, "🚀..."}} {
		if got := fit(c.in, c.n); got != c.want || width(got) > c.n {
			t.Errorf("fit(%q, %d) = %q (width %d), want %q", c.in, c.n, got, width(got), c.want)
		}
	}
}

// An owner (or a publisher) the replica has no name for shows as "member", never a raw id.
func TestOwnerWithNoName(t *testing.T) {
	in := example()
	delete(in.Names, huy)
	delete(in.Names, lan)
	text := Build(in, now).Text()
	if strings.Contains(text, "mb_") || !strings.Contains(text, "  - Store screenshots     member  2026-11-30\n") ||
		!strings.Contains(text, "updated 2026-11-26 15:28 by member\n") {
		t.Fatalf("%s", text)
	}
}

// Dates are the person's local dates: at 06:00 in UTC+7 it is still the day before in UTC, and "today" is the
// local day.
func TestLocalDateEarlyMorning(t *testing.T) {
	early := time.Date(2026, 11, 27, 6, 0, 0, 0, ict) // 2026-11-26 23:00 UTC
	in := example()
	in.Deliverables.Milestones = []protocol.Milestone{{ID: "ms_1", Name: "M", Due: "2026-11-28", Actions: []protocol.Action{
		action("01", "Due today", "2026-11-27", vu), action("02", "Due yesterday", "2026-11-26", vu),
		action("03", "Due tomorrow", "2026-11-28", vu)}}}
	in.Statuses = nil
	b := Build(in, early)
	text := b.Text()
	for _, want := range []string{"  - Due yesterday                 due 2026-11-26  (LATE)  !\n",
		"  - Due today                     due 2026-11-27  (today)  !\n",
		"  - Due tomorrow                  due 2026-11-28  (tomorrow)  !\n", "due tomorrow\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
	if b.Today != "2026-11-27" {
		t.Fatalf("today %s", b.Today)
	}
}
