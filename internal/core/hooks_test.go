package core

// The hook paths at the core layer: what the Claude Code fixtures do not reach (the size budget, overdue notices,
// stale citations, the Deliverables lines, a project not synced yet, signed out).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

func seedProject(t *testing.T, c *Core) {
	t.Helper()
	p := protocol.NewProject()
	p.ID, p.Name, p.Initiator, p.Members, p.Version, p.Titles = P, "Pricing", tM1, []string{tM1, tM2}, 1, map[string]string{tM2: "Finance"}
	st := protocol.NewProjectState()
	st.Project, st.Version, st.Plan = P, 1, "Ship"
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.St.PutProject(p))
	must(c.St.PutTopic(protocol.Topic{ID: tT1, Project: P, Name: "Rounding", Description: "d"}))
	must(c.St.PutState(st))
	must(c.St.PutPeople(map[string]store.Person{tM1: {Name: "lan", Alias: "L"}, tM2: {Name: "huy", Title: "Dev"}}))
}

func accept(t *testing.T, c *Core, id, title string, seq int64, state protocol.SignalState, supersedes string, due string) {
	t.Helper()
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = id, P, tT1, protocol.SignalKindDecision, title
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points = "key_points.v1", strings.Repeat("long summary ", 40), []string{"p"}
	sig.Body = body
	chain := id
	if supersedes != "" {
		sig.Supersedes, chain = &supersedes, supersedes
	}
	if due != "" {
		sig.RequestForApproval = &protocol.ApprovalRequest{Approvers: []string{tM1}, Required: 1, Due: &due}
	}
	s := protocol.NewStoredSignal()
	s.Signal, s.Author.Member, s.Author.Actor = sig, tM2, protocol.ActorAgent
	s.Seq, s.Chain, s.ChainVersion, s.State = seq, chain, 1, state
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal = P, seq, "signal.accepted", s
	if err := c.St.Tx(func(tx *store.Tx) error {
		if err := syncer.Apply(tx, ev, time.Now()); err != nil {
			return err
		}
		return tx.SetCursor(P, seq)
	}); err != nil {
		t.Fatal(err)
	}
}

// Over the budget an agent keeps, the full context lists signals one line each; members are named with alias and
// title; a session following some topics is told so.
func TestFullContextBudgetAndPeople(t *testing.T) {
	ctx := context.Background()
	c := open(t, offlineCfg())
	seedProject(t, c)
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Share("s1", P, tM1, "ref", []string{tT1}); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		accept(t, c, "sg_"+strings.Repeat("0", 24)+string(rune('A'+i/10))+string(rune('0'+i%10)), "Point", int64(i+1), protocol.SignalStateInForce, "", "")
	}
	text, err := c.Full(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one line each to fit", "members lan (L); huy, Finance", "follows only these topics"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lacks %q:\n%s", want, text)
		}
	}
	if len(text) > ContextChars+500 {
		t.Fatalf("over budget: %d", len(text))
	}
}

// An approval asked of the session's member past its due time is told once as overdue; a cited signal since
// superseded is told stale once, with the version in force.
func TestDeltaOverdueAndStale(t *testing.T) {
	c := open(t, offlineCfg())
	seedProject(t, c)
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Share("s1", P, tM1, "ref", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Seen("s1", 0, 1); err != nil {
		t.Fatal(err)
	}
	old := "sg_00000000000000000000000OLD"
	accept(t, c, old, "Old", 1, protocol.SignalStateInForce, "", "")
	if _, err := c.Cite("s1", []string{old}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delta("s1"); err != nil {
		t.Fatal(err)
	}
	accept(t, c, "sg_00000000000000000000000DUE", "Late one", 2, protocol.SignalStatePending, "", "2020-01-01T00:00:00Z")
	if err := c.St.SetSignalState(old, protocol.SignalStateSuperseded, 3); err != nil {
		t.Fatal(err)
	}
	accept(t, c, "sg_00000000000000000000000NEW", "New", 3, protocol.SignalStateInForce, old, "")
	if err := c.St.Notice("s1", protocol.NoticeTypeCitedSuperseded, old, "You cite "+old+", now superseded.", time.Now()); err != nil {
		t.Fatal(err)
	}
	text, err := c.Delta("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Approval overdue: sg_00000000000000000000000DUE", "Stale citations",
		"You cite " + old + "; in force now:", "a new approval request arrived"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lacks %q:\n%s", want, text)
		}
	}
	if again, err := c.Delta("s1"); err != nil || strings.Contains(again, "overdue") || strings.Contains(again, "Stale") {
		t.Fatalf("told twice: %q %v", again, err)
	}
}

// A project not synced yet, a session not shared, signed out: no context, nothing claimed or failed.
func TestHookPathsSadPaths(t *testing.T) {
	ctx := context.Background()
	c := open(t, offlineCfg())
	if _, err := c.St.UpsertSession("s1", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Share("s1", P, tM1, "ref", nil); err != nil {
		t.Fatal(err)
	}
	if text, err := c.Full(ctx, "s1"); err != nil || !strings.Contains(text, "not synced yet") {
		t.Fatalf("not synced: %q %v", text, err)
	}
	if text, err := c.Delta("s1"); err != nil || text != "" {
		t.Fatalf("delta before sync: %q %v", text, err)
	}
	if text, err := c.Prompt(ctx, "loose", "agent", 0, "", "hi"); err != nil || text != "" {
		t.Fatalf("unshared: %q %v", text, err)
	}
	if text, err := c.AfterCompact("loose"); err != nil || text != "" {
		t.Fatalf("compact unshared: %q %v", text, err)
	}
	out := open(t, nil)
	if text, err := out.Start(ctx, "s9", "agent", 0, "", "startup"); err != nil || text != "" {
		t.Fatalf("signed out start: %q %v", text, err)
	}
	if err := out.End(ctx, "s9"); err != nil {
		t.Fatal(err)
	}
}
