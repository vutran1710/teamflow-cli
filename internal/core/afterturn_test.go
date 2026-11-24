package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

const (
	tM1 = "mb_00000000000000000000000001"
	tM2 = "mb_00000000000000000000000002"
	tT1 = "tp_00000000000000000000000001"
	tS1 = "sg_00000000000000000000000001"
)

// offlineCfg is a sign-in on a server nobody answers: syncs fail fast and the hooks work from the replica.
func offlineCfg() *config.Config {
	return &config.Config{Server: "http://127.0.0.1:1", AuthKey: "tfa_example", Email: "lan@acme.test"}
}

// seedApprovalFor puts Pricing with one topic, session sid shared with it as M1, and a DECISION by M2 asking M1 to
// approve (the sync raises an APPROVAL_REQUESTED notice for sid).
func seedApprovalFor(t *testing.T, c *Core, sid string) {
	t.Helper()
	if _, err := c.St.UpsertSession(sid, "agent", 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.St.Share(sid, P, tM1, "ref", nil); err != nil {
		t.Fatal(err)
	}
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = tS1, P, tT1, protocol.SignalKindDecision, "Round half up"
	body := protocol.NewKeyPointsBody()
	body.Type, body.Summary, body.Points = "key_points.v1", "Ties go up", []string{"0.025 becomes 0.05"}
	sig.Body = body
	sig.RequestForApproval = &protocol.ApprovalRequest{Approvers: []string{tM1}, Required: 1}
	s := protocol.NewStoredSignal()
	s.Signal, s.Author.Member, s.Author.Actor = sig, tM2, protocol.ActorAgent
	s.Seq, s.Chain, s.ChainVersion, s.State = 6, tS1, 1, protocol.SignalStatePending
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal = P, 6, "signal.accepted", s
	if err := c.St.Tx(func(tx *store.Tx) error { return syncer.Apply(tx, ev, time.Now()) }); err != nil {
		t.Fatal(err)
	}
}

// rawStore runs SQL on the store file behind the client's back (to break and mend a stored record).
func rawStore(t *testing.T, c *Core, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// A new approval request is raised once; when reading the queue fails after it was claimed, it is not lost: the
// next turn raises it (the acceptance rule: an important decision reaches the approver).
func TestAfterTurnNeverLosesAnApprovalRequest(t *testing.T) {
	ctx := context.Background()
	c := open(t, offlineCfg())
	seedApprovalFor(t, c, "s1")

	// Sad path: the replica's signal record is unreadable while the request is claimed.
	rawStore(t, c, "UPDATE signals SET data = '{' WHERE id = ?", tS1)
	if got := c.AfterTurn(ctx, "s1"); got != "" {
		t.Fatalf("a failed read raised %q", got)
	}
	ns, err := c.St.Notices("s1", true)
	if err != nil || len(ns) != 1 || ns[0].Type != protocol.NoticeTypeApprovalRequested {
		t.Fatalf("the request was lost after a failed read: %v %v", ns, err)
	}

	// Mended: the next turn raises it, once.
	seedApprovalFor(t, c, "s1") // rewrites the record (the notice is folded: one per signal)
	got := c.AfterTurn(ctx, "s1")
	if !strings.Contains(got, tS1) || !strings.Contains(got, "«Round half up»") {
		t.Fatalf("not raised after the failure: %q", got)
	}
	if again := c.AfterTurn(ctx, "s1"); again != "" {
		t.Fatalf("raised twice: %q", again)
	}
}

// Signed out, unknown session, or a store closed under it: nothing raised, nothing claimed.
func TestAfterTurnSadPaths(t *testing.T) {
	ctx := context.Background()
	c := open(t, nil)
	seedApprovalFor(t, c, "s1")
	if got := c.AfterTurn(ctx, "s1"); got != "" {
		t.Fatalf("signed out raised %q", got)
	}
	if ns, _ := c.St.Notices("s1", true); len(ns) != 1 {
		t.Fatalf("signed out claimed the request: %v", ns)
	}
	if got := c.AfterTurn(ctx, "nobody"); got != "" {
		t.Fatalf("unknown session raised %q", got)
	}
}
