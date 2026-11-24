package mcpserver

// The sad paths every tool meets: no sign-in, no session, an unshared session, the server unreachable, an expired
// auth key, a revoked membership, an old client, a broken session lookup, a restart, and calls at the same time.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// args are a valid call of each tool (what it needs to get past its schema).
var args = map[string]map[string]any{
	"project_get": nil, "project_list": nil, "project_history": nil, "whoami": nil, "inbox": nil, "approvals": nil,
	"deliverables_get": nil, "deliverables_delete": nil, "deliverables_history": nil, "task_list": nil, "drift_list": nil,
	"project_request_update": nil, "deliverables_archive": nil,
	"project_create":        {"name": "X", "topics": []any{}},
	"project_members":       {"members": []any{"huy"}},
	"topic_create":          {"name": "Tax", "description": "VAT"},
	"topic_update":          {"topic": "Rounding", "closed": true},
	"signal_emit":           {"signals": []any{map[string]any{"kind": "NOTE", "topic": "Rounding", "title": "t", "summary": "s", "points": []any{"p"}}}},
	"signal_get":            {"signal": S1},
	"signal_list":           {"query": "x"},
	"signal_cite":           {"signals": []any{S1}},
	"artifact_write":        {"topic": "Rounding", "summary": "s", "content": "x", "name": "a.md"},
	"artifact_publish":      {"topic": "Rounding", "name": "a.mp4", "summary": "s", "url": "https://x.example/a.mp4"},
	"artifact_get":          {"id": S1},
	"artifact_list":         nil,
	"session_join":          {"project": "Pricing"},
	"deliverables_validate": {"milestones": []any{map[string]any{"name": "D", "due": "2099-01-01", "actions": []any{map[string]any{"name": "A", "topic": "Rounding", "due": "2099-01-01"}}}}},
	"deliverables_publish":  {"milestones": []any{map[string]any{"name": "D", "due": "2099-01-01", "actions": []any{map[string]any{"name": "A", "topic": "Rounding", "due": "2099-01-01"}}}}},
	"deliverables_update_milestone": {"base_version": 1, "milestone": map[string]any{"id": "ms_00000000000000000000000001",
		"name": "D", "due": "2099-01-01", "actions": []any{map[string]any{"name": "A", "topic": "Rounding", "due": "2099-01-01"}}}},
	"deliverables_rename":   {"base_version": 1, "names": map[string]any{A1: "B"}},
	"action_result":         {"action": A1, "outcome": "DONE", "summary": "s"},
	"action_revoke":         {"action": A1, "reason": "r"},
	"verdict":               {"signal": S1, "verdict": "APPROVE"},
	"drift_resolve":         {"drift": D1, "note": "n"},
	"drift_dismiss":         {"drift": D1, "note": "n"},
	"project_set_title":     {"member": "huy", "title": "x"},
	"project_remove_member": {"member": "huy"},
	"project_write_state":   {"base_version": 1, "plan": "p", "instruction": "i"},
	"org_set_title":         {"member": "huy", "title": "x"},
	"profile_update":        {"name": "Huy"},
	"topic_delete":          {"topic": "Rounding", "expected_version": 1},
	"artifact_archive":      {"artifact": S1},
	"org_add_member":        {"email": "mai@acme.test"},
	"org_remove_member":     {"member": "mai"},
}

// offlineFine are the tools that answer from this machine when the server can't be reached (or with nothing to
// reach it for).
var localTools = map[string]bool{"inbox": true, "approvals": true, "signal_cite": true, "signal_emit": true,
	"action_result": true, "action_revoke": true, "deliverables_validate": true, "deliverables_get": true,
	"deliverables_publish": true, "deliverables_update_milestone": true, "deliverables_rename": true,
	"artifact_publish": true, "signal_get": true}

func TestEveryToolHasValidArgs(t *testing.T) {
	for name := range registry {
		if _, ok := args[name]; !ok {
			t.Errorf("no sad-path args for %s", name)
		}
	}
}

