package store

import (
	"database/sql"
	"encoding/json"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Dump is everything the store holds, for `teamflow debug state --json` (the e2e harness looks through it). It
// never carries a key: project keys are listed by project and member only, and the key column is never read.
type Dump struct {
	Sessions     []DumpSession              `json:"sessions"`
	Notices      []DumpNotice               `json:"notices"`
	Spool        []DumpSpool                `json:"spool"`
	Citations    []DumpCitation             `json:"citations"`
	Signals      []protocol.StoredSignal    `json:"signals"`
	Cursors      map[string]int64           `json:"cursors"`
	People       map[string]DumpPerson      `json:"people"`
	Live         map[string]string          `json:"live"`
	ProjectKeys  []DumpProjectKey           `json:"project_keys"`
	Projects     []protocol.Project         `json:"projects"`
	Topics       []protocol.Topic           `json:"topics"`
	Drifts       []protocol.Drift           `json:"drifts"`
	Deliverables []protocol.Deliverables    `json:"deliverables"`
	Archived     map[string]map[string]bool `json:"archived_artifacts"` // project -> chain -> archived
}

type DumpSession struct {
	ID           string   `json:"id"`
	Agent        string   `json:"agent"`
	AgentPID     int      `json:"agent_pid"`
	Cwd          string   `json:"cwd"`
	Project      string   `json:"project"`
	Member       string   `json:"member"`
	KeyRef       string   `json:"key_ref"`
	LastSeq      int64    `json:"last_seq"`
	StateVersion int64    `json:"state_version"`
	Topics       []string `json:"topics"` // null: all of them
}

type DumpNotice struct {
	N         int64  `json:"n"`
	Session   string `json:"session"`
	Type      string `json:"type"`
	About     string `json:"about"`
	Text      string `json:"text"`
	Shown     bool   `json:"shown"`
	CreatedAt string `json:"created_at"`
}

type DumpSpool struct {
	N         int64           `json:"n"`
	Signal    string          `json:"signal"`
	Session   string          `json:"session"`
	KeyRef    string          `json:"key_ref"`
	Params    json.RawMessage `json:"params"`
	CreatedAt string          `json:"created_at"`
}

type DumpCitation struct {
	Session string `json:"session"`
	Signal  string `json:"signal"`
	CitedAt string `json:"cited_at"`
}

type DumpPerson struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
	Title string `json:"title"`
}

type DumpProjectKey struct {
	Project string `json:"project"`
	Member  string `json:"member"`
	Org     string `json:"org"`
	Name    string `json:"name"`
}

