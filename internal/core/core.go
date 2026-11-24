// Package core is the agent-independent client that the CLI (and later the MCP server and hooks) calls: the port
// of client/tf_client/ingestor.py. It knows the config (server, auth key), the local store (replica, spool,
// sessions, project keys) and the server, and keeps them in step.
//
// You sign in once per machine (an auth key from email and password). A session is shared with one project:
// sharing fetches this machine's project key for it with the auth key, and everything done on the project goes
// with that key, which can do nothing outside the project.
package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// HookTimeout is how long a step that a person or agent waits on may wait on the server before working from the
// replica (HOOK_TIMEOUT).
const HookTimeout = time.Duration(protocol.HookTimeout * float64(time.Second))

// DefaultTimeout bounds an ordinary call.
const DefaultTimeout = 10 * time.Second

// ContentTimeout bounds sending or fetching up to 5 MB of artifact content.
const ContentTimeout = 300 * time.Second

// ErrNotSignedIn is any account or project call on a machine with no auth key.
var ErrNotSignedIn = errors.New("not signed in: run `teamflow login --email EMAIL`")

// ErrNotShared is an action that needs the project in the replica (or a shared session) when it is not there.
var ErrNotShared = errors.New("the project has not synced yet; try again after the next prompt")

// Core is one machine's client: its home, config and store.
type Core struct {
	Dir string
	Cfg *config.Config // nil: no config.json yet
	St  *store.Store
	Now func() time.Time
}

// Open opens the Teamflow home (creating it, 0700), its config and its store.
func Open() (*Core, error) {
	dir, err := home.Ensure()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Core{Dir: dir, Cfg: cfg, St: st, Now: time.Now}, nil
}

// Close closes the store.
func (c *Core) Close() error { return c.St.Close() }

// SignedIn says whether this machine holds an auth key.
func (c *Core) SignedIn() bool { return c.Cfg.SignedIn() }

func (c *Core) client(key string, timeout time.Duration) *api.Client {
	return api.New(c.Cfg.Server, key, timeout).WithRecorder(c.St)
}

// binding is the sign-in project keys are read and written under: the config's server and account.
func (c *Core) binding() store.KeyBinding {
	if c.Cfg == nil {
		return store.KeyBinding{}
	}
	return store.Binding(c.Cfg.Server, c.Cfg.Email)
}

// KeyProjects are the projects this sign-in has a key for on this machine.
func (c *Core) KeyProjects() ([]string, error) { return c.St.KeyProjects(c.binding()) }

// AuthAPI is the server with the auth key: account things (your projects, fetching project keys).
func (c *Core) AuthAPI(timeout time.Duration) (*api.Client, error) {
	if !c.SignedIn() {
		return nil, ErrNotSignedIn
	}
	return c.client(c.Cfg.AuthKey, timeout), nil
}

// ProjectKey is this machine's key for the project, fetched with the auth key the first time.
func (c *Core) ProjectKey(ctx context.Context, project string) (string, error) {
	if key, _, _, _, ok := c.St.ProjectKey(c.binding(), project); ok {
		return key, nil
	}
	a, err := c.AuthAPI(DefaultTimeout)
	if err != nil {
		return "", err
	}
	var got protocol.ProjectKey
	if err := a.Call(ctx, "key.fetch", protocol.KeyFetch{Project: project}, &got); err != nil {
		return "", err
	}
	if err := c.St.PutProjectKey(c.binding(), got.Project, got.Key, got.Member, got.Org, got.Name, c.Now()); err != nil {
		return "", err
	}
	return got.Key, nil
}

// API is the server with the project's key: everything on that project, and only that project.
func (c *Core) API(ctx context.Context, project string, timeout time.Duration) (*api.Client, error) {
	if c.Cfg == nil || c.Cfg.Server == "" {
		return nil, ErrNotSignedIn
	}
	key, err := c.ProjectKey(ctx, project)
	if err != nil {
		return nil, err
	}
	return c.client(key, timeout), nil
}

// Sync pushes the spool and pulls the events of each project this machine has a key for (or only `only`), then
// fetches the names of members the replica has none for. False when offline. A key the server no longer takes
// (NOT_ALLOWED, NOT_A_MEMBER) is dropped.
func (c *Core) Sync(ctx context.Context, timeout time.Duration, only string) bool {
	projects := []string{only}
	if only == "" {
		var err error
		if projects, err = c.KeyProjects(); err != nil {
			return true
		}
	}
	if c.Cfg == nil || c.Cfg.Server == "" {
		return true
	}
	for _, pid := range projects {
		key, _, _, _, ok := c.St.ProjectKey(c.binding(), pid)
		if !ok {
			continue
		}
		a := c.client(key, timeout)
		err := c.syncOne(ctx, a, pid, timeout)
		if errors.Is(err, api.ErrOffline) {
			return false
		}
		var ae *api.APIError
		if errors.As(err, &ae) && (ae.Reason == string(protocol.RejectReasonNotAllowed) ||
			ae.Reason == string(protocol.RejectReasonNotAMember)) {
			_ = c.St.DropProjectKey(c.binding(), pid)
		}
	}
	return true
}

