package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func signal(id, project, topic string, seq int64, state protocol.SignalState) protocol.StoredSignal {
	s := protocol.NewStoredSignal()
	s.Signal = protocol.NewSignal()
	s.Signal.ID, s.Signal.Project, s.Signal.Topic = id, project, topic
	s.Signal.Kind = protocol.SignalKindNote
	s.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "hello"}
	s.Seq, s.Chain, s.ChainVersion, s.State = seq, id, 1, state
	return s
}

func TestStoreFilesAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	st := open(t, dir)
	must(t, st.SetCursor("pj_a", 3)) // a write, so the WAL file exists
	for _, name := range []string{FileName, FileName + "-wal", FileName + "-shm"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o, want 600", name, fi.Mode().Perm())
		}
	}
}

func TestOpenTightensAnOpenStoreFile(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	st.Close()
	must(t, os.Chmod(filepath.Join(dir, FileName), 0o644))
	open(t, dir)
	fi, _ := os.Stat(filepath.Join(dir, FileName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o after reopening, want 600", fi.Mode().Perm())
	}
}

func TestStoreIsWALWithShortBusyTimeout(t *testing.T) {
	st := open(t, t.TempDir())
	var mode string
	var busy int
	must(t, st.db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	must(t, st.db.QueryRow("PRAGMA busy_timeout").Scan(&busy))
	if mode != "wal" || busy != 250 {
		t.Fatalf("journal_mode=%s busy_timeout=%d, want wal and 250", mode, busy)
	}
}

func TestReopenKeepsDataAndMigratesOnce(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	must(t, st.SetCursor("pj_a", 7))
	st.Close()
	st = open(t, dir)
	if got := st.Cursors(); got["pj_a"] != 7 {
		t.Fatalf("cursors after reopen %v", got)
	}
	var n int
	must(t, st.db.QueryRow("SELECT COUNT(*) FROM "+migrationsTable).Scan(&n))
	if n != len(migrationNames(t)) {
		t.Fatalf("%d migrations recorded, want %d", n, len(migrationNames(t)))
	}
}

func migrationNames(t *testing.T) []string {
	ents, err := migrations.ReadDir("migrations")
	must(t, err)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// Review Focus 1: the background sync holds a write transaction while a hook reads. The hook must answer within
// budget: WAL lets it read the last committed state without waiting.
func TestHookReadsWhileSyncWrites(t *testing.T) {
	dir := t.TempDir()
	syncSt := open(t, dir)
	must(t, syncSt.SetCursor("pj_a", 5))

	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- syncSt.Tx(func(tx *Tx) error {
			if err := tx.SetCursor("pj_a", 9); err != nil {
				return err
			}
			close(held)
			time.Sleep(time.Second)
			return nil
		})
	}()
	<-held

	start := time.Now()
	hook, err := Open(dir) // a hook is a new process: it opens the store too
	if err != nil {
		t.Fatal(err)
	}
	defer hook.Close()
	got := hook.Cursors()
	sameStore := syncSt.Cursors() // and a reader on the writer's own handle
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("a hook's open and read took %v while the sync wrote; budget 300ms", el)
	}
	if got["pj_a"] != 5 || sameStore["pj_a"] != 5 {
		t.Fatalf("reader saw %v / %v, want the committed 5", got, sameStore)
	}
	must(t, <-done)
	if got := hook.Cursors(); got["pj_a"] != 9 {
		t.Fatalf("after the commit the reader sees %v, want 9", got)
	}
}

// The worst case of a writer: an EXCLUSIVE transaction (in a rollback journal it would shut readers out; in WAL
// it must not).
func TestHookReadsWhileExclusiveWriterHolds(t *testing.T) {
	dir := t.TempDir()
	syncSt := open(t, dir)
	must(t, syncSt.SetCursor("pj_a", 5))
	ctx := context.Background()
	conn, err := syncSt.db.Conn(ctx)
	must(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE")
	must(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE cursors SET seq = 9 WHERE project = 'pj_a'")
	must(t, err)

	start := time.Now()
	hook := open(t, dir)
	got := hook.Cursors()
	_, _, _, _, _ = hook.ProjectKey("pj_a")
	_ = hook.LiveAt("pj_a")
	el := time.Since(start)
	_, err = conn.ExecContext(ctx, "COMMIT")
	must(t, err)
	if el > 300*time.Millisecond || got["pj_a"] != 5 {
		t.Fatalf("hook read %v in %v under an exclusive writer; want the committed 5 within 300ms", got, el)
	}
}

// Past the busy timeout a second writer gets ErrBusy quickly, so a hook can answer from what it has.
func TestWriterWhileSyncWritesIsBusyWithinBudget(t *testing.T) {
	dir := t.TempDir()
	syncSt := open(t, dir)
	hook := open(t, dir)

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- syncSt.Tx(func(tx *Tx) error {
			if err := tx.SetCursor("pj_a", 1); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	start := time.Now()
	err := hook.SetCursor("pj_b", 1)
	el := time.Since(start)
	close(release)
	must(t, <-done)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if el > 600*time.Millisecond {
		t.Fatalf("busy after %v; the timeout is 250ms", el)
	}
	err = hook.Tx(func(tx *Tx) error { return tx.SetCursor("pj_b", 2) })
	must(t, err) // free again
}

func TestTxRollsBackOnError(t *testing.T) {
	st := open(t, t.TempDir())
	boom := errors.New("boom")
	err := st.Tx(func(tx *Tx) error {
		if err := tx.SetCursor("pj_a", 4); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx returned %v, want boom", err)
	}
	if _, ok := st.Cursors()["pj_a"]; ok {
		t.Fatal("the failed transaction's write is visible")
	}
}

func TestTxRollsBackOnPanic(t *testing.T) {
	st := open(t, t.TempDir())
	func() {
		defer func() { _ = recover() }()
		_ = st.Tx(func(tx *Tx) error {
			_ = tx.SetCursor("pj_a", 4)
			panic("boom")
		})
	}()
	if _, ok := st.Cursors()["pj_a"]; ok {
		t.Fatal("the panicking transaction's write is visible")
	}
	must(t, st.SetCursor("pj_a", 1)) // and the store is usable after it
}

func TestCursorsNeverGoBack(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SetCursor("pj_a", 10))
	must(t, st.SetCursor("pj_a", 4))
	if got := st.Cursors()["pj_a"]; got != 10 {
		t.Fatalf("cursor %d, want 10", got)
	}
}

func TestReadsOnAClosedStoreAnswerEmpty(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SetCursor("pj_a", 1))
	must(t, st.PutProjectKey("pj_a", "tfk_secret", "mb_1", "org_1", "Alpha", t0))
	st.Close()
	if got := st.Cursors(); len(got) != 0 {
		t.Fatalf("closed store cursors %v", got)
	}
	if _, _, _, _, ok := st.ProjectKey("pj_a"); ok {
		t.Fatal("closed store gave a key")
	}
	if !st.LiveAt("pj_a").IsZero() {
		t.Fatal("closed store gave a live time")
	}
	if err := st.SetCursor("pj_a", 2); err == nil {
		t.Fatal("a write to a closed store succeeded")
	}
}

func TestProjectKeys(t *testing.T) {
	st := open(t, t.TempDir())
	if _, _, _, _, ok := st.ProjectKey("pj_a"); ok {
		t.Fatal("a key before any was put")
	}
	must(t, st.PutProjectKey("pj_b", "tfk_b", "mb_1", "org_1", "Beta", t0))
	must(t, st.PutProjectKey("pj_a", "tfk_a", "mb_1", "org_1", "Alpha", t0.Add(time.Minute)))
	must(t, st.PutProjectKey("pj_a", "tfk_a2", "mb_2", "org_1", "Alpha 2", t0.Add(2*time.Minute)))
	key, member, org, name, ok := st.ProjectKey("pj_a")
	if !ok || key != "tfk_a2" || member != "mb_2" || org != "org_1" || name != "Alpha 2" {
		t.Fatalf("ProjectKey = %q %q %q %q %v", key, member, org, name, ok)
	}
	ps, err := st.KeyProjects()
	must(t, err)
	if !reflect.DeepEqual(ps, []string{"pj_b", "pj_a"}) {
		t.Fatalf("KeyProjects %v, want in fetch order", ps)
	}
	must(t, st.DropProjectKey("pj_a"))
	if _, _, _, _, ok := st.ProjectKey("pj_a"); ok {
		t.Fatal("dropped key still there")
	}
	must(t, st.DropProjectKey("pj_zzz")) // dropping a missing key is fine
	must(t, st.DropProjectKeys())
	if ps, _ := st.KeyProjects(); len(ps) != 0 {
		t.Fatalf("keys left %v", ps)
	}
}

func fillProject(t *testing.T, st *Store, p string, seq int64) {
	t.Helper()
	pr := protocol.NewProject()
	pr.ID, pr.Name = p, "P "+p
	must(t, st.PutProject(pr))
	tp := protocol.NewTopic()
	tp.ID, tp.Project = "tp_"+p, p
	must(t, st.PutTopic(tp))
	ps := protocol.NewProjectState()
	ps.Project, ps.Version = p, 1
	must(t, st.PutState(ps))
	must(t, st.PutSignal(signal("sg_"+p, p, "tp_"+p, seq, protocol.SignalStateInForce)))
	d := protocol.NewDrift()
	d.ID, d.Project, d.Status = "dr_"+p, p, protocol.DriftStatusOpen
	must(t, st.PutDrift(d, seq))
	must(t, st.SetArchived(p, "sg_"+p, true, "old"))
	dl := protocol.NewDeliverables()
	dl.Project, dl.Version = p, 2
	must(t, st.PutDeliverables(dl, seq))
	must(t, st.SetCursor(p, seq))
	must(t, st.SetLiveAt(p, t0))
	must(t, st.PutProjectKey(p, "tfk_"+p, "mb_1", "org_1", p, t0))
	_, err := st.UpsertSession("ss_"+p, "agent", 100, "/w", t0)
	must(t, err)
	must(t, st.Share("ss_"+p, p, "mb_1", "ref", nil))
	must(t, st.Seen("ss_"+p, seq, 1))
	must(t, st.Notice("ss_"+p, protocol.NoticeTypeProjectUpdated, "x", "updated", t0))
}

func TestResetProjectDropsOnlyThatProject(t *testing.T) {
	st := open(t, t.TempDir())
	fillProject(t, st, "pj_a", 40)
	fillProject(t, st, "pj_b", 30)

	must(t, st.ResetProject("pj_a", 12))

	if _, ok, _ := st.Project("pj_a"); ok {
		t.Fatal("pj_a's project row survived")
	}
	if ts, _ := st.Topics("pj_a"); len(ts) != 0 {
		t.Fatal("pj_a's topics survived")
	}
	if _, ok, _ := st.LatestState("pj_a"); ok {
		t.Fatal("pj_a's state survived")
	}
	if _, ok, _ := st.Signal("sg_pj_a"); ok {
		t.Fatal("pj_a's signal survived")
	}
	if ds, _ := st.Drifts("pj_a", ""); len(ds) != 0 {
		t.Fatal("pj_a's drifts survived")
	}
	if a, _ := st.ArchivedChains("pj_a"); len(a) != 0 {
		t.Fatal("pj_a's archive survived")
	}
	if _, ok, _ := st.Deliverables("pj_a"); ok {
		t.Fatal("pj_a's deliverables survived")
	}
	if c, ok := st.Cursors()["pj_a"]; !ok || c != 0 {
		t.Fatalf("pj_a's cursor %d (present %v), want 0", c, ok)
	}
	if !st.LiveAt("pj_a").IsZero() {
		t.Fatal("pj_a still counts as live")
	}
	// the key and the sessions stay; a session's place is clamped to the server's head
	if _, _, _, _, ok := st.ProjectKey("pj_a"); !ok {
		t.Fatal("reset dropped the project key")
	}
	sa, ok, err := st.Session("ss_pj_a")
	must(t, err)
	if !ok || sa.Project != "pj_a" || sa.LastSeq != 12 {
		t.Fatalf("session after reset %+v", sa)
	}

	// pj_b is untouched
	if _, ok, _ := st.Project("pj_b"); !ok {
		t.Fatal("pj_b's project row dropped")
	}
	if ts, _ := st.Topics("pj_b"); len(ts) != 1 {
		t.Fatal("pj_b's topics dropped")
	}
	if _, ok, _ := st.LatestState("pj_b"); !ok {
		t.Fatal("pj_b's state dropped")
	}
	if _, ok, _ := st.Signal("sg_pj_b"); !ok {
		t.Fatal("pj_b's signal dropped")
	}
	if ds, _ := st.Drifts("pj_b", ""); len(ds) != 1 {
		t.Fatal("pj_b's drifts dropped")
	}
	if a, _ := st.ArchivedChains("pj_b"); !a["sg_pj_b"] {
		t.Fatal("pj_b's archive dropped")
	}
	if _, ok, _ := st.Deliverables("pj_b"); !ok {
		t.Fatal("pj_b's deliverables dropped")
	}
	if st.Cursors()["pj_b"] != 30 || st.LiveAt("pj_b").IsZero() {
		t.Fatal("pj_b's cursor or live time changed")
	}
	sb, _, _ := st.Session("ss_pj_b")
	if sb.LastSeq != 30 {
		t.Fatalf("pj_b's session moved to %d", sb.LastSeq)
	}
	// a session already below the head keeps its place
	must(t, st.ResetProject("pj_b", 100))
	sb, _, _ = st.Session("ss_pj_b")
	if sb.LastSeq != 30 {
		t.Fatalf("session below the head moved to %d", sb.LastSeq)
	}
}

func TestSpool(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SpoolAdd("sg_1", "ss_1", "ref_a", `{"a":1}`, t0))
	must(t, st.SpoolAdd("sg_2", "ss_1", "ref_b", `{"a":2}`, t0))
	must(t, st.SpoolAdd("sg_3", "ss_2", "ref_a", `{"a":3}`, t0))
	must(t, st.SpoolAdd("sg_1", "ss_9", "ref_a", `{"changed":1}`, t0)) // the same signal again: kept once, as first
	items, err := st.Spooled("ref_a")
	must(t, err)
	if len(items) != 2 || items[0].Signal != "sg_1" || items[1].Signal != "sg_3" || items[0].Params != `{"a":1}` ||
		items[0].Session != "ss_1" {
		t.Fatalf("Spooled %+v", items)
	}
	if n, _ := st.SpoolSize(); n != 3 {
		t.Fatalf("SpoolSize %d", n)
	}
	must(t, st.SpoolDone("sg_1"))
	must(t, st.SpoolDone("sg_missing"))
	if n, _ := st.SpoolSize(); n != 2 {
		t.Fatalf("SpoolSize after done %d", n)
	}
	if items, _ := st.Spooled("ref_none"); len(items) != 0 {
		t.Fatalf("another key's spool %+v", items)
	}
}

func TestSessions(t *testing.T) {
	st := open(t, t.TempDir())
	if _, ok, err := st.Session("ss_none"); ok || err != nil {
		t.Fatalf("missing session: ok=%v err=%v", ok, err)
	}
	s, err := st.UpsertSession("ss_1", "agent", 100, "/w", t0)
	must(t, err)
	if s.ID != "ss_1" || s.AgentPID != 100 || s.Project != "" || s.Topics != nil || !s.Follows("tp_any", nil) {
		t.Fatalf("new session %+v", s)
	}
	_, err = st.UpsertSession("ss_2", "agent", 100, "/w", t0.Add(time.Second))
	must(t, err)
	_, err = st.UpsertSession("ss_3", "agent", 0, "/x", t0)
	must(t, err)
	s3, _, _ := st.Session("ss_3")
	if s3.AgentPID != 0 {
		t.Fatalf("no pid should read as 0, got %d", s3.AgentPID)
	}

	got, ok, err := st.SessionOfPIDs([]int{555, 100})
	must(t, err)
	if !ok || got.ID != "ss_2" {
		t.Fatalf("SessionOfPIDs = %+v %v, want the latest in pid 100", got, ok)
	}
	if _, ok, _ := st.SessionOfPIDs([]int{555}); ok {
		t.Fatal("a session for an unknown pid")
	}
	prev, ok, _ := st.PreviousSession(100, "ss_2")
	if !ok || prev.ID != "ss_1" {
		t.Fatalf("PreviousSession %+v", prev)
	}

	must(t, st.Share("ss_1", "pj_a", "mb_1", "ref_a", []string{"tp_x"}))
	must(t, st.Seen("ss_1", 40, 3))
	s, _, _ = st.Session("ss_1")
	if s.Project != "pj_a" || s.Member != "mb_1" || s.KeyRef != "ref_a" || s.LastSeq != 40 || s.StateVersion != 3 {
		t.Fatalf("shared session %+v", s)
	}
	if !s.Follows("tp_x", nil) || s.Follows("tp_y", nil) || !s.Follows("tp_plan", map[string]bool{"tp_plan": true}) {
		t.Fatalf("Follows wrong for topics %v", s.Topics)
	}
	must(t, st.Share("ss_1", "pj_a", "mb_1", "ref_a", []string{})) // an empty list follows no topic, unlike nil
	s, _, _ = st.Session("ss_1")
	if s.Topics == nil || s.Follows("tp_x", nil) || s.LastSeq != 0 {
		t.Fatalf("empty topics / reshare %+v", s)
	}
	on, _ := st.SessionsOn("pj_a")
	shared, _ := st.SharedSessions()
	if len(on) != 1 || len(shared) != 1 {
		t.Fatalf("SessionsOn %d SharedSessions %d", len(on), len(shared))
	}
	must(t, st.Share("ss_1", "", "", "", nil)) // unshare
	if shared, _ := st.SharedSessions(); len(shared) != 0 {
		t.Fatal("unshared session still shared")
	}
}

func TestNotices(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.Notice("ss_1", protocol.NoticeTypeApprovalRequested, "sg_1", "approve sg_1", t0))
	must(t, st.Notice("ss_1", protocol.NoticeTypeApprovalRequested, "sg_1", "again", t0)) // once per (type, about)
	must(t, st.Notice("ss_1", protocol.NoticeTypeProjectUpdated, "", "a", t0))            // no subject: never folded
	must(t, st.Notice("ss_1", protocol.NoticeTypeProjectUpdated, "", "b", t0))
	must(t, st.Notice("ss_2", protocol.NoticeTypeApprovalRequested, "sg_1", "other session", t0))
	ns, err := st.Notices("ss_1", true)
	must(t, err)
	if len(ns) != 3 || ns[0].Text != "approve sg_1" || ns[0].About != "sg_1" || ns[1].About != "" {
		t.Fatalf("Notices %+v", ns)
	}
	must(t, st.NoticesShown([]int64{ns[0].N, ns[1].N}))
	if ns, _ := st.Notices("ss_1", true); len(ns) != 1 || ns[0].Text != "b" {
		t.Fatalf("unshown after showing %+v", ns)
	}
	if ns, _ := st.Notices("ss_1", false); len(ns) != 3 {
		t.Fatalf("all notices %+v", ns)
	}
	must(t, st.NoticesShown(nil))
}

func TestCitations(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.Cite("ss_1", []string{"sg_1", "sg_2"}, t0))
	must(t, st.Cite("ss_1", []string{"sg_1"}, t0.Add(time.Second)))
	must(t, st.Cite("ss_2", []string{"sg_2"}, t0))
	cs, err := st.Citations("ss_1")
	must(t, err)
	if len(cs) != 2 {
		t.Fatalf("Citations %v", cs)
	}
	by, err := st.SessionsCiting([]string{"sg_2", "sg_9"})
	must(t, err)
	if !reflect.DeepEqual(by, map[string][]string{"ss_1": {"sg_2"}, "ss_2": {"sg_2"}}) {
		t.Fatalf("SessionsCiting %v", by)
	}
}

func TestPeople(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.PutNames(map[string]string{"mb_1": "Ann", "mb_2": "Bo"}))
	must(t, st.PutPeople(map[string]Person{"mb_1": {Name: "Ann B", Alias: "annie", Title: "Lead"}}))
	ppl, err := st.People()
	must(t, err)
	if ppl["mb_1"] != (Person{Name: "Ann B", Alias: "annie", Title: "Lead"}) || ppl["mb_2"].Name != "Bo" {
		t.Fatalf("People %+v", ppl)
	}
	must(t, st.PutNames(map[string]string{"mb_1": "Ann C"})) // names keep alias and title
	names, _ := st.Names()
	ppl, _ = st.People()
	if names["mb_1"] != "Ann C" || ppl["mb_1"].Alias != "annie" {
		t.Fatalf("Names %v People %v", names, ppl)
	}
}

