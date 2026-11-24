package core

// The hooks of agents whose prompt hook cannot add context (Copilot CLI, Cursor CLI): BeforePrompt records and
// syncs but claims nothing; AfterTool tells what is new after a tool, synced as MidTurn syncs.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// The first AfterTool of a shared session gives the full context; later ones what is new, each signal once; an
// approval request is raised once; 50 tool steps make at most one network sync.
// Sad paths: nothing new (""), a repeated step (no second telling), the network throttled.
func TestAfterToolTellsWhatIsNewOnce(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, _ := midTurnCore(t, srv.URL)
	first, err := c.AfterTool(ctx, "s1")
	if err != nil || !strings.Contains(first, "## Teamflow: shared project") {
		t.Fatalf("first: %q %v", first, err)
	}
	if again, err := c.AfterTool(ctx, "s1"); err != nil || again != "" {
		t.Fatalf("nothing new: %q %v", again, err)
	}
	accept(t, c, "sg_00000000000000000000000NWS", "Fresh news", 1, protocol.SignalStateInForce, "", "")
	news, err := c.AfterTool(ctx, "s1")
	if err != nil || !strings.Contains(news, "Fresh news") {
		t.Fatalf("news: %q %v", news, err)
	}
	accept(t, c, "sg_00000000000000000000000ASK", "Approve me", 2, protocol.SignalStatePending, "", "2099-01-01T00:00:00Z")
	if err := c.St.Notice("s1", protocol.NoticeTypeApprovalRequested, "sg_00000000000000000000000ASK", "asks", time.Now()); err != nil {
		t.Fatal(err)
	}
	ask, err := c.AfterTool(ctx, "s1")
	if err != nil || !strings.Contains(ask, "a new approval request arrived") {
		t.Fatalf("approval: %q %v", ask, err)
	}
	for range 50 {
		if out, err := c.AfterTool(ctx, "s1"); err != nil || strings.Contains(out, "Fresh news") || strings.Contains(out, "new approval request") {
			t.Fatalf("told twice: %q %v", out, err)
		}
	}
	if calls.Load() > 2 {
		t.Fatalf("%d network calls for 54 tool steps", calls.Load())
	}
}

// Unknown, unshared, signed out, or a project not synced yet: nothing, no error.
func TestAfterToolSadPaths(t *testing.T) {
	ctx := context.Background()
	srv, _ := countingServer(t)
	c, _ := midTurnCore(t, srv.URL)
	if out, err := c.AfterTool(ctx, "nobody"); err != nil || out != "" {
		t.Fatalf("unknown: %q %v", out, err)
	}
	if _, err := c.St.UpsertSession("loose", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := c.AfterTool(ctx, "loose"); err != nil || out != "" {
		t.Fatalf("unshared: %q %v", out, err)
	}
	if err := c.St.Share("loose", "pj_notsynced", tM1, "ref", nil); err != nil {
		t.Fatal(err)
	}
	if out, err := c.AfterTool(ctx, "loose"); err != nil || out != "" {
		t.Fatalf("not synced: %q %v", out, err)
	}
	out := open(t, nil)
	if text, err := out.AfterTool(ctx, "s1"); err != nil || text != "" {
		t.Fatalf("signed out: %q %v", text, err)
	}
}

// BeforePrompt records the session (agent, pid, cwd) and syncs its project, but claims nothing: a waiting notice
// is still there for the next hook that can tell it. Signed out: recorded, no network.
// Sad paths: a pending notice not lost; signed out; an unshared session (no network).
func TestBeforePromptClaimsNothing(t *testing.T) {
	ctx := context.Background()
	srv, calls := countingServer(t)
	c, _ := midTurnCore(t, srv.URL)
	if err := c.St.Notice("s1", protocol.NoticeTypeApprovalRequested, "sg_x", "asks", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.BeforePrompt(ctx, "s1", "copilot", 4242, "/work"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("the prompt hook did not sync")
	}
	s, _, _ := c.St.Session("s1")
	if s.AgentPID != 4242 || s.Cwd != "/work" {
		t.Fatalf("not recorded: %+v", s)
	}
	if ns, _ := c.St.Notices("s1", true); len(ns) != 1 {
		t.Fatalf("a notice was claimed by a hook that cannot tell it: %v", ns)
	}
	before := calls.Load()
	if err := c.BeforePrompt(ctx, "fresh", "copilot", 1, ""); err != nil || calls.Load() != before {
		t.Fatalf("unshared session synced: %v", err)
	}
	out := open(t, nil)
	if err := out.BeforePrompt(ctx, "s9", "copilot", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := out.St.Session("s9"); !ok {
		t.Fatal("signed out: not recorded")
	}
}
