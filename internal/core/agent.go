package core

// The agent's side of the client: what the agent's teamflow commands do with a session. A port
// of the Ingestor's emit, cite, inbox, after_turn, Deliverables and action paths (client/tf_client/ingestor.py).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/deliverables"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// ErrSessionNotShared is an agent action that needs the session's project before the person chose one.
var ErrSessionNotShared = errors.New("this session is not shared: the person chooses its project first")

// BadInput is a refusal of what was asked (a bad argument, an unknown name, out of the session's topics): the
// tool's caller reads its words and can fix the call. Python raised ValueError for these.
type BadInput struct{ Msg string }

func (e *BadInput) Error() string { return e.Msg }

func bad(format string, a ...any) error { return &BadInput{fmt.Sprintf(format, a...)} }

// InvalidError is a Deliverables submission that failed the shared check on this machine: nothing was sent.
type InvalidError struct{ Problems []string }

func (e *InvalidError) Error() string {
	return "the Deliverables are not valid: " + strings.Join(e.Problems, "; ")
}

// Shared is the session sid, which must be shared with a project.
func (c *Core) Shared(sid string) (store.Session, error) {
	sess, ok, err := c.St.Session(sid)
	if err != nil {
		return sess, err
	}
	if !ok || sess.Project == "" {
		return sess, ErrSessionNotShared
	}
	return sess, nil
}

// followed are the names of the (non-system) topics the session follows.
func (c *Core) followed(sess store.Session) string {
	topics, _ := c.St.Topics(sess.Project)
	var names []string
	for _, t := range topics {
		if !t.System && sess.Follows(t.ID, nil) {
			names = append(names, t.Name)
		}
	}
	return strings.Join(names, ", ")
}

// InScope refuses a topic outside the topics the session follows (when it is on that project and follows only
// some): what it is told and what it looks up stay in them.
func (c *Core) InScope(sess store.Session, project, topic string) error {
	if sess.Project != project || sess.Topics == nil || sess.Follows(topic, c.SystemTopics(project)) {
		return nil
	}
	return bad("this session follows only %s; the person adds topics to the session", c.followed(sess))
}

// ResolveMember is a member by id or by name (ignoring case). Names are not unique: when two people share it, it
// says so with their aliases and ids, rather than pick one.
func (c *Core) ResolveMember(who string) (string, error) {
	people, err := c.St.People()
	if err != nil {
		return "", err
	}
	if _, ok := people[who]; ok {
		return who, nil
	}
	var hits []string
	var names []string
	for id, p := range people {
		names = append(names, p.Name)
		if strings.EqualFold(p.Name, who) {
			hits = append(hits, id)
		}
	}
	slices.Sort(hits)
	slices.Sort(names)
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", bad("no member %s; members: %s", pyRepr(who), strings.Join(names, ", "))
	}
	var each []string
	for _, id := range hits {
		p := people[id]
		alias := ""
		if p.Alias != "" {
			alias = " (" + p.Alias + ")"
		}
		each = append(each, fmt.Sprintf("%s%s = %s", p.Name, alias, id))
	}
	return "", bad("several members are called %s: %s; use the id", pyRepr(who), strings.Join(each, ", "))
}

// FindMember is one of everyone by id, name (ignoring case) or email (ignoring case).
func FindMember(everyone []protocol.Member, who string) (protocol.Member, error) {
	for _, m := range everyone {
		if who == m.ID || who == m.Name || strings.EqualFold(who, m.Name) || (m.Email != "" && strings.EqualFold(who, m.Email)) {
			return m, nil
		}
	}
	var names []string
	for _, m := range everyone {
		names = append(names, m.Name)
	}
	return protocol.Member{}, bad("no member %s; members: %s", pyRepr(who), strings.Join(names, ", "))
}

// Everyone are the members of the project's org (with the project's key), or of an org (with the auth key).
func (c *Core) Everyone(ctx context.Context, project, org string) ([]protocol.Member, error) {
	var out protocol.Members
	a, err := c.AuthAPI(DefaultTimeout)
	if project != "" {
		a, err = c.API(ctx, project, DefaultTimeout)
	}
	if err != nil {
		return nil, err
	}
	if project != "" {
		return out.Members, a.Call(ctx, "member.list", nil, &out)
	}
	return out.Members, a.Call(ctx, "org.members", protocol.OrgRef{Org: org}, &out)
}