func TestDeliverables(t *testing.T) {
	st := open(t, t.TempDir())
	if _, ok, err := st.Deliverables("pj_a"); ok || err != nil {
		t.Fatal("deliverables before any")
	}
	must(t, st.SetDeliverablesArchived("pj_a", true, "x")) // nothing to archive: no error, no row
	if _, ok, _ := st.Deliverables("pj_a"); ok {
		t.Fatal("archiving created a row")
	}
	d := protocol.NewDeliverables()
	d.Project, d.Version = "pj_a", 3
	must(t, st.PutDeliverables(d, 17))
	if seq, _ := st.DeliverablesSeq("pj_a"); seq != 17 {
		t.Fatalf("DeliverablesSeq %d", seq)
	}
	if seq, _ := st.DeliverablesSeq("pj_none"); seq != 0 {
		t.Fatalf("DeliverablesSeq of none %d", seq)
	}
	must(t, st.SetDeliverablesArchived("pj_a", true, "done"))
	got, ok, err := st.Deliverables("pj_a")
	must(t, err)
	if !ok || !got.Archived || got.ArchiveNote != "done" || got.Version != 3 {
		t.Fatalf("archived deliverables %+v", got)
	}
	must(t, st.SetDeliverablesArchived("pj_a", false, "ignored"))
	got, _, _ = st.Deliverables("pj_a")
	if got.Archived || got.ArchiveNote != "" {
		t.Fatalf("unarchived deliverables %+v", got)
	}
	must(t, st.DropDeliverables("pj_a"))
	if _, ok, _ := st.Deliverables("pj_a"); ok {
		t.Fatal("dropped deliverables still there")
	}
}