func (c *Core) syncOne(ctx context.Context, a *api.Client, pid string, timeout time.Duration) error {
	if _, err := syncer.Push(ctx, c.St, a); err != nil {
		return err
	}
	if _, err := syncer.Pull(ctx, c.St, a, timeout); err != nil {
		return err
	}
	return c.refreshNames(ctx, a, pid)
}

// refreshNames fetches the org's people when a member of the project has no name in the replica (Python's
// Ingestor._refresh_names).
func (c *Core) refreshNames(ctx context.Context, a *api.Client, pid string) error {
	p, ok, err := c.St.Project(pid)
	if err != nil || !ok {
		return err
	}
	names, err := c.St.Names()
	if err != nil {
		return err
	}
	for _, m := range p.Members {
		if _, known := names[m]; !known {
			return c.refreshPeople(ctx, a)
		}
	}
	return nil
}

// refreshPeople records the names, aliases and org titles of the project's org (one call).
func (c *Core) refreshPeople(ctx context.Context, a *api.Client) error {
	var everyone protocol.Members
	if err := a.Call(ctx, "member.list", nil, &everyone); err != nil {
		return err
	}
	people := map[string]store.Person{}
	for _, m := range everyone.Members {
		people[m.ID] = store.Person{Name: m.Name, Alias: m.Alias, Title: m.Title}
	}
	return c.St.PutPeople(people)
}

// ------------------------------------------------------------------ the account

// Login trades email and password for an auth key, kept in config.json (0600). The password is not kept.
func (c *Core) Login(ctx context.Context, server, email, password string) (protocol.LoginResult, error) {
	var out protocol.LoginResult
	a := api.New(server, "", DefaultTimeout).WithRecorder(c.St)
	if err := a.Call(ctx, "auth.login", protocol.Login{Email: email, Password: password}, &out); err != nil {
		return out, err
	}
	if c.Cfg != nil && (c.Cfg.Server != server || !strings.EqualFold(c.Cfg.Email, out.Account.Email)) {
		// Another server or another account: the old sign-in's project keys must never reach the new one.
		if err := c.forgetProjects(); err != nil {
			return out, err
		}
	}
	cfg := config.Config{Server: server, AuthKey: out.AuthKey, Email: out.Account.Email}
	if err := config.Save(c.Dir, cfg); err != nil {
		return out, err
	}
	c.Cfg = &cfg
	return out, nil
}

// forgetProjects drops every project key and unshares every session shared with them.
func (c *Core) forgetProjects() error {
	return c.St.Tx(func(tx *store.Tx) error {
		shared, err := tx.SharedSessions()
		if err != nil {
			return err
		}
		for _, s := range shared {
			if err := tx.Share(s.ID, "", "", "", nil); err != nil {
				return err
			}
		}
		return tx.DropProjectKeys()
	})
}

// Logout revokes the auth key (best effort: offline it is still forgotten here), and forgets it and this
// machine's project keys.
func (c *Core) Logout(ctx context.Context) error {
	if a, err := c.AuthAPI(DefaultTimeout); err == nil {
		_ = a.Call(ctx, "auth.logout", nil, nil)
	}
	if c.Cfg != nil {
		cfg := config.Config{Server: c.Cfg.Server}
		if err := config.Save(c.Dir, cfg); err != nil {
			return err
		}
		c.Cfg = &cfg
	}
	return c.St.DropProjectKeys()
}

// Projects are every project you are in, across your orgs.
func (c *Core) Projects(ctx context.Context) (protocol.AccountProjects, error) {
	var out protocol.AccountProjects
	a, err := c.AuthAPI(DefaultTimeout)
	if err != nil {
		return out, err
	}
	return out, a.Call(ctx, "account.projects", nil, &out)
}

// ------------------------------------------------------------------ sessions

// SystemTopics are the project's built-in topics (Plan): every session follows them.
func (c *Core) SystemTopics(project string) map[string]bool {
	out := map[string]bool{}
	topics, _ := c.St.Topics(project)
	for _, t := range topics {
		if t.System {
			out[t.ID] = true
		}
	}
	return out
}

