package core

import (
	"context"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/dashboard"
	"github.com/vutran1710/teamflow-cli/internal/deliverables"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// Dashboard is the project at a glance (`teamflow project status`, /teamf status), drawn from the replica after a
// sync within a hook's budget; offline (or signed out), from the replica as of its last sync. sess is the agent session this runs
// under (nil outside one): its member is "you" and its topics are named when it works on this project; otherwise
// "you" is the member this machine's key for the project acts as. The release the background sync last updated
// teamflow to is taken: it is told once.
func (c *Core) Dashboard(ctx context.Context, pid string, sess *store.Session, now time.Time) (dashboard.Board, error) {
	ctx, cancel := context.WithTimeout(ctx, hookDeadline)
	defer cancel()
	online := c.SignedIn() && c.Sync(ctx, HookTimeout, pid) // signed out: the replica is all there is
	if err := c.Replica(ctx, pid); err != nil {
		return dashboard.Board{}, err
	}
	project, _, err := c.St.Project(pid)
	if err != nil {
		return dashboard.Board{}, err
	}
	on := sess != nil && sess.Project == pid
	me := ""
	if on {
		me = sess.Member
	}
	if b, err := c.binding(); me == "" && err == nil {
		_, me, _, _, _ = c.St.ProjectKey(b, pid)
	}
	names, err := c.St.Names()
	if err != nil {
		return dashboard.Board{}, err
	}
	in := dashboard.Input{ProjectID: pid, Project: project.Name, Org: c.orgName(ctx, project.Org, online), You: me,
		Names: names, Offline: !online, SyncedAt: c.St.SyncedAt(pid)}
	if st, ok, err := c.St.LatestState(pid); err != nil {
		return dashboard.Board{}, err
	} else if ok {
		in.Plan = st.Version
	}
	if err := c.dashboardDeliverables(pid, &in); err != nil {
		return dashboard.Board{}, err
	}
	asking := store.Session{Project: pid, Member: me} // outside a session on the project: every topic
	if on {
		asking = *sess
	}
	waiting, err := c.approvalsOf(asking, false)
	if err != nil {
		return dashboard.Board{}, err
	}
	in.Approvals = len(waiting)
	drifts, err := c.St.Drifts(pid, protocol.DriftStatusOpen)
	if err != nil {
		return dashboard.Board{}, err
	}
	in.Drifts = len(drifts)
	if on {
		topics, err := c.St.Topics(pid)
		if err != nil {
			return dashboard.Board{}, err
		}
		in.Session = topicScope(topics, func(id string) bool { return sess.Follows(id, nil) }, sess.Topics == nil)
	}
	in.AutoUpdate, _ = config.AutoUpdate(c.Cfg)
	in.UpdatedTo, _ = c.St.TakeUpdatedTo()
	return dashboard.Build(in, now), nil
}

// dashboardDeliverables puts the project's Deliverables in in: the version in force with each action's done state
// and status from the results the replica holds, or the archived version as it is.
func (c *Core) dashboardDeliverables(pid string, in *dashboard.Input) error {
	held, ok, err := c.St.Deliverables(pid)
	if err != nil || !ok {
		return err
	}
	if held.Archived {
		in.Deliverables = &held
		return nil
	}
	d, results, err := c.liveDeliverables(pid)
	if err != nil || d == nil {
		return err
	}
	in.Deliverables, in.Statuses = d, map[string]protocol.ActionStatus{}
	for _, m := range d.Milestones {
		for _, a := range m.Actions {
			in.Statuses[a.ID] = deliverables.ActionStatus(a, results[a.ID])
		}
	}
	return nil
}

// orgName is the org's name as last seen; asked of the server (account.projects) when it is not known yet and the
// server answers ("" when it stays unknown).
func (c *Core) orgName(ctx context.Context, org string, online bool) string {
	if n := c.St.OrgName(org); n != "" || !online || !c.SignedIn() || org == "" {
		return n
	}
	if _, err := c.Projects(ctx); err != nil {
		return ""
	}
	return c.St.OrgName(org)
}