func TestSignals(t *testing.T) {
	st := open(t, t.TempDir())
	a := signal("sg_1", "pj_a", "tp_1", 5, protocol.SignalStatePending)
	a.Signal.RequestForApproval = &protocol.ApprovalRequest{Approvers: []string{"mb_1"}, Required: 1}
	must(t, st.PutSignal(a))
	must(t, st.PutSignal(signal("sg_2", "pj_a", "tp_1", 6, protocol.SignalStateInForce)))
	r := signal("sg_3", "pj_a", "tp_1", 7, protocol.SignalStateInForce)
	r.Signal.Kind = protocol.SignalKindActionResult
	r.Signal.Body = protocol.ActionResultBody{Type: "action_result.v1", Action: "ac_1", Outcome: protocol.ActionOutcomeDone}
	must(t, st.PutSignal(r))

	got, ok, err := st.Signal("sg_1")
	must(t, err)
	if !ok || got.Seq != 5 || got.Signal.RequestForApproval == nil {
		t.Fatalf("Signal %+v", got)
	}
	if body, ok := got.Signal.Body.(protocol.CommentBody); !ok || body.Text != "hello" {
		t.Fatalf("body did not round-trip: %#v", got.Signal.Body)
	}
	if p, _ := st.PendingFor("mb_1"); len(p) != 1 {
		t.Fatalf("PendingFor mb_1 %d", len(p))
	}
	if p, _ := st.PendingFor("mb_2"); len(p) != 0 {
		t.Fatalf("PendingFor mb_2 %d", len(p))
	}
	must(t, st.SetSignalState("sg_1", protocol.SignalStateRejected, 9))
	must(t, st.SetSignalState("sg_missing", protocol.SignalStateRejected, 9))
	got, _, _ = st.Signal("sg_1")
	if got.State != protocol.SignalStateRejected {
		t.Fatalf("state %s", got.State)
	}
	after, _ := st.SignalsAfter("pj_a", 5)
	if len(after) != 2 || after[0].Signal.ID != "sg_2" {
		t.Fatalf("SignalsAfter %d", len(after))
	}
	changed, _ := st.ChangedAfter("pj_a", 6)
	if len(changed) != 2 || changed[0].Signal.Signal.ID != "sg_3" || !changed[0].New ||
		changed[1].Signal.Signal.ID != "sg_1" || changed[1].New {
		t.Fatalf("ChangedAfter %+v", changed)
	}
	ar, err := st.ActionResults("pj_a")
	must(t, err)
	if len(ar["ac_1"]) != 1 || len(ar) != 1 {
		t.Fatalf("ActionResults %+v", ar)
	}
	ch, _ := st.Chain("sg_2")
	if len(ch) != 1 {
		t.Fatalf("Chain %d", len(ch))
	}
	must(t, st.Cite("ss_1", []string{"sg_2"}, t0))
	must(t, st.DeleteTopic("tp_1", []string{"sg_2"}, nil))
	if _, ok, _ := st.Signal("sg_1"); ok {
		t.Fatal("DeleteTopic left the topic's signals")
	}
	if cs, _ := st.Citations("ss_1"); len(cs) != 0 {
		t.Fatal("DeleteTopic left citations")
	}
}

