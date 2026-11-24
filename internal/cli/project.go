package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/artifacts"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/deliverables"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

var (
	projectFlag = flagSpec{name: "project", help: "Project id (default: this session's project)"}
	yesFlag     = flagSpec{name: "yes", kind: flag, help: "Skip the confirmation"}
)

// where is the project to act on: the one given, or the shared session this runs in; and that session's id when
// it is on that project.
func (r *inv) where(project string) (string, string, bool) {
	sess, ok := r.session()
	pid := project
	if pid == "" && ok {
		pid = sess.Project
	}
	if pid == "" {
		return "", "", false
	}
	sid := ""
	if ok && sess.Project == pid {
		sid = sess.ID
	}
	return pid, sid, true
}

const whichProject = "which project? pass --project, or run this from a shared session"

// ------------------------------------------------------------------ topic delete

var topicCmd = &command{name: "topic", help: "Topics (initiator only).", sub: []*command{{
	name: "delete", args: "<project> <topic>", help: "Delete a topic and every signal in it, for everyone. Cannot be undone.",
	flags: []flagSpec{yesFlag}, nargs: exactly(2), run: runTopicDelete,
}}}

func runTopicDelete(r *inv, a *parsed) int {
	project, topic := a.pos[0], a.pos[1]
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	cl, err := c.API(r.ctx, project, core.DefaultTimeout)
	if err != nil {
		return r.failErr(err)
	}
	var view protocol.ProjectView
	if err := cl.Call(r.ctx, "project.get", protocol.ProjectGet{Project: project}, &view); err != nil {
		return r.failErr(err)
	}
	var match *protocol.Topic
	for i, t := range view.Topics {
		if topic == t.ID || topic == t.Name {
			match = &view.Topics[i]
			break
		}
	}
	if match == nil {
		return r.fail(fmt.Sprintf("no topic %s in %s", quote(topic), view.Project.Name))
	}
	if !a.has("yes") {
		if ok, why := r.confirm(fmt.Sprintf("Delete topic %s and all its signals for everyone?", quote(match.Name))); !ok {
			return r.fail(why)
		}
	}
	var out protocol.TopicDeleted
	err = cl.Call(r.ctx, "topic.delete", protocol.TopicDelete{Project: project, ExpectedVersion: view.Project.Version,
		Topic: match.ID}, &out)
	if err != nil {
		return r.failErr(err)
	}
	r.printf("deleted %s: %d signal(s), %d drift(s)\n", quote(match.Name), out.SignalsDeleted, out.DriftsDeleted)
	return 0
}

