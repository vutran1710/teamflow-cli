package core

// Sad paths of the agent's side at the core layer (internal/ops and the commands test the happy paths through it).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

func TestAgentActionsNeedASharedSession(t *testing.T) {
	ctx := context.Background()
	c := open(t, nil)
	if _, err := c.Emit(ctx, "nope", nil); !errors.Is(err, ErrSessionNotShared) {
		t.Fatalf("Emit with no session: %v", err)
	}
	if _, err := c.St.UpsertSession("loose", "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"Emit":         func() error { _, err := c.Emit(ctx, "loose", nil); return err },
		"ActionResult": func() error { _, err := c.ActionResult(ctx, "loose", "a", "DONE", "s", nil, ""); return err },
		"ActionRevoke": func() error { _, err := c.ActionRevoke(ctx, "loose", "a", "r"); return err },
		"Publish":      func() error { _, err := c.DeliverablesPublish(ctx, "loose", []byte("[]"), nil); return err },
		"Update":       func() error { _, err := c.DeliverablesUpdateMilestone(ctx, "loose", []byte("{}"), nil); return err },
		"Rename":       func() error { _, err := c.DeliverablesRename(ctx, "loose", nil, nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrSessionNotShared) {
			t.Errorf("%s on an unshared session: %v", name, err)
		}
	}
	if got := c.AfterTurn(ctx, "loose"); got != "" {
		t.Fatalf("AfterTurn of an unshared session %q", got)
	}
	if got := c.NewNotices("nope"); got != nil {
		t.Fatalf("NewNotices of no session %v", got)
	}
}

func TestDriftActIsOnlyTheHumanActs(t *testing.T) {
	ctx := context.Background()
	c := open(t, nil)
	if _, err := c.DriftAct(ctx, P, "drift.list", "dr_1", "x", nil); err == nil || !strings.Contains(err.Error(), "not a drift act") {
		t.Fatalf("a method that is not a human act: %v", err)
	}
	var b *BadInput
	if _, err := c.DriftAct(ctx, P, "drift.resolve", "dr_1", "  ", nil); !errors.As(err, &b) {
		t.Fatalf("no note: %v", err)
	}
	if _, err := c.DriftAct(ctx, P, "drift.dismiss", "dr_1", "x", nil); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("signed out: %v", err)
	}
}

func TestMembersByName(t *testing.T) {
	c := open(t, nil)
	if err := c.St.PutPeople(map[string]store.Person{"mb_1": {Name: "Huy", Alias: "h1"}, "mb_2": {Name: "huy"}, "mb_3": {Name: "Lan"}}); err != nil {
		t.Fatal(err)
	}
	if id, err := c.ResolveMember("LAN"); err != nil || id != "mb_3" {
		t.Fatalf("by name ignoring case: %s %v", id, err)
	}
	if id, err := c.ResolveMember("mb_2"); err != nil || id != "mb_2" {
		t.Fatalf("by id: %s %v", id, err)
	}
	_, err := c.ResolveMember("huy")
	if err == nil || !strings.Contains(err.Error(), "several members are called 'huy': Huy (h1) = mb_1, huy = mb_2; use the id") {
		t.Fatalf("two people with one name: %v", err)
	}
	if _, err := c.ResolveMember("Zed"); err == nil || !strings.Contains(err.Error(), "no member 'Zed'; members: Huy, Lan, huy") {
		t.Fatalf("nobody: %v", err)
	}
	everyone := []protocol.Member{{ID: "mb_1", Name: "Huy", Email: "huy@acme.test"}}
	if m, err := FindMember(everyone, "HUY@acme.test"); err != nil || m.ID != "mb_1" {
		t.Fatalf("by email: %v %v", m, err)
	}
	if _, err := FindMember(nil, "x"); err == nil {
		t.Fatal("found someone in no one")
	}
}

func TestFindActionRefusesTwoOfOneName(t *testing.T) {
	d := &protocol.Deliverables{Milestones: []protocol.Milestone{{Actions: []protocol.Action{
		{ID: "ai_1", Name: "Ship"}, {ID: "ai_2", Name: "SHIP"}, {ID: "ai_3", Name: "Test"}}}}}
	if a, err := findAction(d, "Ship"); err != nil || a.ID != "ai_1" {
		t.Fatalf("exact name first: %v %v", a, err)
	}
	if a, err := findAction(d, "test"); err != nil || a.ID != "ai_3" {
		t.Fatalf("ignoring case: %v %v", a, err)
	}
	if _, err := findAction(d, "ship"); err == nil || !strings.Contains(err.Error(), "several actions are called 'ship'") {
		t.Fatalf("two of one name: %v", err)
	}
	if a, err := findAction(nil, "x"); a != nil || err != nil {
		t.Fatalf("no Deliverables: %v %v", a, err)
	}
}