func TestDriftsByStatus(t *testing.T) {
	st := open(t, t.TempDir())
	d1 := protocol.NewDrift()
	d1.ID, d1.Project, d1.Status = "dr_1", "pj_a", protocol.DriftStatusOpen
	d2 := d1
	d2.ID, d2.Status = "dr_2", protocol.DriftStatusResolved
	must(t, st.PutDrift(d2, 2))
	must(t, st.PutDrift(d1, 1))
	all, _ := st.Drifts("pj_a", "")
	open, _ := st.Drifts("pj_a", protocol.DriftStatusOpen)
	if len(all) != 2 || all[0].ID != "dr_1" || len(open) != 1 {
		t.Fatalf("Drifts all=%d open=%d", len(all), len(open))
	}
}

func TestLiveAt(t *testing.T) {
	st := open(t, t.TempDir())
	if !st.LiveAt("pj_a").IsZero() {
		t.Fatal("live before any event")
	}
	at := t0.Add(1500 * time.Millisecond)
	must(t, st.SetLiveAt("pj_a", at))
	if got := st.LiveAt("pj_a"); !got.Equal(at) {
		t.Fatalf("LiveAt %v, want %v", got, at)
	}
}

func TestProtocolOK(t *testing.T) {
	st := open(t, t.TempDir())
	if ok, msg := st.ProtocolOK(); !ok || msg != "" {
		t.Fatalf("fresh store ProtocolOK = %v %q", ok, msg)
	}
	must(t, st.SetProtocol(protocol.ProtocolVersion+1, true))
	ok, msg := st.ProtocolOK()
	if ok || !strings.Contains(msg, "update teamflow") {
		t.Fatalf("after UPGRADE_REQUIRED ProtocolOK = %v %q", ok, msg)
	}
	must(t, st.SetProtocol(protocol.ProtocolVersion+1, false)) // served again
	if ok, _ := st.ProtocolOK(); !ok {
		t.Fatal("served again but still refused")
	}
	must(t, st.SetProtocol(0, false)) // an answer without the header keeps the known protocol
	if got := st.ServerProtocol(); got != protocol.ProtocolVersion+1 {
		t.Fatalf("ServerProtocol %d", got)
	}
}

