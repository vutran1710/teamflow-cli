// Package store is the client store: one SQLite file (store.db, mode 0600) in the home folder with the
// replica of the member's projects, the spool, the agent sessions, citations, notices and the project keys.
// All of the client's SQL lives here; the schema is in migrations/NNNN_*.sql.
//
// Several processes share the file: the background sync writes while hooks and the CLI read. The
// file is in WAL mode, so a reader never waits for a writer: it reads the last committed state. A writer waits for
// another writer at most 250 ms (busy_timeout) and then gets ErrBusy, so a hook answers from what it has rather
// than hold up the agent's step.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// FileName is the store's file in the home folder.
const FileName = "store.db"

// LegacyFileName is the Python client's store. The Go client starts fresh: it never opens or changes that file.
const LegacyFileName = "client.sqlite"

// migrationsTable records the applied migrations (its own name, apart from the Python client's schema_migrations).
const migrationsTable = "go_schema_migrations"

// LegacyPresent says whether dir holds a store left by the Python client (teamflow status mentions it).
func LegacyPresent(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, LegacyFileName))
	return err == nil && fi.Mode().IsRegular()
}

// BusyTimeout is how long a writer waits for another writer before ErrBusy.
const BusyTimeout = 250 * time.Millisecond

// UpdateMessage is what the person is told when the server refused this client's protocol (UPGRADE_REQUIRED).
const UpdateMessage = "update teamflow (`teamflow self-update`, or download the new release)"

// ErrBusy means another process held the store for longer than BusyTimeout. The caller answers from what it has.
var ErrBusy = errors.New("teamflow store busy")

//go:embed migrations/*.sql
var migrations embed.FS

// timeFormat is fixed-width UTC, so stored times sort as text.
const timeFormat = "2006-01-02T15:04:05.000000Z07:00"

func ts(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTS(s string) time.Time {
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// querier is what both the store and a transaction run SQL with.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ops holds the accessors; Store runs each on its own, Tx runs them in one transaction.
type ops struct{ q querier }

// Store is an open client store.
type Store struct {
	ops
	db   *sql.DB
	path string
	in   *Tx // a view of a transaction (Tx.Store): every accessor runs in it
}

// Tx is a write transaction (BEGIN IMMEDIATE): every accessor of Store, all committed together.
type Tx struct{ ops }

// Store is a view of the transaction as a Store, for code written against a Store: every read and write runs in
// the transaction (one snapshot, no other writer committing meanwhile), its Tx runs fn in this same transaction,
// and its Close does nothing.
func (t *Tx) Store() *Store { return &Store{ops: t.ops, in: t} }

// Open opens (creating and migrating when needed) the store in dir: dir/store.db, WAL, busy_timeout 250 ms,
// mode 0600. The folder is created 0700 when missing; guarding an existing one is home.Ensure's job.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store folder: %w", err)
	}
	path := filepath.Join(dir, FileName)
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("store path must not contain ? or #")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}
	_ = f.Close()
	if err := ownerOnly(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(" + strconv.Itoa(int(BusyTimeout/time.Millisecond)) + ")" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", wrap(err))
	}
	st := &Store{ops: ops{q: db}, db: db, path: path}
	if err := st.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ownerOnly(path); err != nil { // the -wal and -shm files exist now
		_ = db.Close()
		return nil, err
	}
	_ = st.adoptLegacy() // best effort: a busy store tries again at the next open; a hook never fails on it
	return st, nil
}

// ownerOnly sets the store file and its WAL companions to 0600.
func ownerOnly(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store file mode: %w", err)
		}
	}
	return nil
}

// Close closes the store. Reads after it answer empty; writes fail.
func (s *Store) Close() error {
	if s.in != nil {
		return nil
	}
	return s.db.Close()
}

// Tx runs fn in one write transaction: committed when fn returns nil, rolled back on an error or a panic.
func (s *Store) Tx(fn func(*Tx) error) (err error) {
	if s.in != nil {
		return fn(s.in)
	}
	sqlTx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return wrap(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = sqlTx.Rollback()
		}
	}()
	if err := fn(&Tx{ops{q: sqlTx}}); err != nil {
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return wrap(err)
	}
	committed = true
	return nil
}

// wrap marks SQLITE_BUSY and SQLITE_LOCKED as ErrBusy.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return fmt.Errorf("%w: %v", ErrBusy, err)
		}
	}
	return err
}

func (s *Store) migrate() error {
	ents, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	// Read first: once migrated (the usual case: every hook opens the store), opening takes no write lock.
	done, err := s.applied()
	if err != nil {
		return err
	}
	pending := false
	for _, n := range names {
		if !done[n] {
			pending = true
		}
	}
	if !pending {
		return nil
	}
	return s.Tx(func(tx *Tx) error {
		if _, err := tx.exec("CREATE TABLE IF NOT EXISTS " + migrationsTable + " (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
			return err
		}
		done, err := tx.applied() // another process may have migrated meanwhile
		if err != nil {
			return err
		}
		for _, n := range names {
			if done[n] {
				continue
			}
			body, err := migrations.ReadFile("migrations/" + n)
			if err != nil {
				return err
			}
			if _, err := tx.exec(string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", n, err)
			}
			if _, err := tx.exec("INSERT INTO "+migrationsTable+" VALUES (?, ?)", n, ts(time.Now())); err != nil {
				return err
			}
		}
		return nil
	})
}

func (o ops) applied() (map[string]bool, error) {
	done := map[string]bool{}
	var n int
	if err := o.row("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", migrationsTable).Scan(&n); err != nil {
		return nil, wrap(err)
	}
	if n == 0 {
		return done, nil
	}
	err := o.each("SELECT name FROM "+migrationsTable, nil, func(r *sql.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		done[name] = true
		return nil
	})
	return done, err
}

// ------------------------------------------------------------------ helpers

func (o ops) exec(q string, args ...any) (sql.Result, error) {
	r, err := o.q.ExecContext(context.Background(), q, args...)
	return r, wrap(err)
}

func (o ops) row(q string, args ...any) *sql.Row {
	return o.q.QueryRowContext(context.Background(), q, args...)
}

// one scans one row; ok is false when there is none.
func (o ops) one(q string, args []any, dest ...any) (bool, error) {
	err := o.row(q, args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrap(err)
	}
	return true, nil
}