// Signed out: a tool that needs the server says to sign in, and sends nothing.
func TestNotSignedIn(t *testing.T) {
	m := newMachine(t, false)
	for name, a := range args {
		if localTools[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			m.srv.reset()
			r := m.call(name, a)
			if !r.err || !strings.Contains(r.text(), "not signed in") && !strings.Contains(r.text(), "the project has not synced yet") &&
				!strings.Contains(r.text(), "no signal") && !strings.Contains(r.text(), "no artifact") {
				t.Fatalf("signed out: %s", r)
			}
			if calls := m.srv.methods(); len(calls) != 0 {
				t.Fatalf("sent while signed out: %v", calls)
			}
		})
	}
	// Answered from this machine, or refused without a word to the server.
	for _, name := range []string{"inbox", "signal_cite"} {
		if r := m.call(name, args[name]); r.err {
			t.Fatalf("%s signed out: %s", name, r)
		}
	}
	if o := m.call("signal_emit", args["signal_emit"]); !o.err {
		t.Fatalf("signal_emit signed out must fail: %s", o)
	}
	if calls := m.srv.methods(); len(calls) != 0 {
		t.Fatalf("sent while signed out: %v", calls)
	}
}

// No session found (or a session lookup that fails): a tool that needs the session says so; the others work.
func TestNoSession(t *testing.T) {
	needs := []string{"signal_emit", "signal_cite", "inbox", "approvals", "session_join", "artifact_write",
		"artifact_publish", "artifact_list", "deliverables_get", "deliverables_validate", "deliverables_publish",
		"deliverables_update_milestone", "deliverables_rename", "action_result", "action_revoke", "project_get",
		"project_history", "drift_list", "task_list", "deliverables_archive", "deliverables_delete", "topic_delete"}
	m := newMachine(t, true)
	m.find.id = ""
	for _, name := range needs {
		r := m.call(name, args[name])
		wantErr(t, r, "no agent session found; is Teamflow installed for this agent?")
	}
	m.find.err = errors.New("database is locked")
	wantErr(t, m.call("inbox", nil), "could not look up the agent session: database is locked")
	// A project named outright needs no session.
	m.find.err = nil
	m.srv.on("project.get", pricing(t))
	if o := m.call("project_get", map[string]any{"project": P}).obj(t); o["id"] != P {
		t.Fatalf("project_get by id without a session: %v", o)
	}
}

// A session not shared yet: the project-bound tools say the person chooses its project first.
func TestSessionNotShared(t *testing.T) {
	m := newMachine(t, true)
	m.session("loose", "", nil)
	m.find.id = "loose"
	for _, name := range []string{"signal_emit", "artifact_write", "artifact_publish", "artifact_list", "deliverables_get",
		"deliverables_publish", "action_result", "project_get", "drift_list", "task_list", "topic_delete"} {
		wantErr(t, m.call(name, args[name]), "this session is not shared: the person chooses its project first")
	}
	if calls := m.srv.methods(); len(calls) != 0 {
		t.Fatalf("sent for an unshared session: %v", calls)
	}
}

// The server unreachable: reads fail with "unreachable"; signals and action results queue in the spool, once;
// Deliverables say they are not queued.
func TestOffline(t *testing.T) {
	m := newMachine(t, true)
	seedDeliverables(t, m)
	m.srv.offline(true)
	wantErr(t, m.call("project_get", nil), "unreachable")
	wantErr(t, m.call("whoami", nil), "unreachable")
	o := m.call("deliverables_get", nil).obj(t)
	if o["deliverables"] != nil || o["message"] != offlineGet {
		t.Fatalf("deliverables_get offline %v", o)
	}
	o = m.call("deliverables_rename", map[string]any{"base_version": 1, "names": map[string]any{A1: "B"}}).obj(t)
	if o["published"] != false || o["message"] != offlinePublish {
		t.Fatalf("rename offline %v", o)
	}
	a := m.call("signal_emit", args["signal_emit"]).objs(t)
	if a[0]["queued"] != true || a[0]["message"] != "server unreachable; queued and sent when it is back" {
		t.Fatalf("emit offline %v", a)
	}
	o = m.call("action_result", map[string]any{"action": A1, "outcome": "DONE", "summary": "s"}).obj(t)
	if o["queued"] != true {
		t.Fatalf("action_result offline %v", o)
	}
	m.withStore(func(st *store.Store) error {
		n, err := st.SpoolSize()
		if n != 2 {
			t.Fatalf("spool %d, want 2", n)
		}
		return err
	})
	if got := m.srv.sent("signal.emit"); len(got) != 0 {
		t.Fatalf("sent while offline: %v", got)
	}

	// Back online, in a new server process (a restart): the next call flushes the spool, each signal once.
	m.srv.offline(false)
	m.srv.on("signal.emit", acceptedEmit)
	m.srv.on("account.me", lanMe(t))
	m.srv.on("member.me", memberMe(t))
	m.cs = nil
	m.call("whoami", nil).obj(t)
	m.withStore(func(st *store.Store) error {
		n, err := st.SpoolSize()
		if n != 0 {
			t.Fatalf("spool after the flush: %d", n)
		}
		return err
	})
	if got := m.srv.sent("signal.emit"); len(got) != 2 {
		t.Fatalf("flushed %d signals, want 2", len(got))
	}
	m.call("whoami", nil)
	if got := m.srv.sent("signal.emit"); len(got) != 2 {
		t.Fatalf("a signal was sent twice: %d", len(got))
	}
}

