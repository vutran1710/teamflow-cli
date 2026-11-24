package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	_, _, _, _, _ = hook.ProjectKey(bnd, "pj_a")
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

// bnd is a sign-in for the tests that don't care which.
var bnd = Binding("https://tf.example", "lan@acme.test")

// Binding lower-cases the URL's scheme and host (not its path) and the email, and drops a trailing slash.
func TestBindingNormalises(t *testing.T) {
	want := KeyBinding{Server: "https://a.example", Account: "lan@acme.test"}
	for _, s := range []string{"https://A.example", "https://a.example/", "HTTPS://A.EXAMPLE/", " https://a.example "} {
		if got := Binding(s, "LAN@acme.test"); got != want {
			t.Errorf("Binding(%q) = %+v", s, got)
		}
	}
	if got := NormalizeServer("https://A.example/Team/"); got != "https://a.example/Team" {
		t.Errorf("the path keeps its case: %q", got)
	}
	if got := NormalizeServer("http://LOCALHOST:8080"); got != "http://localhost:8080" {
		t.Errorf("port: %q", got)
	}
	for in, want := range map[string]string{
		"":                                 "",
		"   ":                              "",
		"teamflow.example":                 "teamflow.example",   // no scheme: kept as written (api refuses it)
		"Teamflow.Example/":                "Teamflow.Example",   // no scheme: not a URL to lower-case
		"::not a url::":                    "::not a url::",      // garbage: kept, never matches a real server
		"https://":                         "https:",             // no host
		"https://user:pw@A.example/":       "https://a.example",  // userinfo is never part of the binding
		"https://tfa_secret@a.example":     "https://a.example",  // nor a key pasted as userinfo
		"https://[::1]:8443/":              "https://[::1]:8443", // IPv6 with a port
		"https://[FE80::1]/":               "https://[fe80::1]",  // IPv6 lower-cased
		"https://A.example:443/":           "https://a.example:443",
		"HTTPS://A.Example/":               "https://a.example", // mixed case and a trailing slash
		"https://a.example///":             "https://a.example",
		"https://a.example/?x=1#f":         "https://a.example",
		"https://A.example/Team/?q=Upper/": "https://a.example/Team",
	} {
		if got := NormalizeServer(in); got != want {
			t.Errorf("NormalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
	if b := Binding("https://user:pw@A.example/", "lan@acme.test"); strings.Contains(b.Server, "pw") {
		t.Fatalf("userinfo in a binding: %+v", b)
	}
}

// An empty binding (no sign-in) reaches no key, not even rows migrated from before bindings.
func TestEmptyBindingIsRefused(t *testing.T) {
	st := open(t, t.TempDir())
	for _, b := range []KeyBinding{{}, {Server: "https://a.example"}, {Account: "lan@acme.test"}} {
		if err := st.PutProjectKey(b, "pj_a", "tfk", "mb", "og", "n", t0); !errors.Is(err, ErrNoBinding) {
			t.Fatalf("PutProjectKey(%+v): %v", b, err)
		}
		if _, _, _, _, ok := st.ProjectKey(b, "pj_a"); ok {
			t.Fatalf("ProjectKey(%+v)", b)
		}
		if _, err := st.KeyProjects(b); !errors.Is(err, ErrNoBinding) {
			t.Fatalf("KeyProjects(%+v): %v", b, err)
		}
		if err := st.DropProjectKey(b, "pj_a"); !errors.Is(err, ErrNoBinding) {
			t.Fatalf("DropProjectKey(%+v): %v", b, err)
		}
	}
	// A real key is untouched by an empty binding's drop, and an empty binding inside a transaction is refused too.
	must(t, st.PutProjectKey(bnd, "pj_a", "tfk", "mb", "og", "n", t0))
	_ = st.DropProjectKey(KeyBinding{}, "pj_a")
	if _, _, _, _, ok := st.ProjectKey(bnd, "pj_a"); !ok {
		t.Fatal("an empty binding's drop removed a real key")
	}
	err := st.Tx(func(tx *Tx) error { return tx.PutProjectKey(KeyBinding{}, "pj_b", "k", "m", "o", "n", t0) })
	if !errors.Is(err, ErrNoBinding) {
		t.Fatalf("Tx PutProjectKey with no binding: %v", err)
	}
}

// A store from before bindings (0001 only) holding project keys, odd ones included (a damaged or hand-made table
// without the constraints: NULL columns, the same project twice, an empty key): it migrates, the old rows are
// dropped (no binding could ever reach them), and nothing is returned under a real sign-in or an empty one.
func TestPreBindingKeysAreNeverReturned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table string // the 0001-era project_keys, as found
		rows  []string
	}{
		{"as 0001 made it", "", []string{
			"('pj_old', 'tfk_old', 'mb_1', 'og_1', 'Old', '2026-11-01T00:00:00Z')",
			"('pj_two', 'tfk_two', 'mb_1', 'og_1', 'Two', '2026-11-02T00:00:00Z')"}},
		{"without constraints: NULLs, duplicates, empty values",
			"CREATE TABLE project_keys (project TEXT, key TEXT, member TEXT, org TEXT, name TEXT, fetched_at TEXT)",
			[]string{
				"('pj_old', 'tfk_old', 'mb_1', 'og_1', 'Old', '2026-11-01T00:00:00Z')",
				"('pj_old', 'tfk_dup', 'mb_2', 'og_1', 'Old again', '2026-11-03T00:00:00Z')",
				"(NULL, 'tfk_null_project', 'mb_1', 'og_1', 'x', '2026-11-01T00:00:00Z')",
				"('pj_nulls', NULL, NULL, NULL, NULL, NULL)",
				"('', '', '', '', '', '')"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, FileName))
			must(t, err)
			initSQL, err := os.ReadFile(filepath.Join("migrations", "0001_init.sql"))
			must(t, err)
			_, err = db.Exec(string(initSQL))
			must(t, err)
			stmts := []string{
				"CREATE TABLE " + migrationsTable + " (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)",
				"INSERT INTO " + migrationsTable + " VALUES ('0001_init.sql', '2026-11-01T00:00:00Z')",
			}
			if tc.table != "" {
				stmts = append(stmts, "DROP TABLE project_keys", tc.table)
			}
			for _, r := range tc.rows {
				stmts = append(stmts, "INSERT INTO project_keys VALUES "+r)
			}
			for _, q := range stmts {
				_, err = db.Exec(q)
				must(t, err)
			}
			must(t, db.Close())

			st := open(t, dir) // migrates; must not fail on what it finds
			var n int
			must(t, st.db.QueryRow("SELECT count(*) FROM project_keys").Scan(&n))
			if n != 0 {
				t.Fatalf("%d pre-binding rows kept", n)
			}
			for _, b := range []KeyBinding{bnd, Binding("https://other.example", "x@y"), {}, {Server: "", Account: ""}} {
				for _, p := range []string{"pj_old", "pj_two", "pj_nulls", ""} {
					if k, _, _, _, ok := st.ProjectKey(b, p); ok {
						t.Fatalf("old key %q for %q under %+v", k, p, b)
					}
				}
				if ps, _ := st.KeyProjects(b); len(ps) != 0 {
					t.Fatalf("old keys under %+v: %v", b, ps)
				}
			}
			// The migrated table works: a key under a real binding round-trips.
			must(t, st.PutProjectKey(bnd, "pj_old", "tfk_new", "mb_1", "og_1", "Old", t0))
			if k, _, _, _, ok := st.ProjectKey(bnd, "pj_old"); !ok || k != "tfk_new" {
				t.Fatalf("after migrating: %q %v", k, ok)
			}
		})
	}
}

