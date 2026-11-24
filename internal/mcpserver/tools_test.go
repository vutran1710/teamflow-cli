package mcpserver

// Each tool against the fake server with golden answers: what it sends (method, params, key) and what it answers.
// Python twins: tests/mcp/test_tools.py (the 28 tools), tests/server/test_web_actions_from_client.py (the
// formerly web-only actions, from the client's keys).

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const acceptedEmit = `{"result": {"signal": "sg_00000000000000000000000077", "accepted": true, "reason": null, "message": "", "seq": 9, "received_at": "2026-01-15T09:00:01Z", "state": "IN_FORCE"}}`

func TestProjectTools(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("project.get", pricing(t))
	o := m.call("project_get", nil).obj(t)
	if o["id"] != P || o["name"] != "Pricing" || o["initiator"] != "lan" || !reflect.DeepEqual(o["members"], []any{"lan", "huy"}) ||
		o["state_version"] != float64(1) || len(o["topics"].([]any)) != 3 {
		t.Fatalf("project_get %v", o)
	}
	if c := m.srv.sent("project.get"); c[0].Auth != "Bearer "+projKey {
		t.Fatalf("project calls go with the project key: %q", c[0].Auth)
	}
	if keys := orderOf(t, m.call("project_get", nil).blocks[0]); strings.Join(keys, ",") !=
		"id,name,version,initiator,members,topics,state_version,plan,instruction" {
		t.Fatalf("Python's field order: %v", keys)
	}

	m.srv.on("account.projects", ok(t, "account.projects"))
	list := m.call("project_list", nil).objs(t)
	if len(list) != 1 || list[0]["id"] != P || list[0]["org"] != "Acme" {
		t.Fatalf("project_list %v", list)
	}
	if c := m.srv.sent("account.projects"); c[0].Auth != "Bearer "+auth {
		t.Fatalf("your projects go with the auth key: %q", c[0].Auth)
	}

	m.srv.on("account.me", lanMe(t))
	m.srv.on("org.members", ok(t, "org.members"))
	m.srv.on("project.create", ok(t, "project.create"))
	o = m.call("project_create", map[string]any{"name": "Billing", "topics": []any{map[string]any{"name": "Tax", "description": "VAT"}},
		"members": []any{"Huy", "mai@acme.test"}}).obj(t)
	p := m.params("project.create")
	if p["org"] != "Acme" || !reflect.DeepEqual(p["members"], []any{M2, "mb_00000000000000000000000003"}) || o["id"] != P ||
		!strings.Contains(o["note"].(string), P) {
		t.Fatalf("project_create params %v answer %v", p, o)
	}
	wantErr(t, m.call("project_create", map[string]any{"name": "X", "topics": []any{}, "members": []any{"nobody"}}),
		"no member 'nobody'; members: lan, huy, mai")

	m.srv.on("project.add_members", ok(t, "project.add_members"))
	o = m.call("project_members", map[string]any{"members": []any{"mai"}}).obj(t)
	if p := m.params("project.add_members"); !reflect.DeepEqual(p["members"], []any{"mb_00000000000000000000000003"}) ||
		p["expected_version"] != float64(1) || o["members"] == nil {
		t.Fatalf("project_members %v %v", p, o)
	}

	m.srv.on("topic.create", ok(t, "topic.create"))
	o = m.call("topic_create", map[string]any{"name": "Tax", "description": "VAT"}).obj(t)
	if p := m.params("topic.create"); p["name"] != "Tax" || p["expected_version"] != float64(1) || o["topics"] == nil {
		t.Fatalf("topic_create %v %v", p, o)
	}

	m.srv.on("topic.update", ok(t, "topic.update"))
	m.call("topic_update", map[string]any{"topic": "Currency", "closed": true}).obj(t)
	if p := m.params("topic.update"); p["topic"] != T2 || p["closed"] != true || p["name"] != nil {
		t.Fatalf("topic_update %v", p)
	}
	wantErr(t, m.call("topic_update", map[string]any{"topic": "Nope"}), "no topic 'Nope'")

	m.srv.on("project.request_update", ok(t, "project.request_update"))
	o = m.call("project_request_update", map[string]any{"note": "the VAT change"}).obj(t)
	if p := m.params("project.request_update"); p["note"] != "the VAT change" || o["kind"] != "ROLLING_UPDATE_PROJECT" || o["covers_seq"] == nil {
		t.Fatalf("project_request_update %v %v", p, o)
	}
	m.srv.on("project.request_update", refused(t, "project.request_update", "TASK_IN_PROGRESS"))
	wantErr(t, m.call("project_request_update", nil), "TASK_IN_PROGRESS")

	m.srv.on("project.history", ok(t, "project.history"))
	h := m.call("project_history", nil).objs(t)
	if len(h) == 0 || h[0]["version"] != float64(1) || !strings.HasSuffix(h[0]["created_at"].(string), "+00:00") {
		t.Fatalf("project_history %v", h)
	}
}