// An expired or revoked auth key, a membership revoked, an old client: the server's words reach the agent.
func TestRefusedKeysAndOldClients(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.me", refused(t, "account.me", "NOT_ALLOWED"))
	wantErr(t, m.call("whoami", nil), "NOT_ALLOWED")
	m.srv.on("account.update_profile", refused(t, "account.update_profile", "NOT_ALLOWED"))
	wantErr(t, m.call("profile_update", map[string]any{"name": "Huy"}), "NOT_ALLOWED")

	m.srv.on("project.get", refused(t, "project.get", "NOT_A_MEMBER"))
	wantErr(t, m.call("project_get", nil), "NOT_A_MEMBER")
	// A key the server no longer takes is dropped at the next sync.
	m.srv.on("sync.poll", refused(t, "member.list", "NOT_A_MEMBER"))
	m.call("inbox", nil)
	m.withStore(func(st *store.Store) error {
		if _, _, _, _, ok := st.ProjectKey(store.Binding(m.srv.srv.URL, email), P); ok {
			t.Fatal("a revoked membership's key is kept")
		}
		return nil
	})
	// …and a new one is fetched with the auth key, which says why not.
	m.srv.on("key.fetch", refused(t, "key.fetch", "NOT_A_MEMBER"))
	wantErr(t, m.call("drift_list", nil), "NOT_A_MEMBER")

	m2 := newMachine(t, true)
	m2.srv.on("member.me", refused(t, "member.me", "UPGRADE_REQUIRED"))
	m2.srv.on("account.me", lanMe(t))
	wantErr(t, m2.call("whoami", nil), "update teamflow")
	m2.srv.on("signal.emit", refused(t, "signal.emit", "UPGRADE_REQUIRED"))
	wantErr(t, m2.call("signal_emit", args["signal_emit"]), "update teamflow")
	m2.withStore(func(st *store.Store) error { // kept for the update, never lost
		n, err := st.SpoolSize()
		if n != 1 {
			t.Fatalf("spool %d", n)
		}
		return err
	})
}

// A sign-in made elsewhere (teamflow login in a terminal) is seen at the next call, with no restart.
func TestSignInIsReadAtEachCall(t *testing.T) {
	m := newMachine(t, false)
	wantErr(t, m.call("whoami", nil), "not signed in")
	if err := config.Save(m.dir, config.Config{Server: m.srv.srv.URL, AuthKey: auth, Email: email}); err != nil {
		t.Fatal(err)
	}
	m.srv.on("account.me", lanMe(t))
	m.srv.on("key.fetch", ok(t, "key.fetch"))
	m.srv.on("member.me", memberMe(t))
	if o := m.call("whoami", nil).obj(t); o["account"] != "lan" || o["member"] != M1 {
		t.Fatalf("whoami after a sign-in elsewhere %v", o)
	}
	if c := m.srv.sent("account.me"); c[0].Auth != "Bearer "+auth {
		t.Fatalf("auth %q", c[0].Auth)
	}
	if c := m.srv.sent("key.fetch"); len(c) != 1 || c[0].Auth != "Bearer "+auth {
		t.Fatalf("the project key is fetched with the auth key once: %+v", c)
	}
	if c := m.srv.sent("member.me"); c[0].Auth != "Bearer "+projKey {
		t.Fatalf("member.me with the project key: %q", c[0].Auth)
	}
}

// Calls at the same time (an agent's parallel tool calls) each get their answer, the store intact.
func TestConcurrentCalls(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("signal.emit", acceptedEmit)
	m.connect()
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := m.call("signal_emit", map[string]any{"signals": []any{map[string]any{"kind": "NOTE", "topic": "Rounding",
				"title": fmt.Sprint("t", i), "summary": "s", "points": []any{"p"}}}})
			if r.err || !strings.Contains(r.text(), `"accepted": true`) {
				errs <- r.String()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	m.withStore(func(st *store.Store) error {
		n, err := st.SpoolSize()
		if n != 0 {
			t.Fatalf("spool %d", n)
		}
		return err
	})
}