func (o ops) each(q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := o.q.QueryContext(context.Background(), q, args...)
	if err != nil {
		return wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return wrap(err)
		}
	}
	return wrap(rows.Err())
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func asJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// getJSON reads one data column into a value; ok is false when there is no row.
func getJSON[T any](o ops, q string, args ...any) (T, bool, error) {
	var zero T
	var data string
	ok, err := o.one(q, args, &data)
	if !ok || err != nil {
		return zero, false, err
	}
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return zero, false, fmt.Errorf("store: bad stored record: %w", err)
	}
	return v, true, nil
}

// listJSON reads the data column of every row.
func listJSON[T any](o ops, q string, args ...any) ([]T, error) {
	var out []T
	err := o.each(q, args, func(r *sql.Rows) error {
		var data string
		if err := r.Scan(&data); err != nil {
			return err
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return fmt.Errorf("store: bad stored record: %w", err)
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// ------------------------------------------------------------------ sessions

// Session is an agent session on this machine.
type Session struct {
	ID           string
	Agent        string
	AgentPID     int // 0: unknown
	Cwd          string
	Project      string // "" while not shared
	Member       string
	KeyRef       string
	LastSeq      int64
	StateVersion int64
	Topics       []string  // the topics followed; nil: all of them (an empty list follows none)
	UpdatedAt    time.Time // when the session was last seen
}

// Follows says whether the session follows a topic; the project's system topics always come along.
func (s Session) Follows(topic string, system map[string]bool) bool {
	if s.Topics == nil || system[topic] {
		return true
	}
	for _, t := range s.Topics {
		if t == topic {
			return true
		}
	}
	return false
}

const sessionCols = "id, agent, agent_pid, cwd, project, member, key_ref, last_seq, state_version, topics, updated_at"

func scanSession(r interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var pid sql.NullInt64
	var project, member, keyRef, topics sql.NullString
	var updated string
	if err := r.Scan(&s.ID, &s.Agent, &pid, &s.Cwd, &project, &member, &keyRef, &s.LastSeq, &s.StateVersion, &topics,
		&updated); err != nil {
		return Session{}, err
	}
	s.UpdatedAt = parseTS(updated)
	s.AgentPID, s.Project, s.Member, s.KeyRef = int(pid.Int64), project.String, member.String, keyRef.String
	if topics.Valid && topics.String != "" {
		s.Topics = []string{}
		if err := json.Unmarshal([]byte(topics.String), &s.Topics); err != nil {
			return Session{}, fmt.Errorf("store: bad session topics: %w", err)
		}
	}
	return s, nil
}

func (o ops) sessionWhere(where string, args ...any) (Session, bool, error) {
	s, err := scanSession(o.row("SELECT "+sessionCols+" FROM sessions WHERE "+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, wrap(err)
	}
	return s, true, nil
}

func (o ops) sessionsWhere(where string, args ...any) ([]Session, error) {
	var out []Session
	err := o.each("SELECT "+sessionCols+" FROM sessions WHERE "+where, args, func(r *sql.Rows) error {
		s, err := scanSession(r)
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// UpsertSession records a session (seen now in process pid, 0 when unknown, at cwd) and returns it.
func (o ops) UpsertSession(id, agent string, pid int, cwd string, now time.Time) (Session, error) {
	var p any
	if pid > 0 {
		p = pid
	}
	if _, err := o.exec("INSERT INTO sessions (id, agent, agent_pid, cwd, started_at, updated_at) VALUES (?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT(id) DO UPDATE SET agent_pid = excluded.agent_pid, cwd = excluded.cwd, updated_at = excluded.updated_at",
		id, agent, p, cwd, ts(now), ts(now)); err != nil {
		return Session{}, err
	}
	s, ok, err := o.Session(id)
	if err == nil && !ok {
		err = fmt.Errorf("store: session %s vanished", id)
	}
	return s, err
}

// Session is the session with this id.
func (o ops) Session(id string) (Session, bool, error) { return o.sessionWhere("id = ?", id) }

// SessionOfPIDs is the session most recently seen in the nearest of these agent processes that live (nil: any)
// accepts: a caller rejects a session whose pid now belongs to another process.
func (o ops) SessionOfPIDs(pids []int, live func(Session) bool) (Session, bool, error) {
	for _, pid := range pids {
		ss, err := o.sessionsWhere("agent_pid = ? ORDER BY updated_at DESC", pid)
		if err != nil {
			return Session{}, false, err
		}
		for _, s := range ss {
			if live == nil || live(s) {
				return s, true, nil
			}
		}
	}
	return Session{}, false, nil
}

// SessionsSince are the sessions seen at or after t (the background sync's quiet exit).
func (o ops) SessionsSince(t time.Time) ([]Session, error) {
	return o.sessionsWhere("updated_at >= ?", ts(t))
}

// PreviousSession is the latest other session seen in process pid.
func (o ops) PreviousSession(pid int, notID string) (Session, bool, error) {
	return o.sessionWhere("agent_pid = ? AND id != ? ORDER BY updated_at DESC LIMIT 1", pid, notID)
}

// Share puts a session on a project as a member ("" project: not shared) following topics (nil: all), from the start.
func (o ops) Share(id, project, member, keyRef string, topics []string) error {
	var t any
	if topics != nil {
		js, err := asJSON(topics)
		if err != nil {
			return err
		}
		t = js
	}
	_, err := o.exec("UPDATE sessions SET project = ?, member = ?, key_ref = ?, topics = ?, last_seq = 0, state_version = 0 "+
		"WHERE id = ?", nullable(project), nullable(member), nullable(keyRef), t, id)
	return err
}

// Seen records the last project event and plan version a session was given.
func (o ops) Seen(id string, lastSeq, stateVersion int64) error {
	_, err := o.exec("UPDATE sessions SET last_seq = ?, state_version = ? WHERE id = ?", lastSeq, stateVersion, id)
	return err
}

// SessionsOn are the sessions shared on a project.
func (o ops) SessionsOn(project string) ([]Session, error) {
	return o.sessionsWhere("project = ? ORDER BY started_at, id", project)
}

// SharedSessions are the sessions shared on any project.
func (o ops) SharedSessions() ([]Session, error) {
	return o.sessionsWhere("project IS NOT NULL ORDER BY started_at, id")
}

// ------------------------------------------------------------------ project keys

// KeyBinding is the sign-in a project key belongs to: the server and the account (email). A key is only ever
// read under the binding it was fetched with, so a key of one server or account never reaches another, even when
// a process still holding the old config writes one after a new sign-in.
type KeyBinding struct{ Server, Account string }

// ErrNoBinding is a project-key read or write with no sign-in (no server or no account): nothing is ever found
// or written that way, not even keys from before bindings (which carry none).
var ErrNoBinding = errors.New("not signed in")

// NormalizeServer is a server URL in one form: scheme and host lower-cased, the path as it is, no trailing slash,
// and no userinfo, query or fragment (none of them names the server; a key pasted as userinfo is never kept). A
// string that is not a URL with a scheme and host is only trimmed: it never matches a real server, and the API
// client refuses it.
func NormalizeServer(server string) string {
	s := strings.TrimSpace(server)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
		u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
		s = u.String()
	}
	return strings.TrimRight(s, "/")
}

// Binding is the binding of a sign-in: the server normalised (NormalizeServer), the email lower-cased.
func Binding(server, email string) KeyBinding {
	return KeyBinding{Server: NormalizeServer(server), Account: strings.ToLower(strings.TrimSpace(email))}
}

func (b KeyBinding) valid() bool { return b.Server != "" && b.Account != "" }

// PutProjectKey records the key this machine fetched for a project under b (replacing an earlier one there).
func (o ops) PutProjectKey(b KeyBinding, project, key, member, org, name string, now time.Time) error {
	if !b.valid() {
		return ErrNoBinding
	}
	_, err := o.exec("INSERT INTO project_keys (server, account, project, key, member, org, name, fetched_at) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(server, account, project) DO UPDATE SET key = excluded.key, "+
		"member = excluded.member, org = excluded.org, name = excluded.name, fetched_at = excluded.fetched_at",
		b.Server, b.Account, project, key, member, org, name, ts(now))
	return err
}

// ProjectKey is the project's key under b and who it acts as; ok is false when there is none (or the store
// can't say).
func (o ops) ProjectKey(b KeyBinding, project string) (key, member, org, name string, ok bool) {
	if !b.valid() {
		return "", "", "", "", false
	}
	ok, err := o.one("SELECT key, member, org, name FROM project_keys WHERE server = ? AND account = ? AND project = ?",
		[]any{b.Server, b.Account, project}, &key, &member, &org, &name)
	if err != nil || !ok {
		return "", "", "", "", false
	}
	return key, member, org, name, true
}

// KeyProjects are the projects with a key under b, in the order they were fetched.
func (o ops) KeyProjects(b KeyBinding) ([]string, error) {
	if !b.valid() {
		return nil, ErrNoBinding
	}
	var out []string
	err := o.each("SELECT project FROM project_keys WHERE server = ? AND account = ? ORDER BY fetched_at, project",
		[]any{b.Server, b.Account}, func(r *sql.Rows) error {
			var p string
			if err := r.Scan(&p); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	return out, err
}

// DropProjectKey forgets a project's key under b.
func (o ops) DropProjectKey(b KeyBinding, project string) error {
	if !b.valid() {
		return ErrNoBinding
	}
	_, err := o.exec("DELETE FROM project_keys WHERE server = ? AND account = ? AND project = ?", b.Server, b.Account, project)
	return err
}

// DropProjectKeys forgets every project key (signing out).
func (o ops) DropProjectKeys() error {
	_, err := o.exec("DELETE FROM project_keys")
	return err
}

// ------------------------------------------------------------------ people

// Person is how a member is shown.
type Person struct{ Name, Alias, Title string }

// PutNames records members' names (keeping their alias and title).
func (o ops) PutNames(names map[string]string) error {
	for id, name := range names {
		if _, err := o.exec("INSERT INTO members (id, name) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name",
			id, name); err != nil {
			return err
		}
	}
	return nil
}

// PutPeople records members' name, alias and org title.
func (o ops) PutPeople(people map[string]Person) error {
	for id, p := range people {
		if _, err := o.exec("INSERT INTO members (id, name, alias, title) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO UPDATE "+
			"SET name = excluded.name, alias = excluded.alias, title = excluded.title", id, p.Name, p.Alias, p.Title); err != nil {
			return err
		}
	}
	return nil
}

// People is member id -> name, alias and org title.
func (o ops) People() (map[string]Person, error) {
	out := map[string]Person{}
	err := o.each("SELECT id, name, alias, title FROM members", nil, func(r *sql.Rows) error {
		var id string
		var p Person
		if err := r.Scan(&id, &p.Name, &p.Alias, &p.Title); err != nil {
			return err
		}
		out[id] = p
		return nil
	})
	return out, err
}

// Names is member id -> name.
func (o ops) Names() (map[string]string, error) {
	ppl, err := o.People()
	out := make(map[string]string, len(ppl))
	for id, p := range ppl {
		out[id] = p.Name
	}
	return out, err
}

// ------------------------------------------------------------------ replica: projects, topics, states

// PutProject records a project.
func (o ops) PutProject(p protocol.Project) error {
	data, err := asJSON(p)
	if err != nil {
		return err
	}
	_, err = o.exec("INSERT INTO projects (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data", p.ID, data)
	return err
}

// Project is a project of the replica.
func (o ops) Project(id string) (protocol.Project, bool, error) {
	return getJSON[protocol.Project](o, "SELECT data FROM projects WHERE id = ?", id)
}

// PutTopic records a topic.
func (o ops) PutTopic(t protocol.Topic) error {
	data, err := asJSON(t)
	if err != nil {
		return err
	}
	_, err = o.exec("INSERT INTO topics (id, project, data) VALUES (?, ?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data",
		t.ID, t.Project, data)
	return err
}

// Topics are a project's topics, in the order first seen.
func (o ops) Topics(project string) ([]protocol.Topic, error) {
	return listJSON[protocol.Topic](o, "SELECT data FROM topics WHERE project = ? ORDER BY rowid", project)
}

// PutState records a version of a project's plan and instruction.
func (o ops) PutState(s protocol.ProjectState) error {
	data, err := asJSON(s)
	if err != nil {
		return err
	}
	_, err = o.exec("INSERT OR REPLACE INTO states (project, version, data) VALUES (?, ?, ?)", s.Project, s.Version, data)
	return err
}

// LatestState is a project's latest plan and instruction.
func (o ops) LatestState(project string) (protocol.ProjectState, bool, error) {
	return getJSON[protocol.ProjectState](o, "SELECT data FROM states WHERE project = ? ORDER BY version DESC LIMIT 1", project)
}

// ------------------------------------------------------------------ replica: signals

// PutSignal records an accepted signal (its change seq is its own seq).
func (o ops) PutSignal(st protocol.StoredSignal) error {
	data, err := asJSON(st)
	if err != nil {
		return err
	}
	s := st.Signal
	_, err = o.exec("INSERT OR REPLACE INTO signals (id, project, topic, kind, seq, chain, chain_version, state, changed_seq, data) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", s.ID, s.Project, s.Topic, string(s.Kind), st.Seq, st.Chain, st.ChainVersion,
		string(st.State), st.Seq, data)
	return err
}

// SetSignalState records a signal's new state, changed at event seq. A signal not in the replica is skipped.
func (o ops) SetSignalState(id string, state protocol.SignalState, seq int64) error {
	st, ok, err := o.Signal(id)
	if !ok || err != nil {
		return err
	}
	st.State = state
	data, err := asJSON(st)
	if err != nil {
		return err
	}
	_, err = o.exec("UPDATE signals SET state = ?, changed_seq = ?, data = ? WHERE id = ?", string(state), seq, data, id)
	return err
}

// Signal is a signal of the replica.
func (o ops) Signal(id string) (protocol.StoredSignal, bool, error) {
	return getJSON[protocol.StoredSignal](o, "SELECT data FROM signals WHERE id = ?", id)
}

// Chain is every version of a chain, in order.
func (o ops) Chain(chain string) ([]protocol.StoredSignal, error) {
	return listJSON[protocol.StoredSignal](o, "SELECT data FROM signals WHERE chain = ? ORDER BY chain_version", chain)
}

// SignalsAfter are a project's signals accepted after seq (the checkpoint view).
func (o ops) SignalsAfter(project string, seq int64) ([]protocol.StoredSignal, error) {
	return listJSON[protocol.StoredSignal](o, "SELECT data FROM signals WHERE project = ? AND seq > ? ORDER BY seq", project, seq)
}

// Changed is a signal accepted or changed after a seq; New when it was accepted after it.
type Changed struct {
	Signal protocol.StoredSignal
	New    bool
}

// ChangedAfter are a project's signals accepted or changed after seq, in change order.
func (o ops) ChangedAfter(project string, seq int64) ([]Changed, error) {
	var out []Changed
	err := o.each("SELECT data, seq FROM signals WHERE project = ? AND changed_seq > ? ORDER BY changed_seq",
		[]any{project, seq}, func(r *sql.Rows) error {
			var data string
			var at int64
			if err := r.Scan(&data, &at); err != nil {
				return err
			}
			var st protocol.StoredSignal
			if err := json.Unmarshal([]byte(data), &st); err != nil {
				return fmt.Errorf("store: bad stored record: %w", err)
			}
			out = append(out, Changed{Signal: st, New: at > seq})
			return nil
		})
	return out, err
}

// PendingFor are the pending signals that ask this member for approval.
func (o ops) PendingFor(member string) ([]protocol.StoredSignal, error) {
	all, err := listJSON[protocol.StoredSignal](o, "SELECT data FROM signals WHERE state = ? ORDER BY seq",
		string(protocol.SignalStatePending))
	var out []protocol.StoredSignal
	for _, s := range all {
		if r := s.Signal.RequestForApproval; r != nil {
			for _, a := range r.Approvers {
				if a == member {
					out = append(out, s)
					break
				}
			}
		}
	}
	return out, err
}

// ActionResults is action id -> its ACTION_RESULT and ACTION_REVOKE signals, in seq order.
func (o ops) ActionResults(project string) (map[string][]protocol.StoredSignal, error) {
	all, err := listJSON[protocol.StoredSignal](o, "SELECT data FROM signals WHERE project = ? AND kind IN (?, ?) ORDER BY seq",
		project, string(protocol.SignalKindActionResult), string(protocol.SignalKindActionRevoke))
	out := map[string][]protocol.StoredSignal{}
	for _, s := range all {
		switch b := s.Signal.Body.(type) {
		case protocol.ActionResultBody:
			out[b.Action] = append(out[b.Action], s)
		case protocol.ActionRevokeBody:
			out[b.Action] = append(out[b.Action], s)
		}
	}
	return out, err
}

// DeleteTopic forgets a deleted topic, its signals and drifts, and what refers to them.
func (o ops) DeleteTopic(topic string, signals, drifts []string) error {
	stmts := [][]any{{"DELETE FROM topics WHERE id = ?", topic}, {"DELETE FROM signals WHERE topic = ?", topic}}
	for _, id := range signals {
		stmts = append(stmts, []any{"DELETE FROM signals WHERE id = ?", id}, []any{"DELETE FROM artifact_archive WHERE chain = ?", id},
			[]any{"DELETE FROM citations WHERE signal = ?", id}, []any{"DELETE FROM notices WHERE about = ?", id})
	}
	for _, id := range drifts {
		stmts = append(stmts, []any{"DELETE FROM drifts WHERE id = ?", id}, []any{"DELETE FROM notices WHERE about = ?", id})
	}
	for _, s := range stmts {
		if _, err := o.exec(s[0].(string), s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ replica: drifts, archive, deliverables

// PutDrift records a drift as of event seq.
func (o ops) PutDrift(d protocol.Drift, seq int64) error {
	data, err := asJSON(d)
	if err != nil {
		return err
	}
	_, err = o.exec("INSERT OR REPLACE INTO drifts (id, project, status, seq, data) VALUES (?, ?, ?, ?, ?)",
		d.ID, d.Project, string(d.Status), seq, data)
	return err
}

// Drifts are a project's drifts with this status ("" for all), in event order.
func (o ops) Drifts(project string, status protocol.DriftStatus) ([]protocol.Drift, error) {
	if status == "" {
		return listJSON[protocol.Drift](o, "SELECT data FROM drifts WHERE project = ? ORDER BY seq", project)
	}
	return listJSON[protocol.Drift](o, "SELECT data FROM drifts WHERE project = ? AND status = ? ORDER BY seq", project, string(status))
}

// SetArchived records whether an artifact chain is archived.
func (o ops) SetArchived(project, chain string, archived bool, note string) error {
	_, err := o.exec("INSERT INTO artifact_archive (chain, project, archived, note) VALUES (?, ?, ?, ?) "+
		"ON CONFLICT(chain) DO UPDATE SET archived = excluded.archived, note = excluded.note", chain, project, archived, note)
	return err
}

// ArchivedChains are a project's archived artifact chains.
func (o ops) ArchivedChains(project string) (map[string]bool, error) {
	out := map[string]bool{}
	err := o.each("SELECT chain FROM artifact_archive WHERE project = ? AND archived = 1", []any{project}, func(r *sql.Rows) error {
		var c string
		if err := r.Scan(&c); err != nil {
			return err
		}
		out[c] = true
		return nil
	})
	return out, err
}

// PutDeliverables records the version published at event seq; signals after it are folded over it when read.
func (o ops) PutDeliverables(d protocol.Deliverables, seq int64) error {
	data, err := asJSON(d)
	if err != nil {
		return err
	}
	_, err = o.exec("INSERT INTO deliverables (project, version, archived, seq, data) VALUES (?, ?, ?, ?, ?) "+
		"ON CONFLICT(project) DO UPDATE SET version = excluded.version, archived = excluded.archived, seq = excluded.seq, "+
		"data = excluded.data", d.Project, d.Version, d.Archived, seq, data)
	return err
}

// Deliverables are a project's Deliverables as last published.
func (o ops) Deliverables(project string) (protocol.Deliverables, bool, error) {
	var archived bool
	var data string
	ok, err := o.one("SELECT archived, data FROM deliverables WHERE project = ?", []any{project}, &archived, &data)
	if !ok || err != nil {
		return protocol.Deliverables{}, false, err
	}
	var d protocol.Deliverables
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return protocol.Deliverables{}, false, fmt.Errorf("store: bad stored record: %w", err)
	}
	d.Archived = archived
	return d, true, nil
}

// DeliverablesSeq is the seq of the event that carried the stored version (0: none).
func (o ops) DeliverablesSeq(project string) (int64, error) {
	var seq int64
	_, err := o.one("SELECT seq FROM deliverables WHERE project = ?", []any{project}, &seq)
	return seq, err
}

// SetDeliverablesArchived archives or restores a project's Deliverables; nothing happens when there are none.
func (o ops) SetDeliverablesArchived(project string, archived bool, note string) error {
	d, ok, err := o.Deliverables(project)
	if !ok || err != nil {
		return err
	}
	d.Archived, d.ArchiveNote = archived, ""
	if archived {
		d.ArchiveNote = note
	}
	data, err := asJSON(d)
	if err != nil {
		return err
	}
	_, err = o.exec("UPDATE deliverables SET archived = ?, data = ? WHERE project = ?", archived, data, project)
	return err
}

// DropDeliverables forgets a project's Deliverables.
func (o ops) DropDeliverables(project string) error {
	_, err := o.exec("DELETE FROM deliverables WHERE project = ?", project)
	return err
}

// DropActionNotices forgets the not yet shown due and revoked notices of a project's sessions.
func (o ops) DropActionNotices(project string) error {
	_, err := o.exec("DELETE FROM notices WHERE shown = 0 AND type IN (?, ?) AND session IN (SELECT id FROM sessions WHERE project = ?)",
		string(protocol.NoticeTypeActionDue), string(protocol.NoticeTypeActionRevoked), project)
	return err
}

// ------------------------------------------------------------------ cursors, reset, live, protocol

// Cursors is project -> last seq synced, for hooks: a store that can't answer gives an empty map.
func (o ops) Cursors() map[string]int64 {
	out, err := o.CursorsErr()
	if err != nil {
		return map[string]int64{}
	}
	return out
}

// CursorsErr is Cursors with the store's error, for the sync (which must not pull from 0 on a busy store).
func (o ops) CursorsErr() (map[string]int64, error) {
	out := map[string]int64{}
	err := o.each("SELECT project, seq FROM cursors", nil, func(r *sql.Rows) error {
		var p string
		var seq int64
		if err := r.Scan(&p, &seq); err != nil {
			return err
		}
		out[p] = seq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetCursor moves a project's cursor forward to seq (never back; ResetProject is how it goes back).
func (o ops) SetCursor(project string, seq int64) error {
	_, err := o.exec("INSERT INTO cursors (project, seq) VALUES (?, ?) ON CONFLICT(project) DO UPDATE SET seq = MAX(seq, excluded.seq)",
		project, seq)
	return err
}

// ResetProject forgets what this machine holds of a project after the server said reset (sync.poll rule): its
// replica rows go and its cursor is 0, so it is pulled again from the start. Its key, people, sessions, citations
// and notices stay; sessions keep their place, but never beyond head, the server's last seq.
func (o ops) ResetProject(project string, head int64) error {
	for _, table := range []string{"topics", "states", "signals", "drifts", "artifact_archive", "deliverables", "cursors", "live"} {
		if _, err := o.exec("DELETE FROM "+table+" WHERE project = ?", project); err != nil {
			return err
		}
	}
	if _, err := o.exec("DELETE FROM projects WHERE id = ?", project); err != nil {
		return err
	}
	if _, err := o.exec("INSERT INTO cursors (project, seq) VALUES (?, 0)", project); err != nil {
		return err
	}
	_, err := o.exec("UPDATE sessions SET last_seq = MIN(last_seq, ?) WHERE project = ?", head, project)
	return err
}

// LiveAt is when the background sync last heard from the project's live socket (zero: never, or unknown).
func (o ops) LiveAt(project string) time.Time {
	var at string
	ok, err := o.one("SELECT at FROM live WHERE project = ?", []any{project}, &at)
	if !ok || err != nil {
		return time.Time{}
	}
	return parseTS(at)
}

// SetLiveAt records that the project's live socket was heard from at t.
func (o ops) SetLiveAt(project string, t time.Time) error {
	_, err := o.exec("INSERT INTO live (project, at) VALUES (?, ?) ON CONFLICT(project) DO UPDATE SET at = excluded.at",
		project, ts(t))
	return err
}

// ClearLiveAt forgets when the project's live socket was heard from (it ended): hooks pull again. When it was last
// heard stays as the project's last sync (SyncedAt).
func (o ops) ClearLiveAt(project string) error {
	if _, err := o.exec("INSERT INTO meta (name, value) SELECT ?, at FROM live WHERE project = ? "+
		"ON CONFLICT(name) DO UPDATE SET value = MAX(meta.value, excluded.value)", "synced_at:"+project, project); err != nil {
		return wrap(err)
	}
	_, err := o.exec("DELETE FROM live WHERE project = ?", project)
	return wrap(err)
}

// SetSyncedAt records that the project was synced at t (a pull answered); a later time already kept stays.
func (o ops) SetSyncedAt(project string, t time.Time) error {
	_, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET "+
		"value = MAX(meta.value, excluded.value)", "synced_at:"+project, ts(t))
	return wrap(err)
}

// SyncedAt is when the project was last synced: a pull answered, or its live socket heard (zero: never).
func (o ops) SyncedAt(project string) time.Time {
	at := parseTS(o.meta("synced_at:" + project))
	if live := o.LiveAt(project); live.After(at) {
		return live
	}
	return at
}

// PutOrgNames records org names by id, as account.projects gives them (the dashboard names the org offline too).
func (o ops) PutOrgNames(names map[string]string) error {
	for id, name := range names {
		if id == "" || name == "" || o.meta("org_name:"+id) == name {
			continue
		}
		if err := o.setMeta("org_name:"+id, name); err != nil {
			return err
		}
	}
	return nil
}

// OrgName is an org's name as last seen ("" when unknown).
func (o ops) OrgName(id string) string { return o.meta("org_name:" + id) }

// UpdateCheckedAt is when the background sync last checked for a newer release (zero: never).
func (o ops) UpdateCheckedAt() time.Time { return parseTS(o.meta("self_update_checked_at")) }

// SetUpdateCheckedAt records a check for a newer release, whatever came of it.
func (o ops) SetUpdateCheckedAt(t time.Time) error { return o.setMeta("self_update_checked_at", ts(t)) }

// FirstNoticeOf records that the person was told of release version (a new one they must install themselves):
// true the first time for that version, false after (one notice per version, however often it is checked).
func (o ops) FirstNoticeOf(version string) (bool, error) {
	res, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value "+
		"WHERE meta.value != excluded.value", "self_update_noticed", version)
	if err != nil {
		return false, wrap(err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetUpdatedTo records the release the background sync updated teamflow to, for the dashboard to tell once.
func (o ops) SetUpdatedTo(version string) error { return o.setMeta("self_update_updated_to", version) }

// TakeUpdatedTo is the release the background sync updated teamflow to, taken (told once): "" when none.
func (o ops) TakeUpdatedTo() (string, error) {
	var v string
	ok, err := o.one("DELETE FROM meta WHERE name = ? RETURNING value", []any{"self_update_updated_to"}, &v)
	if !ok || err != nil {
		return "", wrap(err)
	}
	return v, nil
}

func (o ops) setMeta(name, value string) error {
	_, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value",
		name, value)
	return wrap(err)
}

// ClaimHookSync takes a hook's turn to sync the project over the network: true at most once per every (racing
// hooks: one wins), and never while the project is marked offline (SetHookOffline) for at most offlineFor. A clock
// set back never keeps the hook off the network: a last sync later than now, or an offline mark further ahead than
// offlineFor, is stale.
func (o ops) ClaimHookSync(project string, now time.Time, every, offlineFor time.Duration) (bool, error) {
	if o.HookOffline(project, now, offlineFor) {
		return false, nil
	}
	res, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value "+
		"WHERE meta.value <= ? OR meta.value > ?", "hook_sync_at:"+project, ts(now), ts(now.Add(-every)), ts(now))
	if err != nil {
		return false, wrap(err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// HookOffline says whether the project is marked offline for hooks at now (a mark further ahead than offlineFor,
// from a clock set back, is stale).
func (o ops) HookOffline(project string, now time.Time, offlineFor time.Duration) bool {
	until := o.meta("hook_offline_until:" + project)
	if until == "" {
		return false
	}
	u := parseTS(until)
	return u.After(now) && !u.After(now.Add(offlineFor))
}

// SetHookOffline keeps hooks off the network for the project until then (a sync just failed).
func (o ops) SetHookOffline(project string, until time.Time) error {
	_, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value",
		"hook_offline_until:"+project, ts(until))
	return wrap(err)
}

func (o ops) meta(name string) string {
	var v string
	if ok, err := o.one("SELECT value FROM meta WHERE name = ?", []any{name}, &v); !ok || err != nil {
		return ""
	}
	return v
}

// SetProtocol records the server's protocol (its Teamflow-Protocol header) and whether it refused this client
// (UPGRADE_REQUIRED); server 0 keeps the protocol already known. It writes only when something changed, so a call answered as usual costs no write lock.
func (o ops) SetProtocol(server int, upgradeRequired bool) error {
	want := map[string]string{"upgrade_required": strconv.FormatBool(upgradeRequired)}
	if server > 0 { // 0: the answer did not say; keep what is known
		want["server_protocol"] = strconv.Itoa(server)
	}
	for name, v := range want {
		if o.meta(name) == v {
			continue
		}
		if _, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value",
			name, v); err != nil {
			return err
		}
	}
	return nil
}

// ServerProtocol is the server's protocol as last seen (0: not yet).
func (o ops) ServerProtocol() int {
	n, _ := strconv.Atoi(o.meta("server_protocol"))
	return n
}

// ProtocolOK says whether the server still serves this client; when it does not, the message tells the person
// to update teamflow.
func (o ops) ProtocolOK() (bool, string) {
	if o.meta("upgrade_required") == "true" {
		return false, UpdateMessage
	}
	return true, ""
}

// ------------------------------------------------------------------ spool

// The spool is the ordered outbox of each project under each sign-in (server, account, project): what this
// machine records on the project's shared record leaves in the order it was made, under whatever key that sign-in
// holds for the project when it is sent. Most items are signal.emit; the human acts drift.resolve and
// drift.dismiss wait in it too (a verdict is a signal.emit).

// Outbox names one outbox: a sign-in and a project. The zero sign-in names rows queued before outboxes were
// bound whose key is gone: never sent, only listed.
type Outbox struct{ Server, Account, Project string }

// OutboxOf is the outbox of project under the sign-in b.
func OutboxOf(b KeyBinding, project string) Outbox {
	return Outbox{Server: b.Server, Account: b.Account, Project: project}
}

// Ref names the outbox without revealing whose it is: 16 hex digits of a SHA-256 (the sender lock's name).
func (o Outbox) Ref() string {
	sum := sha256.Sum256([]byte(o.Server + "\x00" + o.Account + "\x00" + o.Project))
	return hex.EncodeToString(sum[:])[:16]
}

// SpoolItem is an item waiting to be sent: Signal is its id (the signal's id; for a drift act, its own).
type SpoolItem struct {
	N         int64
	Signal    string
	Method    string // signal.emit, drift.resolve or drift.dismiss
	Session   string
	Params    string // the call's params as JSON (SignalEmit for signal.emit)
	CreatedAt time.Time
	Tries     int       // times it was sent
	Fails     int       // failures counting towards giving up on it
	FirstFail time.Time // the first of them (zero: none)
	LastError string
	Alone     bool // goes in a request of its own next time
}

// OutboxRow is an item with the outbox it waits in.
type OutboxRow struct {
	Outbox
	SpoolItem
}

const itemCols = "n, signal, method, session, params, created_at, tries, fails, COALESCE(first_fail_at, ''), last_error, alone"

func scanItem(r interface{ Scan(...any) error }, extra ...any) (SpoolItem, error) {
	var it SpoolItem
	var created, first string
	err := r.Scan(append([]any{&it.N, &it.Signal, &it.Method, &it.Session, &it.Params, &created, &it.Tries, &it.Fails,
		&first, &it.LastError, &it.Alone}, extra...)...)
	it.CreatedAt = parseTS(created)
	if first != "" {
		it.FirstFail = parseTS(first)
	}
	return it, err
}

// SpoolAdd queues an emitted signal at the end of the outbox ob. The same signal twice is queued once.
func (o ops) SpoolAdd(signal, session string, ob Outbox, params string, now time.Time) error {
	return o.OutboxAdd(signal, "signal.emit", session, ob, params, now)
}

// OutboxAdd queues one call (its id, method and params) at the end of the outbox ob. The same id twice is queued
// once, in its first place.
func (o ops) OutboxAdd(id, method, session string, ob Outbox, params string, now time.Time) error {
	_, err := o.exec("INSERT OR IGNORE INTO spool (signal, method, session, key_ref, server, account, project, params, "+
		"created_at) VALUES (?, ?, ?, '', ?, ?, ?, ?, ?)", id, method, session, ob.Server, ob.Account, ob.Project, params, ts(now))
	return err
}

// Spooled are the items waiting in the outbox ob, in order.
func (o ops) Spooled(ob Outbox) ([]SpoolItem, error) {
	var out []SpoolItem
	err := o.each("SELECT "+itemCols+" FROM spool WHERE server = ? AND account = ? AND project = ? ORDER BY n",
		[]any{ob.Server, ob.Account, ob.Project}, func(r *sql.Rows) error {
			it, err := scanItem(r)
			out = append(out, it)
			return err
		})
	return out, err
}

// OutboxAll is every waiting item of every outbox (every sign-in), in order.
func (o ops) OutboxAll() ([]OutboxRow, error) {
	var out []OutboxRow
	err := o.each("SELECT "+itemCols+", server, account, project FROM spool ORDER BY n", nil, func(r *sql.Rows) error {
		var row OutboxRow
		var err error
		row.SpoolItem, err = scanItem(r, &row.Server, &row.Account, &row.Project)
		out = append(out, row)
		return err
	})
	return out, err
}

// OutboxProjects are the projects with items waiting under the sign-in b.
func (o ops) OutboxProjects(b KeyBinding) ([]string, error) {
	var out []string
	err := o.each("SELECT DISTINCT project FROM spool WHERE server = ? AND account = ? AND project != '' ORDER BY project",
		[]any{b.Server, b.Account}, func(r *sql.Rows) error {
			var p string
			out = append(out, p)
			return r.Scan(&out[len(out)-1])
		})
	return out, err
}

// Ahead is how many items wait ahead of the item id in the outbox ob; false when id is not queued there.
func (o ops) Ahead(ob Outbox, id string) (int, bool, error) {
	var n int64
	found, err := o.one("SELECT n FROM spool WHERE signal = ? AND server = ? AND account = ? AND project = ?",
		[]any{id, ob.Server, ob.Account, ob.Project}, &n)
	if err != nil || !found {
		return 0, false, err
	}
	var ahead int
	_, err = o.one("SELECT COUNT(*) FROM spool WHERE server = ? AND account = ? AND project = ? AND n < ?",
		[]any{ob.Server, ob.Account, ob.Project, n}, &ahead)
	return ahead, err == nil, err
}

// Waiting is how many items wait in the outbox ob.
func (o ops) Waiting(ob Outbox) (int, error) {
	var n int
	_, err := o.one("SELECT COUNT(*) FROM spool WHERE server = ? AND account = ? AND project = ?",
		[]any{ob.Server, ob.Account, ob.Project}, &n)
	return n, err
}

// Tried records that these items are being sent (once more).
func (o ops) Tried(ids []string) error {
	for _, id := range ids {
		if _, err := o.exec("UPDATE spool SET tries = tries + 1 WHERE signal = ?", id); err != nil {
			return err
		}
	}
	return nil
}

// Failed records a failure of the item id that counts towards giving up on it, and returns the item as it is now.
func (o ops) Failed(id, why string, now time.Time) (SpoolItem, error) {
	if _, err := o.exec("UPDATE spool SET fails = fails + 1, first_fail_at = COALESCE(first_fail_at, ?), last_error = ? "+
		"WHERE signal = ?", ts(now), why, id); err != nil {
		return SpoolItem{}, err
	}
	it, err := scanItem(o.row("SELECT "+itemCols+" FROM spool WHERE signal = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return it, fmt.Errorf("no queued item %s", id)
	}
	return it, err
}

// SetAlone marks the item id to go in a request of its own next time (or clears the mark).
func (o ops) SetAlone(id string, on bool) error {
	_, err := o.exec("UPDATE spool SET alone = ? WHERE signal = ?", on, id)
	return err
}

// KeyFetchWaiting says whether fetching a key for the outbox ob waits after a refusal of the membership (until a
// time at most window ahead of now: a clock set back never waits longer).
func (o ops) KeyFetchWaiting(ob Outbox, now time.Time, window time.Duration) bool {
	until := o.meta("key_fetch_refused:" + ob.Ref())
	if until == "" {
		return false
	}
	u := parseTS(until)
	return u.After(now) && !u.After(now.Add(window))
}

// SetKeyFetchRefused keeps key fetches for the outbox ob off until then (the server refused the membership).
func (o ops) SetKeyFetchRefused(ob Outbox, until time.Time) error {
	_, err := o.exec("INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value",
		"key_fetch_refused:"+ob.Ref(), ts(until))
	return wrap(err)
}

// adoptLegacy binds rows queued before outboxes were bound (no sign-in) to the sign-in and project of the key
// they were queued with, when that key is still on this machine. Run on open; cheap when there are none.
func (o ops) adoptLegacy() error {
	var refs []string
	err := o.each("SELECT DISTINCT key_ref FROM spool WHERE server = '' AND key_ref != ''", nil, func(r *sql.Rows) error {
		var ref string
		out := r.Scan(&ref)
		refs = append(refs, ref)
		return out
	})
	if err != nil || len(refs) == 0 {
		return err
	}
	type bound struct{ server, account, project string }
	byRef := map[string]bound{}
	err = o.each("SELECT server, account, project, key FROM project_keys", nil, func(r *sql.Rows) error {
		var b bound
		var key string
		if err := r.Scan(&b.server, &b.account, &b.project, &key); err != nil {
			return err
		}
		byRef[keyRef(key)] = b
		return nil
	})
	if err != nil {
		return err
	}
	for _, ref := range refs {
		b, ok := byRef[ref]
		if !ok {
			continue
		}
		if _, err := o.exec("UPDATE spool SET server = ?, account = ?, project = ? WHERE key_ref = ? AND server = ''",
			b.server, b.account, b.project, ref); err != nil {
			return err
		}
	}
	return nil
}

// keyRef is api.KeyRef (the first 16 hex digits of the key's SHA-256), which the store cannot import.
func keyRef(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// SpoolSize is how many signals wait.
func (o ops) SpoolSize() (int, error) {
	var n int
	_, err := o.one("SELECT COUNT(*) FROM spool", nil, &n)
	return n, err
}

// SpoolDone takes a signal off the spool.
func (o ops) SpoolDone(signal string) error {
	_, err := o.exec("DELETE FROM spool WHERE signal = ?", signal)
	return err
}

// ------------------------------------------------------------------ citations and notices

// Cite records that a session relies on these signals.
func (o ops) Cite(session string, signals []string, now time.Time) error {
	for _, s := range signals {
		if _, err := o.exec("INSERT OR IGNORE INTO citations (session, signal, cited_at) VALUES (?, ?, ?)", session, s, ts(now)); err != nil {
			return err
		}
	}
	return nil
}

// Citations are the signals a session cites, in the order cited.
func (o ops) Citations(session string) ([]string, error) {
	var out []string
	err := o.each("SELECT signal FROM citations WHERE session = ? ORDER BY cited_at, signal", []any{session}, func(r *sql.Rows) error {
		var s string
		if err := r.Scan(&s); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// SessionsCiting is session -> the given signals it cites.
func (o ops) SessionsCiting(signals []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, s := range signals {
		err := o.each("SELECT session FROM citations WHERE signal = ? ORDER BY session", []any{s}, func(r *sql.Rows) error {
			var sess string
			if err := r.Scan(&sess); err != nil {
				return err
			}
			out[sess] = append(out[sess], s)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Notice is something to tell a session.
type Notice struct {
	N     int64
	Type  protocol.NoticeType
	About string // "" when about nothing in particular
	Text  string
}

// Notice queues a notice for a session; one per (type, about), while a notice about nothing is never folded.
func (o ops) Notice(session string, typ protocol.NoticeType, about, text string, now time.Time) error {
	_, err := o.exec("INSERT OR IGNORE INTO notices (session, type, about, text, created_at) VALUES (?, ?, ?, ?, ?)",
		session, string(typ), nullable(about), text, ts(now))
	return err
}

// Notices are a session's notices (only those not shown yet when unshownOnly), in order.
func (o ops) Notices(session string, unshownOnly bool) ([]Notice, error) {
	q := "SELECT n, type, about, text FROM notices WHERE session = ?"
	if unshownOnly {
		q += " AND shown = 0"
	}
	var out []Notice
	err := o.each(q+" ORDER BY n", []any{session}, func(r *sql.Rows) error {
		var n Notice
		var typ string
		var about sql.NullString
		if err := r.Scan(&n.N, &typ, &about, &n.Text); err != nil {
			return err
		}
		n.Type, n.About = protocol.NoticeType(typ), about.String
		out = append(out, n)
		return nil
	})
	return out, err
}

// ClaimNotices takes the session's unshown notices (only approval requests when approvals, else every other type)
// and marks them shown in one statement, so two callers racing (parallel tool calls, a hook beside them) never
// both get one. In order.
func (o ops) ClaimNotices(session string, approvals bool) ([]Notice, error) {
	op := "!="
	if approvals {
		op = "="
	}
	var out []Notice
	err := o.each("UPDATE notices SET shown = 1 WHERE session = ? AND shown = 0 AND type "+op+" ? RETURNING n, type, about, text",
		[]any{session, string(protocol.NoticeTypeApprovalRequested)}, func(r *sql.Rows) error {
			var n Notice
			var typ string
			var about sql.NullString
			if err := r.Scan(&n.N, &typ, &about, &n.Text); err != nil {
				return err
			}
			n.Type, n.About = protocol.NoticeType(typ), about.String
			out = append(out, n)
			return nil
		})
	if err != nil {
		return nil, wrap(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// NoticesShown marks notices as shown.
func (o ops) NoticesShown(ns []int64) error {
	for _, n := range ns {
		if _, err := o.exec("UPDATE notices SET shown = 1 WHERE n = ?", n); err != nil {
			return err
		}
	}
	return nil
}