// orderOf is the keys of a JSON object in the order written.
func orderOf(t *testing.T, text string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestSessionJoin(t *testing.T) {
	m := newMachine(t, true)
	m.session("s2", "", nil)
	m.find.id = "s2"
	m.srv.on("account.projects", ok(t, "account.projects"))
	m.srv.on("project.get", pricing(t))
	m.srv.on("member.me", memberMe(t))
	wantText(t, m.call("session_join", map[string]any{"project": "pricing", "topics": []any{"rounding"}}),
		`This session now works on "Pricing" (topics Rounding)`)
	m.withStore(func(st *store.Store) error {
		s, _, err := st.Session("s2")
		if s.Project != P || !reflect.DeepEqual(s.Topics, []string{T1}) {
			t.Fatalf("session after join %+v", s)
		}
		return err
	})
	wantErr(t, m.call("session_join", map[string]any{"project": "Nope"}), "which project? none is called 'Nope'; yours: Acme/Pricing")
	wantErr(t, m.call("session_join", map[string]any{"project": "Pricing", "topics": []any{"Tax"}}), "no topic 'Tax' in Pricing")
}

func TestSignalTools(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("signal.emit", acceptedEmit)
	r := m.call("signal_emit", map[string]any{"signals": []any{
		map[string]any{"kind": "DECISION", "topic": "rounding", "title": "Round half up", "summary": "Half up",
			"points": []any{"0.005 rounds up"}, "approvers": []any{"Huy"}},
		map[string]any{"kind": "NOTE", "topic": "Tax", "title": "x", "summary": "s", "points": []any{"p"}},
	}})
	answers := r.objs(t)
	if len(answers) != 2 || answers[0]["accepted"] != true || answers[1]["reason"] != "INVALID" ||
		!strings.Contains(answers[1]["message"].(string), "no topic 'Tax' in this project") {
		t.Fatalf("signal_emit %v", answers)
	}
	emits := m.srv.sent("signal.emit")
	if len(emits) != 1 || !emits[0].Batch {
		t.Fatalf("routine signals go in a batch: %+v", emits)
	}
	p := m.params("signal.emit")
	sig := p["signal"].(map[string]any)
	rfa := sig["request_for_approval"].(map[string]any)
	if p["actor"] != "AGENT" || p["client_session"] != "s1" || sig["topic"] != T1 || !reflect.DeepEqual(rfa["approvers"], []any{M2}) ||
		rfa["required"] != float64(1) || p["project_version"] != float64(1) {
		t.Fatalf("signal_emit params %v", p)
	}
	m.withStore(func(st *store.Store) error {
		n, err := st.SpoolSize()
		if n != 0 {
			t.Fatalf("an answered signal stays spooled (%d)", n)
		}
		return err
	})
	// A refusal of one signal is its answer.
	m.srv.on("signal.emit", `{"error": {"code": -32602, "message": "title: too long", "data": {"reason": "SCHEMA_VIOLATION"}}}`)
	a := m.call("signal_emit", map[string]any{"signals": []any{map[string]any{"kind": "NOTE", "topic": "Rounding",
		"title": "t", "summary": "s", "points": []any{"p"}}}}).objs(t)
	if a[0]["accepted"] != false || a[0]["reason"] != "SCHEMA_VIOLATION" {
		t.Fatalf("a refused signal: %v", a)
	}
	// An approver nobody is.
	a = m.call("signal_emit", map[string]any{"signals": []any{map[string]any{"kind": "DECISION", "topic": "Rounding",
		"title": "t", "summary": "s", "points": []any{"p"}, "approvers": []any{"Zed"}}}}).objs(t)
	if a[0]["reason"] != "INVALID" || !strings.Contains(a[0]["message"].(string), "no member 'Zed'") {
		t.Fatalf("unknown approver: %v", a)
	}

	// signal_get: word for word from the replica.
	m.withStore(func(st *store.Store) error { return seedApproval(st) })
	m.call("inbox", nil) // the approval request is raised once, here
	wantText(t, m.call("signal_get", map[string]any{"signal": S1}), "["+S1+"] DECISION · PENDING · topic \"Rounding\"",
		"  Needs Huy", "  Summary: Round half up", "asks approval from "+M1)
	m.srv.on("sync.poll", `{"result": {"events": [], "cursors": {}, "reset": []}}`)
	wantErr(t, m.call("signal_get", map[string]any{"signal": "sg_00000000000000000000000099"}),
		"no signal sg_00000000000000000000000099 in your projects")

	// signal_cite
	o := m.call("signal_cite", map[string]any{"signals": []any{S1, "sg_00000000000000000000000099"}}).obj(t)
	if !reflect.DeepEqual(o["cited"], []any{S1}) || !reflect.DeepEqual(o["unknown"], []any{"sg_00000000000000000000000099"}) {
		t.Fatalf("signal_cite %v", o)
	}

	// signal_list
	m.srv.on("signal.list", resultHas(t, "signal.list", S1))
	o = m.call("signal_list", map[string]any{"topic": "Rounding", "query": "round"}).obj(t)
	if p := m.params("signal.list"); p["topic"] != T1 || p["limit"] != float64(50) || p["query"] != "round" {
		t.Fatalf("signal_list params %v", p)
	}
	if s, _ := o["signals"].([]any); len(s) == 0 || !strings.Contains(s[0].(string), S1) {
		t.Fatalf("signal_list %v", o)
	}
}

// A session that follows only some topics looks up nothing outside them (Python's _in_scope).
func TestTopicScope(t *testing.T) {
	m := newMachine(t, true)
	m.withStore(func(st *store.Store) error { return seedApproval(st) }) // S1 is in Rounding (T1)
	m.session("narrow", P, []string{T2})
	m.find.id = "narrow"
	wantErr(t, m.call("signal_get", map[string]any{"signal": S1}), "this session follows only Currency")
	wantErr(t, m.call("signal_list", map[string]any{"topic": "Rounding"}), "this session follows only Currency")
	m.notSent("signal.list")
	a := m.call("signal_emit", map[string]any{"signals": []any{map[string]any{"kind": "NOTE", "topic": "Rounding",
		"title": "t", "summary": "s", "points": []any{"p"}}}}).objs(t)
	if a[0]["reason"] != "INVALID" || !strings.Contains(a[0]["message"].(string), "this session follows only Currency") {
		t.Fatalf("emit out of scope: %v", a)
	}
	m.notSent("signal.emit")
	// The listing drops what is outside the topics.
	m.srv.on("signal.list", resultHas(t, "signal.list", S1))
	o := m.call("signal_list", nil).obj(t)
	if s := o["signals"].([]any); len(s) != 0 {
		t.Fatalf("a signal outside the session's topics was listed: %v", s)
	}
	// Another project's session (or none) has no limit here.
	m.find.id = "s1"
	wantText(t, m.call("signal_get", map[string]any{"signal": S1}), S1)
}

func TestArtifactTools(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("artifact.write", ok(t, "artifact.write"))
	o := m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "The rules", "content": "# Rules\n",
		"name": "rules.md", "points": []any{"half up"}}).obj(t)
	p := m.params("artifact.write")
	sig := p["signal"].(map[string]any)
	body := sig["body"].(map[string]any)
	data, _ := base64.StdEncoding.DecodeString(p["content_b64"].(string))
	if string(data) != "# Rules\n" || p["media_type"] != "text/markdown" || sig["kind"] != "ARTIFACT" || body["name"] != "rules.md" ||
		p["actor"] != "AGENT" || sig["topic"] != T1 || o["accepted"] != true {
		t.Fatalf("artifact_write params %v answer %v", p, o)
	}

	// a file in the session's folder
	file := filepath.Join(m.dir, "..", "notes.txt")
	if err := os.WriteFile(file, []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "s", "path": file}).obj(t)
	if p := m.params("artifact.write"); p["signal"].(map[string]any)["body"].(map[string]any)["name"] != "notes.txt" {
		t.Fatalf("artifact_write path %v", p)
	}
	// Teamflow's home is never published.
	n := len(m.srv.sent("artifact.write"))
	wantErr(t, m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "s", "path": filepath.Join(m.dir, "config.json")}), "")
	wantErr(t, m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "s"}), "give path (a file) or content (text), not both")
	wantErr(t, m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "s", "content": "x", "path": file}), "not both")
	wantErr(t, m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "s", "content": "x"}), "name the artifact")
	// over 5 MB: nothing is sent; the answer says how to publish it as a link
	big := m.call("artifact_write", map[string]any{"topic": "Rounding", "summary": "s", "name": "big.txt",
		"content": strings.Repeat("x", protocol.MaxArtifactBytes+1)}).obj(t)
	if big["reason"] != "ARTIFACT_TOO_LARGE" || !strings.Contains(big["next"].(string), "artifact_publish") || big["size"] == nil {
		t.Fatalf("too large %v", big)
	}
	if got := len(m.srv.sent("artifact.write")); got != n {
		t.Fatalf("refused writes reached the server (%d → %d)", n, got)
	}

	m.srv.on("signal.emit", acceptedEmit)
	a := m.call("artifact_publish", map[string]any{"topic": "Rounding", "name": "demo.mp4", "summary": "The demo",
		"url": "https://files.example/demo.mp4", "size": 7000000}).obj(t)
	sig = m.params("signal.emit")["signal"].(map[string]any)
	body = sig["body"].(map[string]any)
	if a["accepted"] != true || body["url"] != "https://files.example/demo.mp4" || body["kind"] != "VIDEO" || body["size"] != float64(7000000) {
		t.Fatalf("artifact_publish %v %v", a, sig)
	}

	// artifact_get: text inline with the data note; save_to; never into the home
	m.withStore(func(st *store.Store) error { return seedArtifact(st) })
	m.srv.on("artifact.get", resultHas(t, "artifact.get", `"content_b64": "`))
	o = m.call("artifact_get", map[string]any{"id": S1}).obj(t)
	if o["note"] != dataNote || o["content"] == nil || !strings.Contains(o["about"].(string), "ARTIFACT") {
		t.Fatalf("artifact_get %v", o)
	}
	if p := m.params("artifact.get"); p["artifact"] != S1 || p["version"] != nil {
		t.Fatalf("by artifact id: the version in force: %v", p)
	}
	m.call("artifact_get", map[string]any{"id": S1, "exact_version": true}).obj(t)
	if p := m.params("artifact.get"); p["version"] != S1 {
		t.Fatalf("exact_version: %v", p)
	}
	out := filepath.Join(t.TempDir(), "sub", "spec.md")
	o = m.call("artifact_get", map[string]any{"id": S1, "save_to": out}).obj(t)
	if _, err := os.Stat(out); err != nil || o["saved_to"] != out {
		t.Fatalf("save_to %v %v", o, err)
	}
	wantErr(t, m.call("artifact_get", map[string]any{"id": S1, "save_to": filepath.Join(m.dir, "x.md")}), "Teamflow's home")
	if _, err := os.Stat(filepath.Join(m.dir, "x.md")); err == nil {
		t.Fatal("written into the home")
	}
	m.srv.on("artifact.get", refused(t, "artifact.get", "CONTENT_MISMATCH"))
	wantErr(t, m.call("artifact_get", map[string]any{"id": S1}), "CONTENT_MISMATCH")

	m.srv.on("artifact.list", ok(t, "artifact.list"))
	l := m.call("artifact_list", map[string]any{"topic": "Rounding", "include_archived": true}).objs(t)
	if p := m.params("artifact.list"); p["topic"] != T1 || p["include_archived"] != true || len(l) == 0 || l[0]["artifact"] != S1 {
		t.Fatalf("artifact_list %v %v", p, l)
	}

	m.srv.on("artifact.archive", ok(t, "artifact.archive"))
	o = m.call("artifact_archive", map[string]any{"artifact": S1, "note": "replaced"}).obj(t)
	if p := m.params("artifact.archive"); p["archived"] != true || p["note"] != "replaced" || o["artifact"] != S1 {
		t.Fatalf("artifact_archive %v %v", p, o)
	}
	m.call("artifact_archive", map[string]any{"artifact": S1, "archived": false})
	if p := m.params("artifact.archive"); p["archived"] != false {
		t.Fatalf("restore %v", p)
	}
	m.srv.on("artifact.archive", refused(t, "artifact.archive", "NOT_ALLOWED"))
	wantErr(t, m.call("artifact_archive", map[string]any{"artifact": S1}), "NOT_ALLOWED")
}