// pythonEraStore builds a client.sqlite as the Python client left it: its schema_migrations names 0001..0007 and
// its tables, with no signals.kind, live or meta.
func pythonEraStore(t *testing.T, dir string) []byte {
	t.Helper()
	path := filepath.Join(dir, LegacyFileName)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	must(t, err)
	stmts := []string{
		"CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)",
		"CREATE TABLE signals (id TEXT PRIMARY KEY, project TEXT NOT NULL, topic TEXT NOT NULL, seq INTEGER NOT NULL, " +
			"chain TEXT NOT NULL, chain_version INTEGER NOT NULL, state TEXT NOT NULL, changed_seq INTEGER NOT NULL, data TEXT NOT NULL)",
		"CREATE TABLE cursors (project TEXT PRIMARY KEY, seq INTEGER NOT NULL)",
		"CREATE TABLE project_keys (project TEXT PRIMARY KEY, key TEXT NOT NULL, member TEXT NOT NULL, org TEXT NOT NULL, " +
			"name TEXT NOT NULL, fetched_at TEXT NOT NULL)",
		"INSERT INTO cursors VALUES ('pj_old', 99)",
		"INSERT INTO project_keys VALUES ('pj_old', 'tfk_old', 'mb_1', 'org_1', 'Old', '2026-01-01T00:00:00+00:00')",
	}
	for _, n := range []string{"0001_init.sql", "0002_artifact_archive.sql", "0003_project_keys.sql", "0004_verdict_grants.sql",
		"0005_session_topics.sql", "0006_people.sql", "0007_deliverables.sql"} {
		stmts = append(stmts, "INSERT INTO schema_migrations VALUES ('"+n+"', '2026-01-01 00:00:00')")
	}
	for _, q := range stmts {
		_, err := db.Exec(q)
		must(t, err)
	}
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	must(t, err)
	must(t, db.Close())
	b, err := os.ReadFile(path)
	must(t, err)
	return b
}