// A project key belongs to the server and account that fetched it: under another config it is invisible, and a
// stale process writing one for its old sign-in never makes it usable under the new one.
func TestProjectKeysAreBoundToServerAndAccount(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	a := Binding("https://a.example/", "Lan@Acme.test")
	b := Binding("https://b.example", "lan@acme.test")
	if a != Binding("https://A.example", "lan@acme.test") {
		t.Fatalf("Binding normalises: %+v", a)
	}
	must(t, st.PutProjectKey(a, "pj_a", "tfk_a", "mb_1", "org_1", "Alpha", t0))
	if _, _, _, _, ok := st.ProjectKey(b, "pj_a"); ok {
		t.Fatal("server A's key is visible under server B")
	}
	if ps, _ := st.KeyProjects(b); len(ps) != 0 {
		t.Fatalf("KeyProjects under B: %v", ps)
	}
	other := Binding("https://a.example", "mai@acme.test")
	if _, _, _, _, ok := st.ProjectKey(other, "pj_a"); ok {
		t.Fatal("account lan's key is visible to account mai")
	}
	if k, _, _, _, ok := st.ProjectKey(a, "pj_a"); !ok || k != "tfk_a" {
		t.Fatal("the key under its own binding")
	}
	// The new sign-in fetches its own key; a stale process (old config) then writes A's key again: B keeps its own.
	must(t, st.PutProjectKey(b, "pj_a", "tfk_b", "mb_9", "org_9", "Alpha", t0.Add(time.Minute)))
	stale := open(t, dir)
	must(t, stale.PutProjectKey(a, "pj_a", "tfk_a", "mb_1", "org_1", "Alpha", t0.Add(2*time.Minute)))
	if k, _, _, _, ok := st.ProjectKey(b, "pj_a"); !ok || k != "tfk_b" {
		t.Fatalf("under B: %q %v; a stale process must not replace or reach B's key", k, ok)
	}
	if ps, _ := st.KeyProjects(b); len(ps) != 1 {
		t.Fatalf("KeyProjects under B: %v", ps)
	}
	// Dropping B's key (NOT_A_MEMBER under B) leaves A's row alone and B sees none.
	must(t, st.DropProjectKey(b, "pj_a"))
	if _, _, _, _, ok := st.ProjectKey(b, "pj_a"); ok {
		t.Fatal("dropped")
	}
}