// seedArtifact puts S1 as an artifact (spec.md, TEXT) in Rounding.
func seedArtifact(st *store.Store) error {
	sig := protocol.NewSignal()
	sig.ID, sig.Project, sig.Topic, sig.Kind, sig.Title = S1, P, T1, protocol.SignalKindArtifact, "spec.md"
	sig.Body = protocol.ArtifactBody{Type: "artifact.v1", Name: "spec.md", Kind: protocol.ArtifactKindText, Summary: "The spec",
		Points: []string{}}
	s := protocol.NewStoredSignal()
	s.Signal, s.Author.Member, s.Author.Actor = sig, M2, protocol.ActorAgent
	s.Seq, s.Chain, s.ChainVersion, s.State = 5, S1, 1, protocol.SignalStateInForce
	ev := protocol.NewSignalAccepted()
	ev.Project, ev.Seq, ev.Type, ev.Signal = P, 5, "signal.accepted", s
	return st.Tx(func(tx *store.Tx) error { return applyEvent(tx, ev) })
}

// seedDeliverables puts the golden first version: Design (A1 "Draft rounding rules", Huy's, in Rounding; A2 "List
// currencies", no owner, in Currency) and Ship (A3 "Implement rounding", Lan's, in Rounding).
func seedDeliverables(t *testing.T, m *machine) {
	t.Helper()
	var view protocol.DeliverablesView
	result(t, golden(t, "deliverables.get", "version 1", func(g rpcGolden) bool {
		return g.Response.Error == nil && strings.Contains(string(g.Response.Result), `"version": 1,`)
	}), &view)
	m.withStore(func(st *store.Store) error {
		ev := protocol.NewDeliverablesPublished()
		ev.Project, ev.Seq, ev.Type, ev.Deliverables = P, 5, "deliverables.published", *view.Deliverables
		return st.Tx(func(tx *store.Tx) error { return applyEvent(tx, ev) })
	})
}