// The Go client starts fresh beside a Python-era store: it never opens or changes client.sqlite.
func TestPythonEraStoreIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	before := pythonEraStore(t, dir)
	if !LegacyPresent(dir) {
		t.Fatal("LegacyPresent missed the Python-era store")
	}
	st := open(t, dir)
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("Open did not create %s: %v", FileName, err)
	}
	if c := st.Cursors(); len(c) != 0 {
		t.Fatalf("the Go store read the Python-era cursors: %v", c)
	}
	if _, _, _, _, ok := st.ProjectKey("pj_old"); ok {
		t.Fatal("the Go store read a Python-era key")
	}
	// the Go schema is in place: the columns and tables the Python store lacks work
	must(t, st.PutSignal(signal("sg_1", "pj_a", "tp_1", 1, protocol.SignalStateInForce)))
	must(t, st.SetLiveAt("pj_a", t0))
	must(t, st.SetProtocol(1, true))
	if ok, _ := st.ProtocolOK(); ok {
		t.Fatal("ProtocolOK ignored the recorded refusal")
	}
	st.Close()
	after, err := os.ReadFile(filepath.Join(dir, LegacyFileName))
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("client.sqlite changed")
	}
	for _, f := range []string{LegacyFileName + "-wal", LegacyFileName + "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s appeared: the legacy store was opened", f)
		}
	}
}

func TestLegacyPresentWithoutOne(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)
	if LegacyPresent(dir) {
		t.Fatal("LegacyPresent with no client.sqlite")
	}
	must(t, os.Mkdir(filepath.Join(dir, LegacyFileName), 0o700))
	if LegacyPresent(dir) {
		t.Fatal("a folder named client.sqlite counted as a store")
	}
}

func TestCursorsErrReportsAStoreThatCantAnswer(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SetCursor("pj_a", 3))
	got, err := st.CursorsErr()
	if err != nil || got["pj_a"] != 3 {
		t.Fatalf("CursorsErr = %v, %v", got, err)
	}
	st.Close()
	if got, err := st.CursorsErr(); err == nil || got != nil {
		t.Fatalf("closed store: CursorsErr = %v, %v; want an error", got, err)
	}
	if got := st.Cursors(); len(got) != 0 {
		t.Fatalf("closed store: Cursors = %v; want empty", got)
	}
}