// quote names a thing as Python's repr of a plain string does.
func quote(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// ------------------------------------------------------------------ artifacts

var (
	topicReq   = flagSpec{name: "topic", help: "Topic name or id (required)"}
	summaryReq = flagSpec{name: "summary", help: "What it is, at most 240 characters (required)"}
	pointFlag  = flagSpec{name: "point", kind: list, help: "A key point (up to 7; repeatable)"}
	kindFlag   = flagSpec{name: "kind", help: "TEXT, DATA, CODE, DOCUMENT, PRESENTATION, IMAGE, AUDIO, VIDEO or LINK (default: from the name)"}
	supersedes = flagSpec{name: "supersedes", help: "The newest version's sg_ id"}
)

var artifactCmd = &command{name: "artifact", help: "Project artifacts: files teammates rely on.", sub: []*command{
	{name: "write", args: "<file>", help: "Store a file (any type, up to 5 MB) in Teamflow's file store. Larger: keep it " +
		"where your team can reach it and publish it with `teamflow artifact link`.",
		flags: []flagSpec{topicReq, summaryReq, pointFlag, {name: "name", help: "Default: the file's name"}, kindFlag,
			supersedes, projectFlag}, nargs: exactly(1), run: runArtifactWrite},
	{name: "link", args: "<url>", help: "Publish an artifact that lives elsewhere (an http or https link). Teamflow stores nothing.",
		flags: []flagSpec{{name: "name", help: "The artifact's name, e.g. demo.mp4 (required)"}, topicReq, summaryReq, pointFlag,
			{name: "file", help: "A local copy, to record its size and SHA-256"}, kindFlag, supersedes, projectFlag},
		nargs: exactly(1), run: runArtifactLink},
	{name: "pull", args: "<artifact>", help: "Save an artifact's content (the version in force, or --exact for one version).",
		flags: []flagSpec{{name: "output", short: "o", help: "Where to save it (default: its name, here)"},
			{name: "exact", kind: flag, help: "This exact version, not the one in force"}}, nargs: exactly(1), run: runArtifactPull},
	{name: "archive", args: "<artifact>", help: "Archive an artifact (initiator only): hidden from lists and plan updates, still readable.",
		flags: []flagSpec{{name: "note", help: "Why, at most 240 characters"}, {name: "unarchive", kind: flag, help: "Bring it back"}},
		nargs: exactly(1), run: runArtifactArchive},
	{name: "list", help: "The project's artifacts.",
		flags: []flagSpec{{name: "topic", help: "Topic name or id"}, {name: "all", kind: flag, help: "Include archived ones"}, projectFlag},
		nargs: exactly(0), run: runArtifactList},
}}

var artifactKinds = []protocol.ArtifactKind{protocol.ArtifactKindText, protocol.ArtifactKindData,
	protocol.ArtifactKindCode, protocol.ArtifactKindDocument, protocol.ArtifactKindPresentation, protocol.ArtifactKindImage,
	protocol.ArtifactKindAudio, protocol.ArtifactKindVideo, protocol.ArtifactKindLink}

func artifactKind(r *inv, path string, a *parsed) (protocol.ArtifactKind, bool) {
	k := protocol.ArtifactKind(strings.ToUpper(a.get("kind")))
	if k == "" {
		return "", true
	}
	for _, known := range artifactKinds {
		if k == known {
			return k, true
		}
	}
	usageFail(r, path, "Invalid value for '--kind': "+a.get("kind"))
	return "", false
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func signalLine(id, name string, kind protocol.ArtifactKind, state *protocol.SignalState) string {
	st := ""
	if state != nil {
		st = string(*state)
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s", id, name, kind, st)
}

func refusal(reason *protocol.RejectReason, message string) string {
	r := ""
	if reason != nil {
		r = string(*reason)
	}
	return "refused: " + r + " " + message
}

func runArtifactWrite(r *inv, a *parsed) int {
	kind, ok := artifactKind(r, "artifact write", a)
	if !ok {
		return 2
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	if c.SignedIn() {
		c.Sync(r.ctx, core.DefaultTimeout, "")
	}
	pid, sid, ok := r.where(a.get("project"))
	if !ok {
		return r.fail(whichProject)
	}
	cwd, _ := os.Getwd()
	file := a.pos[0]
	p, err := artifacts.Prepare(file, artifacts.Options{Name: a.get("name"), Kind: kind, Project: cwd, Home: c.Dir})
	var tooLarge *artifacts.TooLargeError
	if errors.As(err, &tooLarge) {
		return r.fail(artifacts.LinkHint(tooLarge.Size, tooLarge.Name, a.get("topic"), file))
	}
	if err != nil {
		return r.failErr(err)
	}
	d := artifacts.Signal(p.Name, a.get("topic"), p.Kind, a.get("summary"), a.lists["point"], "", optional(a.get("supersedes")), nil, artifacts.Content{})
	res, err := c.ArtifactWrite(r.ctx, pid, sid, d, p, protocol.ActorMember)
	if err != nil {
		return r.failErr(err)
	}
	if res.Reason != nil && *res.Reason == protocol.RejectReasonArtifactTooLarge {
		return r.fail(artifacts.LinkHint(p.Size, p.Name, a.get("topic"), file))
	}
	if !res.Accepted {
		return r.fail(refusal(res.Reason, res.Message))
	}
	r.println(signalLine(res.Signal, p.Name, p.Kind, res.State))
	return 0
}

func runArtifactLink(r *inv, a *parsed) int {
	if a.get("name") == "" {
		return usageFail(r, "artifact link", "Missing option '--name'")
	}
	kind, ok := artifactKind(r, "artifact link", a)
	if !ok {
		return 2
	}
	link := a.pos[0]
	if u, err := url.Parse(link); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return r.fail("the link must be an http or https URL: " + link)
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	if c.SignedIn() {
		c.Sync(r.ctx, core.DefaultTimeout, "")
	}
	pid, sid, ok := r.where(a.get("project"))
	if !ok {
		return r.fail(whichProject)
	}
	content, k, err := artifacts.Link(link, a.get("name"), a.get("file"), kind)
	if err != nil {
		return r.failErr(err)
	}
	d := artifacts.Signal(a.get("name"), a.get("topic"), k, a.get("summary"), a.lists["point"], "", optional(a.get("supersedes")), nil, content)
	res, err := c.ArtifactPublish(r.ctx, pid, sid, d, protocol.ActorMember)
	if err != nil {
		return r.failErr(err)
	}
	if !res.Accepted {
		return r.fail(refusal(res.Reason, res.Message))
	}
	r.println(signalLine(res.Signal, a.get("name"), k, res.State))
	return 0
}

func runArtifactPull(r *inv, a *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	got, err := c.ArtifactGet(r.ctx, a.pos[0], a.has("exact"), true)
	if err != nil {
		return r.failErr(err)
	}
	pulled, err := artifacts.Pull(got, a.get("output"))
	if err != nil {
		return r.failErr(err)
	}
	r.println(pulled.Line())
	return 0
}

func runArtifactArchive(r *inv, a *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	out, err := c.ArtifactArchive(r.ctx, a.pos[0], !a.has("unarchive"), a.get("note"))
	if err != nil {
		return r.failErr(err)
	}
	done := "restored"
	if out.Archived {
		done = "archived"
	}
	r.printf("%s %s\t%s\n", done, out.Artifact, out.Name)
	return 0
}

func runArtifactList(r *inv, a *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	if c.SignedIn() {
		c.Sync(r.ctx, core.DefaultTimeout, "")
	}
	pid, _, ok := r.where(a.get("project"))
	if !ok {
		return r.fail(whichProject)
	}
	out, err := c.ArtifactList(r.ctx, pid, a.get("topic"), a.has("all"))
	if err != nil {
		return r.failErr(err)
	}
	for _, s := range out.Artifacts {
		flag := ""
		if s.Archived {
			flag = "\tarchived"
		}
		r.printf("%s\t%s\t%s\tv%d\t%s%s\n", s.Artifact, s.Name, s.Kind, s.Versions, s.Latest.State, flag)
	}
	return 0
}

// ------------------------------------------------------------------ deliverables

var deliverablesValidate = &command{name: "validate", args: "<file>",
	help: "Check a Deliverables structure (JSON: the list of milestones, as agents publish it) against the project: " +
		"every problem (exit 1), or valid. Nothing is published.",
	flags: []flagSpec{projectFlag, jsonFlag}, nargs: exactly(1), run: runDeliverablesValidate}

var deliverablesCmd = &command{name: "deliverables",
	help:  "Show the project's Deliverables: each milestone and its actions, with status, owner and due.",
	flags: []flagSpec{projectFlag, jsonFlag}, sub: []*command{deliverablesValidate}, run: runDeliverables}

func runDeliverables(r *inv, a *parsed) int {
	if len(a.pos) > 0 && a.pos[0] == "validate" { // `deliverables --project X validate f`
		if len(a.pos) != 2 {
			return usageFail(r, "deliverables validate", "wrong number of arguments")
		}
		a.pos = a.pos[1:]
		return runDeliverablesValidate(r, a)
	}
	if len(a.pos) > 0 {
		return usageFail(r, "deliverables", "No such command '"+a.pos[0]+"'.")
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	pid, _, ok := r.where(a.get("project"))
	if !ok {
		return r.fail(whichProject)
	}
	view, err := c.DeliverablesGet(r.ctx, pid)
	if errors.Is(err, api.ErrOffline) {
		return r.fail("the Teamflow server is unreachable; try again when it is back")
	}
	if err != nil {
		return r.failErr(err)
	}
	c.Sync(r.ctx, core.HookTimeout, pid) // names and topics for the lines below
	d := view.Deliverables
	if d == nil {
		r.println("This project has no Deliverables yet (or they are archived). Agents publish them from the plan.")
		return 0
	}
	names, _ := c.St.Names()
	topics := map[string]string{}
	if ts, err := c.St.Topics(pid); err == nil {
		for _, t := range ts {
			topics[t.ID] = t.Name
		}
	}
	late := map[string]bool{}
	for _, id := range view.Late {
		late[id] = true
	}
	or := func(m map[string]string, k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return k
	}
	mark := func(id string) string {
		if late[id] {
			return " LATE"
		}
		return ""
	}
	archived := ""
	if d.Archived {
		archived = " (archived)"
	}
	r.printf("Deliverables v%d: %d of %d milestones complete%s\n", d.Version, d.MilestonesDone, d.MilestonesTotal, archived)
	for _, m := range d.Milestones {
		r.printf("%s\tdue %s\t%d/%d done%s\n", m.Name, m.Due, m.Done, m.Total, mark(m.ID))
		for _, act := range m.Actions {
			status := protocol.ActionStatusNotStarted
			if s, ok := view.Statuses[act.ID]; ok {
				status = s
			}
			owner := "no owner"
			if act.Owner != nil {
				owner = or(names, *act.Owner)
			}
			r.printf("  %s\t%s%s\t%s\tdue %s\t%s\t%s\n", act.Name, status, mark(act.ID), owner, act.Due, or(topics, act.Topic), act.ID)
		}
	}
	return 0
}

func runDeliverablesValidate(r *inv, a *parsed) int {
	file := a.pos[0]
	f, err := os.Open(file)
	var text []byte
	if err == nil {
		text, err = io.ReadAll(f)
		f.Close()
	}
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return r.fail(fmt.Sprintf("cannot read %s: %v", home.Display(file), err))
	}
	var data any
	if err := json.Unmarshal(text, &data); err != nil {
		return r.fail(fmt.Sprintf("%s is not JSON (%v): it must hold the list of milestones", file, err))
	}
	milestones := data
	if obj, ok := data.(map[string]any); ok {
		milestones = obj["milestones"]
	}
	list, ok := milestones.([]any)
	for _, m := range list {
		if _, isObj := m.(map[string]any); !isObj {
			ok = false
		}
	}
	if !ok {
		return r.fail(fmt.Sprintf(`%s must hold a list of milestones (or an object with "milestones")`, file))
	}
	raw, _ := json.Marshal(list)
	if obj, isObj := data.(map[string]any); isObj { // keep the order as written
		var w struct {
			Milestones json.RawMessage `json:"milestones"`
		}
		if json.Unmarshal(text, &w) == nil {
			raw = w.Milestones
		}
		_ = obj
	} else {
		raw = text
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	pid, _, ok := r.where(a.get("project"))
	if !ok {
		return r.fail(whichProject)
	}
	problems, err := c.DeliverablesValidate(r.ctx, pid, raw)
	if err != nil {
		return r.fail("cannot check it against the project: " + errText(err))
	}
	if a.has("json") {
		b, _ := json.Marshal(deliverables.Result{Valid: len(problems) == 0, Problems: append([]string{}, problems...)})
		r.println(string(b))
		if len(problems) > 0 {
			return 1
		}
		return 0
	}
	if len(problems) > 0 {
		s := ""
		if len(problems) > 1 {
			s = "s"
		}
		r.printf("%d problem%s:\n", len(problems), s)
		for _, p := range problems {
			r.printf("- %s\n", p)
		}
		return 1
	}
	r.println("valid")
	return 0
}

// ------------------------------------------------------------------ org

var orgCmd = &command{name: "org", help: "Your orgs: members (its admin adds and removes them).", sub: []*command{
	{name: "members", args: "<org>", help: "The org's members.", nargs: exactly(1), run: runOrgMembers},
	{name: "add-member", args: "<org> <email>", help: "Add an existing account to the org (the org's admin).",
		flags: []flagSpec{{name: "title", help: "Their title in the org"}}, nargs: exactly(2), run: runOrgAdd},
	{name: "remove-member", args: "<org> <email>", help: "Remove someone from the org (the org's admin): every key they hold there stops working at once.",
		flags: []flagSpec{yesFlag}, nargs: exactly(2), run: runOrgRemove},
}}

func (r *inv) authAPI() (*api.Client, error) {
	c, err := r.core()
	if err != nil {
		return nil, err
	}
	return c.AuthAPI(core.DefaultTimeout)
}

func runOrgMembers(r *inv, a *parsed) int {
	cl, err := r.authAPI()
	if err != nil {
		return r.failErr(err)
	}
	var out protocol.Members
	if err := cl.Call(r.ctx, "org.members", protocol.OrgRef{Org: a.pos[0]}, &out); err != nil {
		return r.failErr(err)
	}
	for _, m := range out.Members {
		r.printf("%s\t%s\t%s\n", m.Name, m.Email, m.ID)
	}
	return 0
}

func runOrgAdd(r *inv, a *parsed) int {
	cl, err := r.authAPI()
	if err != nil {
		return r.failErr(err)
	}
	var m protocol.Member
	if err := cl.Call(r.ctx, "org.add_member", protocol.OrgAddMember{Org: a.pos[0], Email: a.pos[1], Title: a.get("title")}, &m); err != nil {
		return r.fail("not added: " + errText(err))
	}
	r.printf("added %s <%s> to %s\n", m.Name, m.Email, a.pos[0])
	return 0
}

func runOrgRemove(r *inv, a *parsed) int {
	org, email := a.pos[0], a.pos[1]
	cl, err := r.authAPI()
	if err != nil {
		return r.failErr(err)
	}
	var members protocol.Members
	if err := cl.Call(r.ctx, "org.members", protocol.OrgRef{Org: org}, &members); err != nil {
		return r.fail("not removed: " + errText(err))
	}
	var target *protocol.Member
	for i, m := range members.Members {
		if m.Email == strings.ToLower(email) || m.ID == email {
			target = &members.Members[i]
			break
		}
	}
	if target == nil {
		return r.fail(fmt.Sprintf("not removed: no member %s in %s", email, org))
	}
	if !a.has("yes") {
		if ok, why := r.confirm(fmt.Sprintf("Remove %s <%s> from %s? Their keys stop working.", target.Name, target.Email, org)); !ok {
			return r.fail(why)
		}
	}
	if err := cl.Call(r.ctx, "org.remove_member", protocol.OrgRemoveMember{Org: org, Member: target.ID}, nil); err != nil {
		return r.fail("not removed: " + errText(err))
	}
	r.printf("removed %s from %s\n", email, org)
	return 0
}

// ------------------------------------------------------------------ log

var logCmd = &command{name: "log", help: "Print a project's events as they arrive.",
	flags: []flagSpec{{name: "all", kind: flag, help: "Print from the beginning"}, projectFlag}, nargs: exactly(0), run: runLog}

// logRetry is how long the log waits before polling again when the server is unreachable.
var logRetry = 5 * time.Second

func runLog(r *inv, a *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	pid, _, ok := r.where(a.get("project"))
	if !ok {
		return r.fail(whichProject)
	}
	cl, err := c.API(r.ctx, pid, 40*time.Second)
	if err != nil {
		return r.failErr(err)
	}
	cursors := map[string]int64{}
	poll := func(wait float64) (protocol.SyncBatch, error) {
		p := protocol.NewSyncPoll()
		p.Cursors, p.Wait = cursors, wait
		var b protocol.SyncBatch
		err := cl.Call(r.ctx, "sync.poll", p, &b)
		return b, err
	}
	if !a.has("all") { // skip to now
		for {
			b, err := poll(0)
			if err != nil {
				return r.failErr(err)
			}
			cursors = b.Cursors
			if len(b.Events) == 0 {
				break
			}
		}
	}
	for {
		b, err := poll(25)
		if errors.Is(err, api.ErrOffline) {
			time.Sleep(logRetry)
			continue
		}
		if err != nil {
			return r.failErr(err)
		}
		for _, ev := range b.Events {
			r.println(eventLine(ev))
		}
		cursors = b.Cursors
	}
}

// eventLine is one event as the log prints it: time, project, seq, type and what it is about.
func eventLine(ev protocol.SyncEvent) string {
	raw, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	at := fmt.Sprint(m["at"])
	if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
		at = t.Format("15:04:05")
	}
	seq, _ := m["seq"].(float64)
	return fmt.Sprintf("%s %v #%d %s %s", at, m["project"], int64(seq), ev.EventType(), brief(m))
}

func brief(m map[string]any) string {
	switch sig := m["signal"].(type) {
	case map[string]any:
		if s, ok := sig["signal"].(map[string]any); ok {
			return fmt.Sprintf("%v %v %s", s["id"], s["kind"], quote(fmt.Sprint(s["title"])))
		}
	case string:
		return fmt.Sprintf("%s %v -> %v", sig, orEmpty(m["old"]), orEmpty(m["new"]))
	}
	for _, name := range []string{"drift", "topic", "state", "updated"} {
		v, ok := m[name]
		if !ok || v == nil {
			continue
		}
		if obj, ok := v.(map[string]any); ok {
			if id, ok := obj["id"]; ok {
				return fmt.Sprintf("%s %v", name, id)
			}
			if ver, ok := obj["version"].(float64); ok {
				return fmt.Sprintf("%s %d", name, int64(ver))
			}
		}
		return fmt.Sprintf("%s %v", name, v)
	}
	return ""
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// ------------------------------------------------------------------ hook

// runHook is called by an agent's hooks. It never fails the agent: until the adapters arrive it answers with no
// added context.
func runHook(r *inv, _ *parsed) int {
	if !isTerminal() { // the agent pipes the hook's JSON; a person at a terminal is never waited on
		_, _ = io.Copy(io.Discard, r.in)
	}
	return 0
}