// ------------------------------------------------------------------ emitting

// ResultObj is a signal's answer as the tools give it: the server's result without its empty fields.
func ResultObj(r protocol.SignalResult) ojson.Obj {
	o := ojson.Obj{{"signal", r.Signal}, {"accepted", r.Accepted}}
	if r.Reason != nil {
		o = append(o, ojson.KV{"reason", string(*r.Reason)})
	}
	o = append(o, ojson.KV{"message", r.Message})
	if r.Seq != nil {
		o = append(o, ojson.KV{"seq", *r.Seq})
	}
	if r.ReceivedAt != nil {
		o = append(o, ojson.KV{"received_at", *r.ReceivedAt})
	}
	if r.State != nil {
		o = append(o, ojson.KV{"state", string(*r.State)})
	}
	return o
}

func invalid(msg string) ojson.Obj {
	if r := []rune(msg); len(r) > 400 {
		msg = string(r[:400])
	}
	return ojson.Obj{{"accepted", false}, {"reason", "INVALID"}, {"message", msg}}
}

// queuedText is the answer for a signal kept in the spool while the server is unreachable.
const queuedText = "server unreachable; queued and sent when it is back"

// Emit spools the session's drafts, then sends them (one batch). One answer per draft, in order: the server's
// result, an INVALID refusal from this machine (nothing sent), or queued (the server unreachable: the spool sends
// it later, once). A verdict is never emitted here: it has its own path and travels alone.
func (c *Core) Emit(ctx context.Context, sid string, drafts []map[string]any) ([]ojson.Obj, error) {
	sess, err := c.Shared(sid)
	if err != nil {
		return nil, err
	}
	pid := sess.Project
	c.Sync(ctx, HookTimeout, pid) // the current project version before drafting; offline: the replica
	if err := c.Replica(ctx, pid); err != nil {
		return nil, err
	}
	answers := make([]ojson.Obj, len(drafts))
	var ready []protocol.SignalEmit
	var at []int
	for i, d := range drafts {
		if k, _ := d["kind"].(string); slices.Contains(protocol.HumanActsVerdictSignalKinds, k) {
			answers[i] = invalid("a verdict is recorded on its own (`teamflow approve` or `teamflow reject`), only on the person's explicit words")
			continue
		}
		e, err := c.DraftFor(ctx, pid, sid, d, protocol.ActorAgent)
		if err != nil {
			answers[i] = invalid(err.Error())
			continue
		}
		ready, at = append(ready, e), append(at, i)
	}
	if len(ready) == 0 {
		return answers, nil
	}
	key, err := c.ProjectKey(ctx, pid)
	if err != nil {
		return nil, err
	}
	ref := api.KeyRef(key)
	now := c.Now()
	err = c.St.Tx(func(tx *store.Tx) error {
		for _, e := range ready {
			raw, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := tx.SpoolAdd(e.Signal.ID, sid, ref, string(raw), now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	results, err := c.send(ctx, pid, ready)
	if err != nil {
		return nil, err
	}
	for j, e := range ready {
		if r, ok := results[e.Signal.ID]; ok {
			answers[at[j]] = r
			continue
		}
		answers[at[j]] = ojson.Obj{{"signal", e.Signal.ID}, {"queued", true}, {"message", queuedText}}
	}
	return answers, nil
}

// send sends spooled signals in one batch and takes each answered one out of the spool. Offline, the rest stay
// spooled (no answer: queued). A refusal of one signal is its answer. A refusal that is not about the signal (an
// old client, a key no longer good) is the error, and the signals stay spooled for the next sync.
func (c *Core) send(ctx context.Context, pid string, items []protocol.SignalEmit) (map[string]ojson.Obj, error) {
	a, err := c.API(ctx, pid, HookTimeout)
	if err != nil {
		return nil, err
	}
	calls := make([]api.Call, len(items))
	for i, e := range items {
		calls[i] = api.Call{Method: "signal.emit", Params: e}
	}
	results, berr := a.Batch(ctx, calls)
	out := map[string]ojson.Obj{}
	var keep error // why a signal stays spooled, when it is not that the server is unreachable
	err = c.St.Tx(func(tx *store.Tx) error {
		for i, res := range results {
			id := items[i].Signal.ID
			var r protocol.SignalResult
			var ae *api.APIError
			switch {
			case res.Err == nil:
				if err := json.Unmarshal(res.Result, &r); err != nil {
					continue // an answer this client can't read: the spool sends it again
				}
				out[id] = ResultObj(r)
			case errors.As(res.Err, &ae) && ae.Reason != "" && !errors.Is(res.Err, api.ErrUpgradeRequired) &&
				ae.Reason != string(protocol.RejectReasonNotAMember):
				out[id] = ojson.Obj{{"signal", id}, {"accepted", false}, {"reason", ae.Reason}, {"message", ae.Message}}
			default:
				if keep == nil && !errors.Is(res.Err, api.ErrOffline) {
					keep = res.Err
				}
				continue
			}
			if err := tx.SpoolDone(id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if berr != nil && !errors.Is(berr, api.ErrOffline) {
		return nil, berr
	}
	if keep != nil { // an old client, a key no longer good: kept in the spool, and the agent is told why
		return nil, keep
	}
	if len(out) > 0 {
		_, _ = syncer.Pull(ctx, c.St, a, HookTimeout)
	}
	return out, nil
}

// Cite records that the session relies on these signals (the ones the replica knows); it returns them.
func (c *Core) Cite(sid string, signals []string) ([]string, error) {
	known := []string{}
	for _, s := range signals {
		if _, ok, err := c.St.Signal(s); err != nil {
			return nil, err
		} else if ok && !slices.Contains(known, s) {
			known = append(known, s)
		}
	}
	if err := c.St.Cite(sid, known, c.Now()); err != nil {
		return nil, err
	}
	return known, nil
}

// Inbox is every notice of the session, shown or not, oldest first.
func (c *Core) Inbox(sid string) ([]string, error) {
	ns, err := c.St.Notices(sid, false)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range ns {
		out = append(out, n.Text)
	}
	return out, nil
}

// AfterTurn is the approval requests that reached the session since it last heard of them (its project only),
// synced first, as the approval queue's text; each is raised once. "" when there are none.
func (c *Core) AfterTurn(ctx context.Context, sid string) string {
	sess, ok, err := c.St.Session(sid)
	if err != nil || !ok || sess.Project == "" || !c.SignedIn() {
		return ""
	}
	c.Sync(ctx, HookTimeout, sess.Project)
	return c.raiseNew(sid)
}

// raiseNew claims the session's new approval requests and lists the ones still waiting. The claim and the read
// are one transaction: when the read fails, the claim is rolled back and the next turn raises them (claimed atomically,
// so a hook and a command racing never both raise one).
func (c *Core) raiseNew(sid string) string {
	var out string
	err := c.St.Tx(func(tx *store.Tx) error {
		var err error
		out, err = c.claimApprovals(tx, sid)
		return err
	})
	if err != nil {
		c.logHook("raising approval requests", err)
		return ""
	}
	return out
}

// claimApprovals claims the session's new approval requests in tx, as the approval queue's text of those still
// waiting ("" when none).
func (c *Core) claimApprovals(tx *store.Tx, sid string) (string, error) {
	ns, err := tx.ClaimNotices(sid, true)
	if err != nil || len(ns) == 0 {
		return "", err
	}
	asked := map[string]bool{}
	for _, n := range ns {
		asked[n.About] = true
	}
	waiting, err := c.Approvals(sid, false)
	if err != nil {
		return "", err
	}
	var items []protocol.StoredSignal
	for _, st := range waiting {
		if asked[st.Signal.ID] {
			items = append(items, st)
		}
	}
	if len(items) == 0 {
		return "", nil
	}
	return c.ApprovalsText(items, "a new approval request arrived for the person you work with."), nil
}

// NewNotices are the session's other notices not shown yet (stale citations, drifts, rejections after a queue,
// topic deletions, actions…), marked shown: an agent with no hooks hears of them at its next command, once.
func (c *Core) NewNotices(sid string) []string {
	ns, err := c.St.ClaimNotices(sid, false) // taken once, even by commands racing
	if err != nil || len(ns) == 0 {
		return nil
	}
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Text
	}
	return out
}

// News is what every teamflow command's output ends with: the approval requests that reached the session (its
// project only, synced first as a hook inside a turn syncs: not when the background sync is live, at most once per
// HookSyncEvery, briefly) and its other notices not shown yet. Both are claimed in one transaction: two commands
// running at once never both carry one, and a failed read rolls the claims back for the next command.
func (c *Core) News(ctx context.Context, sid string) (approvals string, notices []string) {
	sess, ok, err := c.St.Session(sid)
	if err != nil || !ok {
		return "", nil
	}
	asking := sess.Project != "" && c.SignedIn()
	if asking {
		c.midTurnSync(ctx, sess.Project)
	}
	err = c.St.Tx(func(tx *store.Tx) error {
		approvals, notices = "", nil
		if asking {
			text, err := c.claimApprovals(tx, sid)
			if err != nil {
				return err
			}
			approvals = text
		}
		ns, err := tx.ClaimNotices(sid, false)
		if err != nil {
			return err
		}
		for _, n := range ns {
			notices = append(notices, n.Text)
		}
		return nil
	})
	if err != nil {
		c.logHook("claiming the news for a command", err)
		return "", nil
	}
	return approvals, notices
}

// ------------------------------------------------------------------ human acts on drifts

// DriftAct resolves or dismisses a drift of the project (initiator only), with a note: drift.resolve or
// drift.dismiss, sent alone (HUMAN_ACTS), never in a batch.
func (c *Core) DriftAct(ctx context.Context, project, method, drift, note string, resolvedBy []string) (protocol.Drifts, error) {
	var out protocol.Drifts
	if !slices.Contains(protocol.HumanActsMethods, method) {
		return out, fmt.Errorf("%s is not a drift act", method)
	}
	if strings.TrimSpace(note) == "" {
		return out, bad("say how it was settled: a note is needed")
	}
	a, err := c.API(ctx, project, DefaultTimeout)
	if err != nil {
		return out, err
	}
	var params any = protocol.DriftDismiss{Drift: drift, Note: note}
	if method == "drift.resolve" {
		if resolvedBy == nil {
			resolvedBy = []string{}
		}
		params = protocol.DriftResolve{Drift: drift, Note: note, ResolvedBy: resolvedBy}
	}
	if err := a.Call(ctx, method, params, &out); err != nil { // alone: Call, never Batch
		return out, err
	}
	c.Sync(ctx, HookTimeout, project)
	return out, nil
}

// DriftProject is the project of a drift the replica knows (any status), or "".
func (c *Core) DriftProject(drift string) string {
	projects := map[string]bool{}
	if b, err := c.binding(); err == nil {
		if ps, err := c.St.KeyProjects(b); err == nil {
			for _, p := range ps {
				projects[p] = true
			}
		}
	}
	for p := range c.St.Cursors() {
		projects[p] = true
	}
	for p := range projects {
		for _, s := range []protocol.DriftStatus{protocol.DriftStatusOpen, protocol.DriftStatusResolved, protocol.DriftStatusDismissed} {
			ds, _ := c.St.Drifts(p, s)
			for _, d := range ds {
				if d.ID == drift {
					return p
				}
			}
		}
	}
	return ""
}

// ------------------------------------------------------------------ Deliverables

// currentDeliverables is the current version (synced first), if a submission may be based on it: not archived,
// there at all when need, and the version the agent read (base). Once there are Deliverables, every submission
// says which version it read: one built on an older read would silently drop a teammate's newer change.
func (c *Core) currentDeliverables(ctx context.Context, sess store.Session, base *int64, need bool) (*protocol.Deliverables, error) {
	c.Sync(ctx, HookTimeout, sess.Project)
	if err := c.Replica(ctx, sess.Project); err != nil {
		return nil, err
	}
	d, ok, err := c.St.Deliverables(sess.Project)
	if err != nil {
		return nil, err
	}
	if ok && d.Archived {
		return nil, bad("the Deliverables are archived: nothing changes until the initiator restores them")
	}
	if !ok && need {
		return nil, bad("this project has no Deliverables yet: publish them first with `teamflow deliverables publish`")
	}
	switch {
	case base == nil && ok:
		return nil, bad("the Deliverables are at version %d: your change must say which version it is based on: "+
			"pass the version you read (`teamflow deliverables`), then resubmit", d.Version)
	case base != nil && (!ok || *base != d.Version):
		now := "not published yet"
		if ok {
			now = fmt.Sprintf("at version %d", d.Version)
		}
		return nil, bad("the Deliverables are %s, not version %d: get them again (`teamflow deliverables`) and resubmit", now, *base)
	}
	if !ok {
		return nil, nil
	}
	return &d, nil
}

// changed says whether an action differs in what makes it changed (name, topic, due, owner, after).
func changed(a, b protocol.Action) bool {
	return a.Name != b.Name || a.Topic != b.Topic || a.Due != b.Due || !sameStr(a.Owner, b.Owner) || !slices.Equal(a.After, b.After)
}

func sameStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// deliverablesInScope: a session that does not follow every topic submits only when every action it adds,
// changes or removes is in its topics; a whole publish needs them all.
func (c *Core) deliverablesInScope(sess store.Session, milestones []protocol.Milestone, old *protocol.Deliverables, whole bool) error {
	topics, err := c.St.Topics(sess.Project)
	if err != nil {
		return err
	}
	system := c.SystemTopics(sess.Project)
	all := true
	for _, t := range topics {
		if !sess.Follows(t.ID, system) {
			all = false
		}
	}
	if all {
		return nil
	}
	followed := c.followed(sess)
	if whole {
		return bad("this session follows only %s: publishing the whole Deliverables needs a session on every topic "+
			"(the person widens it); to change one milestone, use `teamflow deliverables update-milestone`", followed)
	}
	was := map[string]protocol.Action{}
	if old != nil {
		for _, m := range old.Milestones {
			for _, a := range m.Actions {
				was[a.ID] = a
			}
		}
	}
	now := map[string]bool{}
	var order []string
	touched := map[string][]string{}
	touch := func(name string, topics ...string) {
		if _, ok := touched[name]; !ok {
			order = append(order, name)
		}
		touched[name] = append(touched[name], topics...)
	}
	for _, m := range milestones {
		for _, a := range m.Actions {
			now[a.ID] = true
			before, ok := was[a.ID]
			switch {
			case !ok:
				touch(a.Name, a.Topic)
			case changed(before, a):
				touch(a.Name, a.Topic, before.Topic)
			}
		}
	}
	if old != nil {
		for _, m := range old.Milestones {
			for _, a := range m.Actions {
				if !now[a.ID] {
					touch(a.Name, a.Topic)
				}
			}
		}
	}
	var outside []string
	for _, name := range order {
		for _, t := range touched[name] {
			if !sess.Follows(t, system) {
				outside = append(outside, pyRepr(name))
				break
			}
		}
	}
	if len(outside) == 0 {
		return nil
	}
	verb := "are"
	if len(outside) == 1 {
		verb = "is"
	}
	return bad("this session follows only %s: %s %s in other topics; the person adds topics to the session",
		followed, strings.Join(outside, ", "), verb)
}

// submitDeliverables: the topic scope, then the shared check on the merged structure. Refused here, never sent,
// when it fails.
func (c *Core) submitDeliverables(sess store.Session, spec []protocol.MilestoneSpec, old *protocol.Deliverables, whole bool) error {
	dc, err := c.deliverablesContext(sess.Project)
	if err != nil {
		return err
	}
	dc.Old = old
	milestones, problems := deliverables.Problems(spec, dc)
	if err := c.deliverablesInScope(sess, milestones, old, whole); err != nil {
		return err
	}
	if len(problems) > 0 {
		return &InvalidError{problems}
	}
	return nil
}

func (c *Core) sentDeliverables(ctx context.Context, pid, method string, params any) (protocol.DeliverablesView, error) {
	var out protocol.DeliverablesView
	a, err := c.API(ctx, pid, DefaultTimeout)
	if err != nil {
		return out, err
	}
	if err := a.Call(ctx, method, params, &out); err != nil {
		return out, err
	}
	c.Sync(ctx, HookTimeout, pid)
	return out, nil
}

// DeliverablesPublish publishes the whole structure (JSON milestones, topics, owners and dependencies by name or
// id) as the next version. Checked here first: an invalid one is an *InvalidError and is never sent. Offline it
// fails: it is not spooled.
func (c *Core) DeliverablesPublish(ctx context.Context, sid string, raw json.RawMessage, base *int64) (protocol.DeliverablesView, error) {
	var out protocol.DeliverablesView
	sess, err := c.Shared(sid)
	if err != nil {
		return out, err
	}
	old, err := c.currentDeliverables(ctx, sess, base, false)
	if err != nil {
		return out, err
	}
	spec, problems := deliverables.Specs(raw)
	if len(problems) > 0 {
		return out, &InvalidError{problems}
	}
	if err := c.submitDeliverables(sess, spec, old, true); err != nil {
		return out, err
	}
	return c.sentDeliverables(ctx, sess.Project, "deliverables.publish",
		protocol.DeliverablesPublish{Project: sess.Project, BaseVersion: base, Milestones: spec})
}

// DeliverablesUpdateMilestone changes one milestone (by its id) with all its actions, merged into the current
// version as the server will.
func (c *Core) DeliverablesUpdateMilestone(ctx context.Context, sid string, raw json.RawMessage, base *int64) (protocol.DeliverablesView, error) {
	var out protocol.DeliverablesView
	sess, err := c.Shared(sid)
	if err != nil {
		return out, err
	}
	old, err := c.currentDeliverables(ctx, sess, base, true)
	if err != nil {
		return out, err
	}
	spec, problems := deliverables.Specs(append(append([]byte("["), raw...), ']'))
	var merged []protocol.MilestoneSpec
	if len(spec) == 1 {
		var more []string
		merged, more = deliverables.MergeMilestone(*old, spec[0])
		problems = append(problems, more...)
	}
	if len(problems) > 0 {
		return out, &InvalidError{problems}
	}
	if err := c.submitDeliverables(sess, merged, old, false); err != nil {
		return out, err
	}
	return c.sentDeliverables(ctx, sess.Project, "deliverables.update_milestone",
		protocol.DeliverablesUpdateMilestone{Project: sess.Project, BaseVersion: old.Version, Milestone: spec[0]})
}

// DeliverablesRename gives milestones and actions new names, keyed by id (or by a current name that names
// exactly one item, ignoring case).
func (c *Core) DeliverablesRename(ctx context.Context, sid string, names []deliverables.Rename, base *int64) (protocol.DeliverablesView, error) {
	var out protocol.DeliverablesView
	sess, err := c.Shared(sid)
	if err != nil {
		return out, err
	}
	old, err := c.currentDeliverables(ctx, sess, base, true)
	if err != nil {
		return out, err
	}
	items := map[string]string{}
	for _, m := range old.Milestones {
		items[m.ID] = m.Name
		for _, a := range m.Actions {
			items[a.ID] = a.Name
		}
	}
	ident := func(k string) string {
		if _, ok := items[k]; ok {
			return k
		}
		var hits []string
		for id, name := range items {
			if strings.EqualFold(k, name) {
				hits = append(hits, id)
			}
		}
		if len(hits) == 1 {
			return hits[0]
		}
		return k
	}
	byID := make([]deliverables.Rename, len(names))
	wire := map[string]string{}
	for i, r := range names {
		byID[i] = deliverables.Rename{ID: ident(r.ID), Name: r.Name}
		wire[byID[i].ID] = r.Name
	}
	merged, problems := deliverables.ApplyRenames(*old, byID)
	if len(problems) > 0 {
		return out, &InvalidError{problems}
	}
	if err := c.submitDeliverables(sess, merged, old, false); err != nil {
		return out, err
	}
	return c.sentDeliverables(ctx, sess.Project, "deliverables.rename",
		protocol.DeliverablesRename{Project: sess.Project, BaseVersion: old.Version, Names: wire})
}

// findAction is an action of the Deliverables by id; else by its exact name; else the one name that matches
// ignoring case (two such are refused, never the first taken).
func findAction(d *protocol.Deliverables, ref string) (*protocol.Action, error) {
	if d == nil {
		return nil, nil
	}
	var all []protocol.Action
	for _, m := range d.Milestones {
		all = append(all, m.Actions...)
	}
	for i, a := range all {
		if ref == a.ID || ref == a.Name {
			return &all[i], nil
		}
	}
	var hits []protocol.Action
	for _, a := range all {
		if strings.EqualFold(ref, a.Name) {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return &hits[0], nil
	}
	var each []string
	for _, a := range hits {
		each = append(each, a.ID+" "+pyRepr(a.Name))
	}
	return nil, bad("several actions are called %s (ignoring case): use the id (%s)", pyRepr(ref), strings.Join(each, ", "))
}

// action is an action of the current Deliverables from the replica (synced once more when it is not there yet).
func (c *Core) action(ctx context.Context, pid, ref string) (protocol.Action, error) {
	held := func() (*protocol.Deliverables, error) {
		d, ok, err := c.St.Deliverables(pid)
		if err != nil || !ok {
			return nil, err
		}
		return &d, nil
	}
	d, err := held()
	if err != nil {
		return protocol.Action{}, err
	}
	hit, ferr := findAction(d, ref)
	if hit == nil && ferr == nil {
		c.Sync(ctx, HookTimeout, pid)
		if d, err = held(); err != nil {
			return protocol.Action{}, err
		}
		hit, ferr = findAction(d, ref)
	}
	switch {
	case ferr != nil:
		return protocol.Action{}, ferr
	case d == nil:
		return protocol.Action{}, bad("this project has no Deliverables yet: there is no action to report on")
	case d.Archived:
		return protocol.Action{}, bad("the Deliverables are archived: no results or revokes until the initiator restores them")
	case hit == nil:
		var each []string
		for _, m := range d.Milestones {
			for _, a := range m.Actions {
				each = append(each, a.ID+" "+pyRepr(a.Name))
			}
		}
		return protocol.Action{}, bad("no action %s in the current Deliverables (version %d); its actions: %s",
			pyRepr(ref), d.Version, strings.Join(each, ", "))
	}
	return *hit, nil
}

var outcomes = map[string]string{"DONE": "Done", "PARTIAL": "Partial", "BLOCKED": "Blocked"}

// ActionResult reports what came of an action: DONE (only its owner's, or anyone's when it has none), PARTIAL or
// BLOCKED (with the blocker). An ordinary signal in the action's topic: spooled first, sent when the server is there.
func (c *Core) ActionResult(ctx context.Context, sid, action, outcome, summary string, points []string, blocker string) (ojson.Obj, error) {
	sess, err := c.Shared(sid)
	if err != nil {
		return nil, err
	}
	kind := strings.ToUpper(outcome)
	word, ok := outcomes[kind]
	if !ok {
		return nil, bad("outcome %s: DONE, PARTIAL or BLOCKED", pyRepr(outcome))
	}
	a, err := c.action(ctx, sess.Project, action)
	if err != nil {
		return nil, err
	}
	if points == nil {
		points = []string{}
	}
	body := map[string]any{"type": "action_result.v1", "action": a.ID, "outcome": kind, "summary": summary,
		"points": points, "blocker": blocker}
	out, err := c.Emit(ctx, sid, []map[string]any{{"kind": string(protocol.SignalKindActionResult), "topic": a.Topic,
		"title": truncate(word+": "+a.Name, 120), "body": body}})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// ActionRevoke withdraws an action's done, saying why (anyone may; its owner is told).
func (c *Core) ActionRevoke(ctx context.Context, sid, action, reason string) (ojson.Obj, error) {
	sess, err := c.Shared(sid)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, bad("a revoke needs a reason: say why the action is not done (at most 240 characters)")
	}
	a, err := c.action(ctx, sess.Project, action)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"type": "action_revoke.v1", "action": a.ID, "reason": strings.TrimSpace(reason)}
	out, err := c.Emit(ctx, sid, []map[string]any{{"kind": string(protocol.SignalKindActionRevoke), "topic": a.Topic,
		"title": truncate("Revoked: "+a.Name, 120), "body": body}})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}