// Dump reads the whole store (see Dump). Lists are never null.
func (o ops) Dump() (Dump, error) {
	d := Dump{Sessions: []DumpSession{}, Notices: []DumpNotice{}, Spool: []DumpSpool{}, Citations: []DumpCitation{},
		People: map[string]DumpPerson{}, Live: map[string]string{}, ProjectKeys: []DumpProjectKey{},
		Archived: map[string]map[string]bool{}}
	sessions, err := o.sessionsWhere("1 = 1 ORDER BY started_at, id")
	if err != nil {
		return d, err
	}
	for _, s := range sessions {
		d.Sessions = append(d.Sessions, DumpSession{s.ID, s.Agent, s.AgentPID, s.Cwd, s.Project, s.Member, s.KeyRef,
			s.LastSeq, s.StateVersion, s.Topics})
	}
	steps := []func() error{
		func() error {
			return o.each("SELECT n, session, type, about, text, shown, created_at FROM notices ORDER BY n", nil, func(r *sql.Rows) error {
				var n DumpNotice
				var about sql.NullString
				if err := r.Scan(&n.N, &n.Session, &n.Type, &about, &n.Text, &n.Shown, &n.CreatedAt); err != nil {
					return err
				}
				n.About = about.String
				d.Notices = append(d.Notices, n)
				return nil
			})
		},
		func() error {
			return o.each("SELECT n, signal, session, key_ref, params, created_at FROM spool ORDER BY n", nil, func(r *sql.Rows) error {
				var s DumpSpool
				var params string
				if err := r.Scan(&s.N, &s.Signal, &s.Session, &s.KeyRef, &params, &s.CreatedAt); err != nil {
					return err
				}
				s.Params = json.RawMessage(params)
				if !json.Valid(s.Params) { // shown as a string, so the dump stays JSON
					s.Params, _ = json.Marshal(params)
				}
				d.Spool = append(d.Spool, s)
				return nil
			})
		},
		func() error {
			return o.each("SELECT session, signal, cited_at FROM citations ORDER BY session, cited_at, signal", nil, func(r *sql.Rows) error {
				var c DumpCitation
				if err := r.Scan(&c.Session, &c.Signal, &c.CitedAt); err != nil {
					return err
				}
				d.Citations = append(d.Citations, c)
				return nil
			})
		},
		func() (err error) {
			d.Signals, err = listJSON[protocol.StoredSignal](o, "SELECT data FROM signals ORDER BY project, seq, id")
			return err
		},
		func() (err error) {
			d.Cursors, err = o.CursorsErr()
			return err
		},
		func() error {
			people, err := o.People()
			for id, p := range people {
				d.People[id] = DumpPerson(p)
			}
			return err
		},
		func() error {
			return o.each("SELECT project, at FROM live", nil, func(r *sql.Rows) error {
				var p, at string
				if err := r.Scan(&p, &at); err != nil {
					return err
				}
				d.Live[p] = at
				return nil
			})
		},
		func() error { // never the key column
			return o.each("SELECT project, member, org, name FROM project_keys ORDER BY fetched_at, project", nil, func(r *sql.Rows) error {
				var k DumpProjectKey
				if err := r.Scan(&k.Project, &k.Member, &k.Org, &k.Name); err != nil {
					return err
				}
				d.ProjectKeys = append(d.ProjectKeys, k)
				return nil
			})
		},
		func() (err error) {
			d.Projects, err = listJSON[protocol.Project](o, "SELECT data FROM projects ORDER BY id")
			return err
		},
		func() (err error) {
			d.Topics, err = listJSON[protocol.Topic](o, "SELECT data FROM topics ORDER BY project, id")
			return err
		},
		func() (err error) {
			d.Drifts, err = listJSON[protocol.Drift](o, "SELECT data FROM drifts ORDER BY project, seq, id")
			return err
		},
		func() error { // through Deliverables, which carries the archived flag kept beside the JSON
			var projects []string
			err := o.each("SELECT project FROM deliverables ORDER BY project", nil, func(r *sql.Rows) error {
				var p string
				err := r.Scan(&p)
				projects = append(projects, p)
				return err
			})
			for _, p := range projects {
				if err != nil {
					break
				}
				var dl protocol.Deliverables
				var ok bool
				if dl, ok, err = o.Deliverables(p); ok {
					d.Deliverables = append(d.Deliverables, dl)
				}
			}
			return err
		},
		func() error {
			return o.each("SELECT project, chain, archived FROM artifact_archive", nil, func(r *sql.Rows) error {
				var p, chain string
				var archived bool
				if err := r.Scan(&p, &chain, &archived); err != nil {
					return err
				}
				if d.Archived[p] == nil {
					d.Archived[p] = map[string]bool{}
				}
				d.Archived[p][chain] = archived
				return nil
			})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return d, err
		}
	}
	if d.Signals == nil {
		d.Signals = []protocol.StoredSignal{}
	}
	if d.Projects == nil {
		d.Projects = []protocol.Project{}
	}
	if d.Topics == nil {
		d.Topics = []protocol.Topic{}
	}
	if d.Drifts == nil {
		d.Drifts = []protocol.Drift{}
	}
	if d.Deliverables == nil {
		d.Deliverables = []protocol.Deliverables{}
	}
	return d, nil
}