func TestReadsOnAClosedStoreAnswerEmpty(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SetCursor("pj_a", 1))
	must(t, st.PutProjectKey(bnd, "pj_a", "tfk_secret", "mb_1", "org_1", "Alpha", t0))
	st.Close()
	if got := st.Cursors(); len(got) != 0 {
		t.Fatalf("closed store cursors %v", got)
	}
	if _, _, _, _, ok := st.ProjectKey(bnd, "pj_a"); ok {
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
	if _, _, _, _, ok := st.ProjectKey(bnd, "pj_a"); ok {
		t.Fatal("a key before any was put")
	}
	must(t, st.PutProjectKey(bnd, "pj_b", "tfk_b", "mb_1", "org_1", "Beta", t0))
	must(t, st.PutProjectKey(bnd, "pj_a", "tfk_a", "mb_1", "org_1", "Alpha", t0.Add(time.Minute)))
	must(t, st.PutProjectKey(bnd, "pj_a", "tfk_a2", "mb_2", "org_1", "Alpha 2", t0.Add(2*time.Minute)))
	key, member, org, name, ok := st.ProjectKey(bnd, "pj_a")
	if !ok || key != "tfk_a2" || member != "mb_2" || org != "org_1" || name != "Alpha 2" {
		t.Fatalf("ProjectKey = %q %q %q %q %v", key, member, org, name, ok)
	}
	ps, err := st.KeyProjects(bnd)
	must(t, err)
	if !reflect.DeepEqual(ps, []string{"pj_b", "pj_a"}) {
		t.Fatalf("KeyProjects %v, want in fetch order", ps)
	}
	must(t, st.DropProjectKey(bnd, "pj_a"))
	if _, _, _, _, ok := st.ProjectKey(bnd, "pj_a"); ok {
		t.Fatal("dropped key still there")
	}
	must(t, st.DropProjectKey(bnd, "pj_zzz")) // dropping a missing key is fine
	must(t, st.DropProjectKeys())
	if ps, _ := st.KeyProjects(bnd); len(ps) != 0 {
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
	must(t, st.PutProjectKey(bnd, p, "tfk_"+p, "mb_1", "org_1", p, t0))
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
	if _, _, _, _, ok := st.ProjectKey(bnd, "pj_a"); !ok {
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
	must(t, st.SpoolAdd("sg_1", "ss_1", obA, `{"a":1}`, t0))
	must(t, st.SpoolAdd("sg_2", "ss_1", obB, `{"a":2}`, t0))
	must(t, st.SpoolAdd("sg_3", "ss_2", obA, `{"a":3}`, t0))
	must(t, st.SpoolAdd("sg_1", "ss_9", obA, `{"changed":1}`, t0)) // the same signal again: kept once, as first
	items, err := st.Spooled(obA)
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
	if items, _ := st.Spooled(OutboxOf(bnd, "pj_none")); len(items) != 0 {
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

	got, ok, err := st.SessionOfPIDs([]int{555, 100}, nil)
	must(t, err)
	if !ok || got.ID != "ss_2" {
		t.Fatalf("SessionOfPIDs = %+v %v, want the latest in pid 100", got, ok)
	}
	if _, ok, _ := st.SessionOfPIDs([]int{555}, nil); ok {
		t.Fatal("a session for an unknown pid")
	}
	if got.UpdatedAt.IsZero() || !got.UpdatedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("UpdatedAt %v", got.UpdatedAt)
	}
	// A session the caller finds stale (its pid now belongs to another process) is skipped for the next one.
	got, ok, err = st.SessionOfPIDs([]int{555, 100}, func(s Session) bool { return s.ID != "ss_2" })
	must(t, err)
	if !ok || got.ID != "ss_1" {
		t.Fatalf("skipping the stale session: %+v %v", got, ok)
	}
	if _, ok, _ := st.SessionOfPIDs([]int{100}, func(Session) bool { return false }); ok {
		t.Fatal("every session stale: none")
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
	if _, _, _, _, ok := st.ProjectKey(bnd, "pj_old"); ok {
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

// A process killed mid-run leaves store.db with a -wal (and -shm) holding committed writes not yet checkpointed.
// The next process opens it and finds every key.
func TestKeysSurviveAKilledProcessWAL(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	if _, err := st.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	must(t, st.PutProjectKey(bnd, "pj_a", "tfk_a", "mb_1", "og_1", "Alpha", t0))
	must(t, st.PutProjectKey(bnd, "pj_b", "tfk_b", "mb_1", "og_1", "Beta", t0))
	wal, err := os.ReadFile(filepath.Join(dir, FileName+"-wal"))
	must(t, err)
	if len(wal) == 0 {
		t.Fatal("the writes should still be in the WAL")
	}
	// The "killed" process's files, copied while it still holds them (no close, no checkpoint).
	crashed := t.TempDir()
	for _, f := range []string{FileName, FileName + "-wal", FileName + "-shm"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(crashed, f), b, 0o600))
	}
	again := open(t, crashed)
	if k, _, _, _, ok := again.ProjectKey(bnd, "pj_a"); !ok || k != "tfk_a" {
		t.Fatalf("pj_a after the crash: %q %v", k, ok)
	}
	if ps, _ := again.KeyProjects(bnd); len(ps) != 2 {
		t.Fatalf("keys after the crash: %v", ps)
	}
}

// ClaimNotices hands each unshown notice out once, even to callers racing on their own connections (two MCP calls
// at once, a hook beside them): the claim and the mark are one statement.
func TestClaimNoticesHandsEachOutOnce(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	must(t, st.Notice("ss_1", protocol.NoticeTypeApprovalRequested, "sg_1", "approve sg_1", t0))
	must(t, st.Notice("ss_1", protocol.NoticeTypeDriftFound, "dr_1", "drift", t0))
	must(t, st.Notice("ss_1", protocol.NoticeTypeTopicDeleted, "tp_1", "topic gone", t0))
	must(t, st.Notice("ss_2", protocol.NoticeTypeDriftFound, "dr_1", "other session", t0))
	got, err := st.ClaimNotices("ss_1", true)
	must(t, err)
	if len(got) != 1 || got[0].About != "sg_1" {
		t.Fatalf("approval claim %+v", got)
	}
	got, err = st.ClaimNotices("ss_1", false)
	must(t, err)
	if len(got) != 2 || got[0].Text != "drift" || got[1].Text != "topic gone" {
		t.Fatalf("other claim (in order) %+v", got)
	}
	if again, _ := st.ClaimNotices("ss_1", false); len(again) != 0 {
		t.Fatalf("claimed twice %+v", again)
	}
	if all, _ := st.Notices("ss_1", false); len(all) != 3 {
		t.Fatalf("a claimed notice stays in the inbox: %+v", all)
	}

	// Racing claimers on separate stores (connections): every notice exactly once.
	for i := range 20 {
		must(t, st.Notice("ss_3", protocol.NoticeTypeDriftFound, fmt.Sprint("dr_", i), fmt.Sprint("n", i), t0))
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(dir)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			for range 5 {
				ns, err := s.ClaimNotices("ss_3", false)
				if err != nil && !errors.Is(err, ErrBusy) {
					t.Error(err)
				}
				mu.Lock()
				for _, n := range ns {
					seen[n.Text]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	rest, err := st.ClaimNotices("ss_3", false)
	must(t, err)
	for _, n := range rest {
		seen[n.Text]++
	}
	if len(seen) != 20 {
		t.Fatalf("claimed %d of 20", len(seen))
	}
	for text, n := range seen {
		if n != 1 {
			t.Fatalf("%s claimed %d times", text, n)
		}
	}
}

// ClaimHookSync: one claim per window; an offline mark in force refuses; a clock set back never keeps the hook off
// the network (a last sync in the future, an offline mark further ahead than offlineFor, are stale).
// Sad paths: a claim inside the window; an offline mark in force; the clock set back after each.
func TestClaimHookSyncClockSetBack(t *testing.T) {
	st := open(t, t.TempDir())
	every, off := 10*time.Second, 30*time.Second
	claim := func(at time.Time) bool {
		t.Helper()
		ok, err := st.ClaimHookSync("pj_a", at, every, off)
		must(t, err)
		return ok
	}
	if !claim(t0) || claim(t0.Add(5*time.Second)) {
		t.Fatal("not one claim per window")
	}
	if !claim(t0.Add(-time.Hour)) {
		t.Fatal("a last sync in the future refused the claim")
	}
	must(t, st.SetHookOffline("pj_a", t0.Add(off)))
	if claim(t0.Add(15 * time.Second)) {
		t.Fatal("claimed while marked offline")
	}
	if !claim(t0.Add(-time.Hour + 15*time.Second)) {
		t.Fatal("an offline mark far ahead refused the claim")
	}
}

// A transaction's Store view: reads see the transaction's own writes, its Tx runs in the same transaction (no
// second BEGIN, which would wait on itself), its Close leaves the store open; a failure rolls the whole back.
// Sad paths: a nested Tx; Close on the view; an error inside the nested Tx.
func TestTxStoreView(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.Tx(func(tx *Tx) error {
		v := tx.Store()
		must(t, v.SetLiveAt("pj_a", t0))
		if got := v.LiveAt("pj_a"); !got.Equal(t0) {
			t.Fatalf("the view does not see its own write: %v", got)
		}
		must(t, v.Tx(func(in *Tx) error { return in.SetLiveAt("pj_b", t0) }))
		return v.Close()
	}))
	if st.LiveAt("pj_b").IsZero() {
		t.Fatal("the nested write was lost")
	}
	boom := errors.New("boom")
	err := st.Tx(func(tx *Tx) error {
		v := tx.Store()
		must(t, v.SetLiveAt("pj_c", t0))
		return v.Tx(func(*Tx) error { return boom })
	})
	if !errors.Is(err, boom) || !st.LiveAt("pj_c").IsZero() {
		t.Fatalf("not rolled back: %v", err)
	}
}

var (
	obA     = OutboxOf(bnd, "pj_a")
	obB     = OutboxOf(bnd, "pj_b")
	obOther = OutboxOf(Binding("https://tf.example", "huy@acme.test"), "pj_a")
)

// The outbox is one per (server, account, project): every item keeps its method and its place, and a human act's
// place says how many wait ahead of it. Sad paths: the same item twice (kept once, in its first place), another
// project's and another account's items (never listed nor counted), an item not queued, a closed store.
func TestOutboxKeepsMethodAndPlace(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SpoolAdd("sg_1", "ss_1", obA, `{"a":1}`, t0))
	must(t, st.OutboxAdd("dra_1", "drift.resolve", "ss_1", obA, `{"drift":"dr_1"}`, t0))
	must(t, st.SpoolAdd("sg_x", "ss_1", obB, `{"a":9}`, t0))
	must(t, st.SpoolAdd("sg_y", "ss_1", obOther, `{"a":8}`, t0))
	must(t, st.OutboxAdd("sg_2", "signal.emit", "", obA, `{"a":2}`, t0))
	must(t, st.OutboxAdd("dra_1", "drift.dismiss", "ss_9", obA, `{}`, t0)) // again: kept once, as first
	items, err := st.Spooled(obA)
	must(t, err)
	var got []string
	for _, it := range items {
		got = append(got, it.Signal+":"+it.Method)
	}
	if strings.Join(got, ",") != "sg_1:signal.emit,dra_1:drift.resolve,sg_2:signal.emit" {
		t.Fatalf("outbox %v", got)
	}
	for id, want := range map[string]int{"sg_1": 0, "dra_1": 1, "sg_2": 2} {
		if n, ok, err := st.Ahead(obA, id); err != nil || !ok || n != want {
			t.Fatalf("Ahead(%s) = %d %v %v, want %d", id, n, ok, err, want)
		}
	}
	if _, ok, err := st.Ahead(obA, "sg_missing"); ok || err != nil {
		t.Fatalf("an item not queued: %v %v", ok, err)
	}
	if _, ok, _ := st.Ahead(obA, "sg_y"); ok {
		t.Fatal("another account's item counted as this outbox's")
	}
	if n, _ := st.Waiting(obA); n != 3 {
		t.Fatalf("Waiting %d", n)
	}
	if ps, _ := st.OutboxProjects(bnd); strings.Join(ps, ",") != "pj_a,pj_b" {
		t.Fatalf("OutboxProjects %v", ps)
	}
	all, err := st.OutboxAll()
	must(t, err)
	if len(all) != 5 || all[3].Outbox != obOther || all[3].Signal != "sg_y" {
		t.Fatalf("OutboxAll %+v", all)
	}
	if obA.Ref() == obB.Ref() || obA.Ref() == obOther.Ref() || len(obA.Ref()) != 16 {
		t.Fatalf("refs %s %s %s", obA.Ref(), obB.Ref(), obOther.Ref())
	}
	st.Close()
	if _, _, err := st.Ahead(obA, "sg_1"); err == nil {
		t.Fatal("a closed store answered Ahead")
	}
	if _, err := st.Waiting(obA); err == nil {
		t.Fatal("a closed store answered Waiting")
	}
}

// Each send and each counted failure is recorded on the item: tries, fails, the first failure's time, the last
// error. Sad paths: an unknown id (nothing happens), a second failure keeps the first time.
func TestOutboxCountsTriesAndFailures(t *testing.T) {
	st := open(t, t.TempDir())
	must(t, st.SpoolAdd("sg_1", "ss_1", obA, `{}`, t0))
	must(t, st.SpoolAdd("sg_2", "ss_1", obA, `{}`, t0))
	must(t, st.Tried([]string{"sg_1", "sg_2", "sg_missing"}))
	must(t, st.Tried([]string{"sg_1"}))
	it, err := st.Failed("sg_1", "no answer", t0.Add(time.Hour))
	must(t, err)
	it, err = st.Failed("sg_1", "server error 500", t0.Add(2*time.Hour))
	must(t, err)
	if it.Tries != 2 || it.Fails != 2 || !it.FirstFail.Equal(t0.Add(time.Hour)) || it.LastError != "server error 500" {
		t.Fatalf("item %+v", it)
	}
	if _, err := st.Failed("sg_missing", "x", t0); err == nil {
		t.Fatal("a failure of an item not queued")
	}
	items, _ := st.Spooled(obA)
	if items[1].Tries != 1 || items[1].Fails != 0 || !items[1].FirstFail.IsZero() || !items[1].CreatedAt.Equal(t0) {
		t.Fatalf("untouched item %+v", items[1])
	}
}

// A spool written before the outbox knew methods and sign-ins (0001..0003) migrates: its rows are bound to the
// sign-in and project of the key they were queued with (the key still on this machine), in their order. A row
// whose key is gone keeps its project (from the signal) and no sign-in: it is never sent by anyone, but listed.
func TestSpoolFromBeforeMigrates(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, FileName))
	must(t, err)
	stmts := []string{"CREATE TABLE " + migrationsTable + " (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"}
	for _, n := range []string{"0001_init.sql", "0002_project_key_binding.sql", "0003_outbox_method.sql"} {
		body, err := os.ReadFile(filepath.Join("migrations", n))
		must(t, err)
		stmts = append(stmts, string(body), "INSERT INTO "+migrationsTable+" VALUES ('"+n+"', '2026-11-01T00:00:00Z')")
	}
	known := "0b1d94f300d754c6" // the key ref of tfp_example_key (as Python's key_ref)
	stmts = append(stmts,
		"INSERT INTO project_keys VALUES ('"+bnd.Server+"', '"+bnd.Account+"', 'pj_a', 'tfp_example_key', 'mb_1', 'og_1', 'A', '2026-11-01T00:00:00Z')",
		"INSERT INTO spool (signal, session, key_ref, params, created_at) VALUES ('sg_old1', 'ss_1', '"+known+"', '{\"signal\":{\"project\":\"pj_a\"}}', '2026-11-01T00:00:00Z')",
		"INSERT INTO spool (signal, session, key_ref, params, created_at) VALUES ('sg_gone', 'ss_1', 'ffffffffffffffff', '{\"signal\":{\"project\":\"pj_z\"}}', '2026-11-01T00:00:00Z')",
		"INSERT INTO spool (signal, session, key_ref, params, created_at) VALUES ('sg_old2', 'ss_1', '"+known+"', '{not json', '2026-11-01T00:00:00Z')")
	for _, q := range stmts {
		_, err = db.Exec(q)
		must(t, err)
	}
	must(t, db.Close())
	st := open(t, dir)
	must(t, st.OutboxAdd("dra_new", "drift.dismiss", "ss_1", obA, "{}", t0))
	items, err := st.Spooled(obA)
	must(t, err)
	if len(items) != 3 || items[0].Signal != "sg_old1" || items[0].Method != "signal.emit" || items[1].Signal != "sg_old2" ||
		items[2].Method != "drift.dismiss" {
		t.Fatalf("migrated outbox %+v", items)
	}
	all, _ := st.OutboxAll()
	var gone *OutboxRow
	for i := range all {
		if all[i].Signal == "sg_gone" {
			gone = &all[i]
		}
	}
	if gone == nil || gone.Project != "pj_z" || gone.Server != "" || gone.Account != "" {
		t.Fatalf("an orphan from before: %+v", gone)
	}
	st.Close()
	st = open(t, dir) // opening again changes nothing
	if items, _ := st.Spooled(obA); len(items) != 3 {
		t.Fatalf("after reopening %+v", items)
	}
}