// Share binds the session to the project (and to these topic ids; nil: all), fetching this machine's project key
// for it if needed.
func (c *Core) Share(ctx context.Context, sid, project string, topics []string) (string, error) {
	a, err := c.API(ctx, project, DefaultTimeout)
	if err != nil {
		return "", err
	}
	var view protocol.ProjectView
	if err := a.Call(ctx, "project.get", protocol.ProjectGet{Project: project}, &view); err != nil {
		return "", err
	}
	var me protocol.Member
	if err := a.Call(ctx, "member.me", nil, &me); err != nil {
		return "", err
	}
	if err := c.St.PutNames(view.Names); err != nil {
		return "", err
	}
	if err := c.refreshPeople(ctx, a); err != nil { // names, aliases and titles may have changed since
		return "", err
	}
	if err := c.St.Share(sid, project, me.ID, a.KeyRef(), topics); err != nil {
		return "", err
	}
	c.Sync(ctx, DefaultTimeout, project)
	scope := "all topics"
	if topics != nil {
		var names []string
		for _, t := range view.Topics {
			if contains(topics, t.ID) {
				names = append(names, t.Name)
			}
		}
		scope = "topics " + strings.Join(names, ", ")
	}
	return fmt.Sprintf("This session now works on %q (%s) as %s.", view.Project.Name, scope, me.Name), nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// orgOf names each project's org ("" when unknown).
func orgOf(mine protocol.AccountProjects) map[string]string {
	out := map[string]string{}
	for _, v := range mine.Projects {
		out[v.Project.ID] = mine.Orgs[v.Project.Org]
	}
	return out
}

// FindProject is a project you are in, by id, by name, or as "Org/Name" when the name is in more than one org.
func (c *Core) FindProject(ctx context.Context, ref string) (protocol.ProjectView, error) {
	mine, err := c.Projects(ctx)
	if err != nil {
		return protocol.ProjectView{}, err
	}
	org := orgOf(mine)
	var hits []protocol.ProjectView
	for _, v := range mine.Projects {
		if ref == v.Project.ID || strings.EqualFold(ref, v.Project.Name) ||
			strings.EqualFold(ref, org[v.Project.ID]+"/"+v.Project.Name) {
			hits = append(hits, v)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	listed := hits
	why := "several are called " + pyRepr(ref)
	if len(hits) == 0 {
		listed, why = mine.Projects, "none is called "+pyRepr(ref)
	}
	var names []string
	for _, v := range listed {
		names = append(names, org[v.Project.ID]+"/"+v.Project.Name)
	}
	yours := strings.Join(names, ", ")
	if yours == "" {
		yours = "none"
	}
	return protocol.ProjectView{}, fmt.Errorf("which project? %s; yours: %s", why, yours)
}

// Join binds the session to a project and topics named as the person names them (names or ids).
func (c *Core) Join(ctx context.Context, sid, project string, topics []string) (string, error) {
	view, err := c.FindProject(ctx, project)
	if err != nil {
		return "", err
	}
	var ids []string
	if len(topics) > 0 {
		var pick []protocol.Topic
		for _, t := range view.Topics {
			if !t.System {
				pick = append(pick, t)
			}
		}
		ids = []string{}
		for _, want := range topics {
			hit := ""
			for _, t := range pick {
				if want == t.ID || want == t.Name || strings.EqualFold(want, t.Name) {
					hit = t.ID
					break
				}
			}
			if hit == "" {
				var names []string
				for _, t := range pick {
					names = append(names, t.Name)
				}
				return "", fmt.Errorf("no topic %s in %s; topics: %s", pyRepr(want), view.Project.Name, strings.Join(names, ", "))
			}
			if !contains(ids, hit) {
				ids = append(ids, hit)
			}
		}
	}
	return c.Share(ctx, sid, view.Project.ID, ids)
}

// ProjectMenu is your projects and their topics, for the person to choose what the session works on.
func (c *Core) ProjectMenu(ctx context.Context, sid string) (string, error) {
	mine, err := c.Projects(ctx)
	if err != nil {
		return "", err
	}
	org := orgOf(mine)
	var lines []string
	if sess, ok, _ := c.St.Session(sid); ok && sess.Project != "" {
		for _, v := range mine.Projects {
			if v.Project.ID != sess.Project {
				continue
			}
			scope := "all topics"
			if sess.Topics != nil {
				var names []string
				for _, t := range v.Topics {
					if !t.System && sess.Follows(t.ID, nil) {
						names = append(names, t.Name)
					}
				}
				scope = "topics " + strings.Join(names, ", ")
			}
			lines = append(lines, fmt.Sprintf("This session works on %q (%s).", v.Project.Name, scope))
		}
	}
	if len(mine.Projects) == 0 {
		return "Teamflow: you are not in any project yet; a project's initiator invites you.", nil
	}
	lines = append(lines, "Teamflow projects you are in:")
	for _, v := range mine.Projects {
		var names []string
		for _, t := range v.Topics {
			if !t.System {
				names = append(names, t.Name)
			}
		}
		lines = append(lines, fmt.Sprintf("- %s/%s: topics %s", org[v.Project.ID], v.Project.Name, strings.Join(names, ", ")))
	}
	lines = append(lines, "Ask the person, in your own words, which project this session works on, then which of its "+
		"topics (several allowed; all by default). Then call the session_join tool with their choice (names as shown).")
	return strings.Join(lines, "\n"), nil
}

// Stop unshares the session.
func (c *Core) Stop(sid string) (string, error) {
	if err := c.St.Share(sid, "", "", "", nil); err != nil {
		return "", err
	}
	return "This session is no longer shared.", nil
}

// pyRepr quotes a name as Python's repr does for the plain names people type ('Nope'; "it's").
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}