const (
	A1 = "ai_00000000000000000000000001"
	A3 = "ai_00000000000000000000000003"
)

func TestDeliverablesTools(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("deliverables.get", resultHas(t, "deliverables.get", `"version": 1,`))
	o := m.call("deliverables_get", nil).obj(t)
	ms := o["milestones"].([]any)
	first := ms[0].(map[string]any)["actions"].([]any)[0].(map[string]any)
	if o["version"] != float64(1) || o["note"] != namesNote || first["name"] != "Draft rounding rules" || first["owner"] != "huy" ||
		first["topic"] != "Rounding" || first["status"] != "NOT_STARTED" || first["yours"] != false {
		t.Fatalf("deliverables_get %v", o)
	}
	m.srv.on("deliverables.get", resultHas(t, "deliverables.get", `"deliverables": null`))
	o = m.call("deliverables_get", nil).obj(t)
	if o["deliverables"] != nil || !strings.Contains(o["message"].(string), "no Deliverables yet") {
		t.Fatalf("none yet %v", o)
	}

	spec := []any{map[string]any{"name": "Design", "due": "2099-01-25", "actions": []any{
		map[string]any{"name": "Draft rules", "topic": "Rounding", "due": "2099-01-20", "owner": "huy"}}}}
	o = m.call("deliverables_validate", map[string]any{"milestones": spec}).obj(t)
	if o["valid"] != true || len(o["problems"].([]any)) != 0 {
		t.Fatalf("deliverables_validate %v", o)
	}
	o = m.call("deliverables_validate", map[string]any{"milestones": []any{map[string]any{"name": "Design", "due": "2099-01-25",
		"actions": []any{map[string]any{"name": "Draft rules", "topic": "Nope", "due": "2099-01-20"}}}}}).obj(t)
	if o["valid"] != false || !strings.Contains(strings.Join(anyStrings(o["problems"]), ";"), "Nope") {
		t.Fatalf("an invalid structure %v", o)
	}
	m.notSent("deliverables.publish")

	m.srv.on("deliverables.publish", ok(t, "deliverables.publish"))
	o = m.call("deliverables_publish", map[string]any{"milestones": spec}).obj(t)
	if p := m.params("deliverables.publish"); o["published"] != true || p["base_version"] != nil ||
		p["milestones"].([]any)[0].(map[string]any)["actions"].([]any)[0].(map[string]any)["topic"] != "Rounding" {
		t.Fatalf("deliverables_publish %v %v", o, p)
	}
	// invalid: problems, nothing sent
	m.srv.reset()
	o = m.call("deliverables_publish", map[string]any{"milestones": []any{map[string]any{"name": "Design", "due": "2099-01-25",
		"actions": []any{map[string]any{"name": "A", "topic": "Rounding", "due": "2099-01-20", "after": []any{"B"}}}}}}).obj(t)
	if o["published"] != false || o["message"] != notSentNote || len(o["problems"].([]any)) == 0 {
		t.Fatalf("invalid publish %v", o)
	}
	m.notSent("deliverables.publish")

	// With Deliverables: the version read is required, and must be the current one.
	seedDeliverables(t, m)
	o = m.call("deliverables_publish", map[string]any{"milestones": spec}).obj(t)
	if o["published"] != false || !strings.Contains(o["message"].(string), "must say which version it is based on") {
		t.Fatalf("no base %v", o)
	}
	o = m.call("deliverables_publish", map[string]any{"milestones": spec, "base_version": 7}).obj(t)
	if !strings.Contains(o["message"].(string), "at version 1, not version 7") {
		t.Fatalf("stale base %v", o)
	}
	m.notSent("deliverables.publish")
	// The server's own refusal comes back as it is.
	m.srv.on("deliverables.publish", refused(t, "deliverables.publish", "STALE_DELIVERABLES_VERSION"))
	o = m.call("deliverables_publish", map[string]any{"milestones": spec, "base_version": 1}).obj(t)
	if o["published"] != false || o["reason"] != "STALE_DELIVERABLES_VERSION" {
		t.Fatalf("server refusal %v", o)
	}

	m.srv.on("deliverables.update_milestone", ok(t, "deliverables.update_milestone"))
	o = m.call("deliverables_update_milestone", map[string]any{"base_version": 1, "milestone": map[string]any{
		"id": "ms_00000000000000000000000002", "name": "Ship it", "due": "2099-02-04", "actions": []any{
			map[string]any{"id": A3, "name": "Implement rounding", "topic": "Rounding", "due": "2099-01-30", "owner": "lan"}}}}).obj(t)
	if p := m.params("deliverables.update_milestone"); o["published"] != true || p["base_version"] != float64(1) ||
		p["milestone"].(map[string]any)["name"] != "Ship it" {
		t.Fatalf("deliverables_update_milestone %v %v", o, p)
	}
	o = m.call("deliverables_update_milestone", map[string]any{"base_version": 1, "milestone": map[string]any{
		"name": "New", "due": "2099-02-04", "actions": []any{map[string]any{"name": "X", "topic": "Rounding", "due": "2099-01-30"}}}}).obj(t)
	if o["published"] != false || !strings.Contains(strings.Join(anyStrings(o["problems"]), ";"), "the milestone's id is required") {
		t.Fatalf("a milestone without its id %v", o)
	}

	m.srv.on("deliverables.rename", ok(t, "deliverables.rename"))
	o = m.call("deliverables_rename", map[string]any{"base_version": 1, "names": map[string]any{"draft ROUNDING rules": "Draft the rules"}}).obj(t)
	if p := m.params("deliverables.rename"); o["published"] != true || !reflect.DeepEqual(p["names"], map[string]any{A1: "Draft the rules"}) {
		t.Fatalf("rename by name %v %v", o, p)
	}
	o = m.call("deliverables_rename", map[string]any{"base_version": 1, "names": map[string]any{"ai_00000000000000000000000009": "X"}}).obj(t)
	if o["published"] != false || !strings.Contains(strings.Join(anyStrings(o["problems"]), ";"), "no item ai_00000000000000000000000009") {
		t.Fatalf("rename unknown %v", o)
	}

	// A narrow session publishes nothing whole, and changes only its own topics.
	m.session("narrow", P, []string{T2})
	m.find.id = "narrow"
	o = m.call("deliverables_publish", map[string]any{"milestones": spec, "base_version": 1}).obj(t)
	if !strings.Contains(o["message"].(string), "publishing the whole Deliverables needs a session on every topic") {
		t.Fatalf("narrow publish %v", o)
	}
	o = m.call("deliverables_rename", map[string]any{"base_version": 1, "names": map[string]any{A1: "Mine now"}}).obj(t)
	if !strings.Contains(o["message"].(string), "'Mine now' is in other topics") {
		t.Fatalf("narrow rename %v", o)
	}

	// The formerly web-only actions.
	m.find.id = "s1"
	m.srv.on("deliverables.archive", ok(t, "deliverables.archive"))
	o = m.call("deliverables_archive", map[string]any{"note": "paused"}).obj(t)
	if p := m.params("deliverables.archive"); p["archived"] != true || p["note"] != "paused" || o["archived"] != true {
		t.Fatalf("deliverables_archive %v %v", p, o)
	}
	m.call("deliverables_archive", map[string]any{"archived": false})
	if p := m.params("deliverables.archive"); p["archived"] != false {
		t.Fatalf("restore %v", p)
	}
	m.srv.on("deliverables.delete", ok(t, "deliverables.delete"))
	if o := m.call("deliverables_delete", nil).obj(t); o["deleted"] != true || m.params("deliverables.delete")["project"] != P {
		t.Fatalf("deliverables_delete %v", o)
	}
	m.srv.on("deliverables.delete", refused(t, "deliverables.delete", "NOT_ALLOWED"))
	wantErr(t, m.call("deliverables_delete", nil), "NOT_ALLOWED")
	m.srv.on("deliverables.history", resultHas(t, "deliverables.history", `"published_by": "mb_`))
	h := m.call("deliverables_history", nil).objs(t)
	if len(h) == 0 || h[0]["published_by"] != "lan" {
		t.Fatalf("deliverables_history %v", h)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestActionTools(t *testing.T) {
	m := newMachine(t, true)
	seedDeliverables(t, m)
	m.srv.on("signal.emit", acceptedEmit)
	o := m.call("action_result", map[string]any{"action": "implement ROUNDING", "outcome": "done", "summary": "Shipped"}).obj(t)
	sig := m.params("signal.emit")["signal"].(map[string]any)
	body := sig["body"].(map[string]any)
	if o["accepted"] != true || sig["kind"] != "ACTION_RESULT" || sig["title"] != "Done: Implement rounding" || body["action"] != A3 ||
		body["outcome"] != "DONE" || sig["topic"] != T1 {
		t.Fatalf("action_result %v %v", o, sig)
	}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": A3, "outcome": "MAYBE", "summary": "s"}, "outcome 'MAYBE': DONE, PARTIAL or BLOCKED"},
		{map[string]any{"action": "Nope", "outcome": "DONE", "summary": "s"}, "no action 'Nope' in the current Deliverables (version 1)"},
	} {
		o := m.call("action_result", tc.args).obj(t)
		if o["accepted"] != false || o["reason"] != "INVALID" || !strings.Contains(o["message"].(string), tc.want) {
			t.Fatalf("action_result %v: %v", tc.args, o)
		}
	}
	o = m.call("action_revoke", map[string]any{"action": A1, "reason": "  "}).obj(t)
	if o["reason"] != "INVALID" || !strings.Contains(o["message"].(string), "a revoke needs a reason") {
		t.Fatalf("revoke without a reason %v", o)
	}
	o = m.call("action_revoke", map[string]any{"action": A1, "reason": "Rules ignore half cents"}).obj(t)
	if sig := m.params("signal.emit")["signal"].(map[string]any); o["accepted"] != true || sig["kind"] != "ACTION_REVOKE" ||
		sig["title"] != "Revoked: Draft rounding rules" {
		t.Fatalf("action_revoke %v %v", o, sig)
	}
	// archived Deliverables: no results
	m.withStore(func(st *store.Store) error { return st.SetDeliverablesArchived(P, true, "") })
	o = m.call("action_result", map[string]any{"action": A3, "outcome": "DONE", "summary": "s"}).obj(t)
	if !strings.Contains(o["message"].(string), "the Deliverables are archived") {
		t.Fatalf("archived %v", o)
	}
}

func TestDriftAndTaskTools(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("drift.list", ok(t, "drift.list"))
	l := m.call("drift_list", map[string]any{"status": "OPEN"}).objs(t)
	if p := m.params("drift.list"); p["status"] != "OPEN" || len(l) == 0 || l[0]["id"] != D1 {
		t.Fatalf("drift_list %v %v", p, l)
	}
	m.srv.on("drift.resolve", refused(t, "drift.resolve", "DRIFT_CLOSED"))
	wantErr(t, m.call("drift_resolve", map[string]any{"drift": D1, "note": "done"}), "DRIFT_CLOSED")
	wantErr(t, m.call("drift_dismiss", map[string]any{"drift": D1, "note": " "}), "a note is needed")
	m.notSent("drift.dismiss")

	m.srv.on("task.list", ok(t, "task.list"))
	tl := m.call("task_list", map[string]any{"state": "DONE"}).objs(t)
	if p := m.params("task.list"); p["state"] != "DONE" || len(tl) == 0 || tl[0]["kind"] != "ROLLING_UPDATE_PROJECT" {
		t.Fatalf("task_list %v %v", p, tl)
	}
}

func TestProjectPeoplePlanAndTopicDelete(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("project.set_title", ok(t, "project.set_title"))
	m.call("project_set_title", map[string]any{"member": "HUY", "title": "Tech lead"}).obj(t)
	if p := m.params("project.set_title"); p["member"] != M2 || p["title"] != "Tech lead" {
		t.Fatalf("project_set_title %v", p)
	}
	wantErr(t, m.call("project_set_title", map[string]any{"member": "Zed", "title": "x"}), "no member 'Zed'")

	m.srv.on("project.get", pricing(t))
	m.srv.on("project.remove_member", ok(t, "project.remove_member"))
	o := m.call("project_remove_member", map[string]any{"member": "huy"}).obj(t)
	if p := m.params("project.remove_member"); p["member"] != M2 || p["expected_version"] != float64(1) || o["members"] == nil {
		t.Fatalf("project_remove_member %v %v", p, o)
	}

	m.srv.on("project.write_state", ok(t, "project.write_state"))
	o = m.call("project_write_state", map[string]any{"base_version": 1, "plan": "Ship v2", "instruction": "Be brief"}).obj(t)
	if p := m.params("project.write_state"); p["base_version"] != float64(1) || p["plan"] != "Ship v2" || o["state_version"] == nil {
		t.Fatalf("project_write_state %v %v", p, o)
	}
	m.srv.on("project.write_state", refused(t, "project.write_state", "STALE_STATE_VERSION"))
	wantErr(t, m.call("project_write_state", map[string]any{"base_version": 1, "plan": "p", "instruction": "i"}), "STALE_STATE_VERSION")
	wantErr(t, m.call("project_write_state", map[string]any{"plan": "p", "instruction": "i"}), "base_version")

	m.srv.on("topic.delete", ok(t, "topic.delete"))
	o = m.call("topic_delete", map[string]any{"topic": "Rounding", "expected_version": 1}).obj(t)
	if p := m.params("topic.delete"); p["topic"] != T1 || p["expected_version"] != float64(1) || o["deleted"] != "Rounding" ||
		o["signals_deleted"] != float64(2) {
		t.Fatalf("topic_delete %v %v", p, o)
	}
	m.srv.on("topic.delete", refused(t, "topic.delete", "STALE_PROJECT_VERSION"))
	wantErr(t, m.call("topic_delete", map[string]any{"topic": "Rounding", "expected_version": 6}), "STALE_PROJECT_VERSION")
	wantErr(t, m.call("topic_delete", map[string]any{"topic": "Rounding"}), "expected_version")
}

func TestOrgAndProfileTools(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.me", lanMe(t))
	m.srv.on("org.members", ok(t, "org.members"))
	m.srv.on("org.set_title", ok(t, "org.set_title"))
	o := m.call("org_set_title", map[string]any{"member": "huy@acme.test", "title": "Engineer"}).obj(t)
	if p := m.params("org.set_title"); p["org"] != "Acme" || p["member"] != M2 || p["title"] != "Engineer" || o["org"] != "Acme" {
		t.Fatalf("org_set_title %v %v", p, o)
	}
	m.srv.on("org.add_member", ok(t, "org.add_member"))
	o = m.call("org_add_member", map[string]any{"email": "mai@acme.test", "title": "PM", "org": "Acme"}).obj(t)
	if p := m.params("org.add_member"); p["email"] != "mai@acme.test" || p["title"] != "PM" || o["member"] == nil {
		t.Fatalf("org_add_member %v %v", p, o)
	}
	m.srv.on("org.add_member", refused(t, "org.add_member", "UNKNOWN_REF"))
	wantErr(t, m.call("org_add_member", map[string]any{"email": "ghost@acme.test"}), "UNKNOWN_REF")
	m.srv.on("org.remove_member", ok(t, "org.remove_member"))
	o = m.call("org_remove_member", map[string]any{"member": "Mai"}).obj(t)
	if p := m.params("org.remove_member"); p["member"] != "mb_00000000000000000000000003" || o["removed"] != "mai" {
		t.Fatalf("org_remove_member %v %v", p, o)
	}
	m.srv.on("org.remove_member", refused(t, "org.remove_member", "NOT_ALLOWED"))
	wantErr(t, m.call("org_remove_member", map[string]any{"member": "lan"}), "NOT_ALLOWED")

	m.srv.on("account.update_profile", ok(t, "account.update_profile"))
	o = m.call("profile_update", map[string]any{"name": "Huy Nguyen"}).obj(t)
	if p := m.params("account.update_profile"); p["name"] != "Huy Nguyen" || p["alias"] != nil || o["name"] != "Huy Nguyen" {
		t.Fatalf("profile_update %v %v", p, o)
	}
	if c := m.srv.sent("account.update_profile"); c[0].Auth != "Bearer "+auth {
		t.Fatalf("the profile goes with the auth key: %q", c[0].Auth)
	}
	wantErr(t, m.call("profile_update", nil), "give a new name or alias")
	wantErr(t, m.call("profile_update", map[string]any{"name": " "}), "a name cannot be empty")

	// More than one org (or none): which one?
	m.srv.on("account.me", `{"result": {"account": {"id": "ac_1", "email": "lan@acme.test", "name": "lan", "alias": "", "avatar": null, "created_at": "2026-01-15T09:00:00Z", "disabled_at": null}, "orgs": [], "protocol": 1, "min_client_protocol": 1}}`)
	wantErr(t, m.call("org_set_title", map[string]any{"member": "huy", "title": "x"}), "which org? you are in: none")
}

// artifact_get's save_to never writes into Teamflow's home (its keys): not through a link to a file there, a
// linked folder, a case variant of its path, or a hard link to one of its files; and never over an existing file
// unless overwrite says so.
func TestSaveToNeverReachesTheHome(t *testing.T) {
	m := newMachine(t, true)
	m.withStore(func(st *store.Store) error { return seedArtifact(st) })
	m.srv.on("artifact.get", resultHas(t, "artifact.get", `"content_b64": "`))
	config := filepath.Join(m.dir, "config.json")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	linkFile := filepath.Join(work, "out.json")
	if err := os.Symlink(config, linkFile); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(work, "into-home")
	if err := os.Symlink(m.dir, linkDir); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(work, "hard.json")
	if err := os.Link(config, hard); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]any{
		"a link to a file in the home":             {"id": S1, "save_to": linkFile, "overwrite": true},
		"a new file through a linked folder":       {"id": S1, "save_to": filepath.Join(linkDir, "new.md")},
		"an existing file through a linked folder": {"id": S1, "save_to": filepath.Join(linkDir, "config.json"), "overwrite": true},
		"a hard link to a file in the home":        {"id": S1, "save_to": hard, "overwrite": true},
		"the home itself":                          {"id": S1, "save_to": filepath.Join(m.dir, "x.md")},
		"a folder made under a link":               {"id": S1, "save_to": filepath.Join(linkDir, "sub", "x.md")},
	}
	if upper := strings.ToUpper(m.dir); upper != m.dir {
		if fi, err := os.Stat(upper); err == nil { // a case-insensitive file system (macOS)
			if home, _ := os.Stat(m.dir); os.SameFile(fi, home) {
				cases["a case variant of the home"] = map[string]any{"id": S1, "save_to": filepath.Join(upper, "y.md")}
			}
		}
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			r := m.call("artifact_get", a)
			if !r.err {
				t.Fatalf("written: %s", r)
			}
		})
	}
	after, _ := os.ReadFile(config)
	if string(after) != string(before) {
		t.Fatal("config.json was changed")
	}
	for _, p := range []string{filepath.Join(m.dir, "new.md"), filepath.Join(m.dir, "x.md"), filepath.Join(m.dir, "sub")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was made in the home", p)
		}
	}

	// An existing file is kept unless overwrite says so; a plain new file is written.
	existing := filepath.Join(work, "spec.md")
	if err := os.WriteFile(existing, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr(t, m.call("artifact_get", map[string]any{"id": S1, "save_to": existing}), "already exists")
	if b, _ := os.ReadFile(existing); string(b) != "mine" {
		t.Fatal("an existing file was replaced without overwrite")
	}
	if o := m.call("artifact_get", map[string]any{"id": S1, "save_to": existing, "overwrite": true}).obj(t); o["saved_to"] != existing {
		t.Fatalf("overwrite %v", o)
	}
	if b, _ := os.ReadFile(existing); string(b) == "mine" {
		t.Fatal("overwrite did not write")
	}
}
