package cli

// Commands for every action the web app has, so none is web-only: drift resolve|dismiss, deliverables
// archive|delete|history, project title|remove-member|plan, org title, profile (and its avatar), tasks; and
// `teamflow mcp`, the MCP server agents start. The server decides who may do what (initiator, admin, approver).

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/artifacts"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/mcpserver"
	"github.com/vutran1710/teamflow-cli/internal/procs"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// ------------------------------------------------------------------ mcp

// ancestry finds the agent session a process runs under by its ancestors (the session recorded against the
// nearest of them), never a dead agent's session whose pid was reused.
type ancestry struct{}

func (ancestry) Current(st *store.Store) (store.Session, bool, error) {
	return st.SessionOfPIDs(procs.Ancestors(os.Getpid(), 12), func(s store.Session) bool {
		started, ok := procs.StartTime(s.AgentPID)
		return ok && !started.After(s.UpdatedAt.Add(pidReuseSlack))
	})
}

// runMCP serves the MCP tools over stdin and stdout, for the agent that started it.
func runMCP(r *inv, _ *parsed) int {
	if err := mcpserver.Run(r.ctx, core.Open, ancestry{}, api.Version, r.in, r.out); err != nil {
		fmt.Fprintln(r.errw, "teamflow mcp:", err)
		return 1
	}
	return 0
}

// ------------------------------------------------------------------ drift

var driftCmd = &command{name: "drift", help: "Settle a drift the runner found (the project's initiator).", sub: []*command{
	{name: "resolve", args: "<dr_id> <note>", help: "Mark a drift settled by its discussion, with a note.",
		flags: []flagSpec{projectFlag, yesFlag, {name: "resolved-by", kind: list, help: "An sg_ id that settled it (repeatable)"}},
		nargs: atLeast(2), run: func(r *inv, a *parsed) int { return r.driftAct("drift.resolve", a) }},
	{name: "dismiss", args: "<dr_id> <note>", help: "Dismiss a drift as no real conflict, with a note.",
		flags: []flagSpec{projectFlag, yesFlag}, nargs: atLeast(2),
		run: func(r *inv, a *parsed) int { return r.driftAct("drift.dismiss", a) }},
}}

func (r *inv) driftAct(method string, a *parsed) int {
	drift, note := a.pos[0], strings.Join(a.pos[1:], " ")
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	pid := a.get("project")
	if pid == "" {
		pid = c.DriftProject(drift)
	}
	if pid == "" {
		var ok bool
		if pid, _, ok = r.where(""); !ok {
			return r.fail(whichProject)
		}
	}
	word, done := "Resolve", "resolved"
	if method == "drift.dismiss" {
		word, done = "Dismiss", "dismissed"
	}
	if !a.has("yes") {
		if ok, why := r.confirm(fmt.Sprintf("%s drift %s with the note %s?", word, drift, quote(note))); !ok {
			return r.fail(why)
		}
	}
	if _, err := c.DriftAct(r.ctx, pid, method, drift, note, a.lists["resolved-by"]); err != nil {
		return r.failErr(err)
	}
	r.printf("%s %s\n", done, drift)
	return 0
}

// ------------------------------------------------------------------ deliverables archive | delete | history

var deliverablesActions = []*command{
	{name: "archive", help: "Archive the Deliverables (frozen; the initiator), or restore them with --restore.",
		flags: []flagSpec{projectFlag, {name: "note", help: "Why, at most 240 characters"}, {name: "restore", kind: flag,
			help: "Bring them back"}, yesFlag}, nargs: exactly(0), run: runDeliverablesArchive},
	{name: "delete", help: "Delete the Deliverables and their history (the initiator); reported results stay as signals.",
		flags: []flagSpec{projectFlag, yesFlag}, nargs: exactly(0), run: runDeliverablesDelete},
	{name: "history", help: "Every kept version of the Deliverables, newest first.",
		flags: []flagSpec{projectFlag}, nargs: exactly(0), run: runDeliverablesHistory},
}

func init() { deliverablesCmd.sub = append(deliverablesCmd.sub, deliverablesActions...) }

// projectAPI is the project to act on (--project, or this session's) and the server with its key.
func (r *inv) projectAPI(a *parsed) (*core.Core, string, *api.Client, bool) {
	c, err := r.core()
	if err != nil {
		r.failErr(err)
		return nil, "", nil, false
	}
	pid, _, ok := r.where(a.get("project"))
	if !ok {
		r.fail(whichProject)
		return nil, "", nil, false
	}
	cl, err := c.API(r.ctx, pid, core.DefaultTimeout)
	if err != nil {
		r.failErr(err)
		return nil, "", nil, false
	}
	return c, pid, cl, true
}

func runDeliverablesArchive(r *inv, a *parsed) int {
	restore := a.has("restore")
	if !restore && !a.has("yes") {
		if ok, why := r.confirm("Archive the Deliverables? Nothing changes in them until they are restored."); !ok {
			return r.fail(why)
		}
	}
	c, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	var view protocol.DeliverablesView
	if err := cl.Call(r.ctx, "deliverables.archive", protocol.DeliverablesArchive{Project: pid, Archived: !restore,
		Note: a.get("note")}, &view); err != nil {
		return r.failErr(err)
	}
	c.Sync(r.ctx, core.HookTimeout, pid)
	if restore {
		r.println("restored the Deliverables")
	} else {
		r.println("archived the Deliverables")
	}
	return 0
}

func runDeliverablesDelete(r *inv, a *parsed) int {
	if !a.has("yes") {
		if ok, why := r.confirm("Delete the Deliverables and their history for everyone? Cannot be undone."); !ok {
			return r.fail(why)
		}
	}
	c, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	if err := cl.Call(r.ctx, "deliverables.delete", protocol.DeliverablesDelete{Project: pid}, nil); err != nil {
		return r.failErr(err)
	}
	c.Sync(r.ctx, core.HookTimeout, pid)
	r.println("deleted the Deliverables; reported results stay as signals")
	return 0
}

func runDeliverablesHistory(r *inv, a *parsed) int {
	c, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	var vs protocol.DeliverablesVersions
	if err := cl.Call(r.ctx, "deliverables.history", protocol.DeliverablesHistory{Project: pid}, &vs); err != nil {
		return r.failErr(err)
	}
	names, _ := c.St.Names()
	for _, v := range vs.Versions {
		by := "-"
		if v.PublishedBy != nil {
			by = *v.PublishedBy
			if n, ok := names[by]; ok {
				by = n
			}
		}
		r.printf("v%d\t%s\t%s\n", v.Version, v.PublishedAt, by)
	}
	return 0
}

// ------------------------------------------------------------------ project title | remove-member | plan

var projectCmd = &command{name: "project", help: "A project's people and plan (the initiator).", sub: []*command{
	{name: "title", args: "<member> <title>", help: "Set a member's title in the project (empty: clear it).",
		flags: []flagSpec{projectFlag}, nargs: atLeast(1), run: runProjectTitle},
	{name: "remove-member", args: "<member>", help: "Remove a member from the project: their project keys stop working.",
		flags: []flagSpec{projectFlag, yesFlag}, nargs: exactly(1), run: runProjectRemove},
	{name: "plan", help: "Show the plan and instruction; with --plan/--instruction (or their files), write them by hand.",
		flags: []flagSpec{projectFlag, {name: "plan", help: "The whole new plan"}, {name: "plan-file", help: "A file holding it"},
			{name: "instruction", help: "The whole new instruction"}, {name: "instruction-file", help: "A file holding it"}, yesFlag},
		nargs: exactly(0), run: runProjectPlan},
}}

func runProjectTitle(r *inv, a *parsed) int {
	c, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	everyone, err := c.Everyone(r.ctx, pid, "")
	if err != nil {
		return r.failErr(err)
	}
	m, err := core.FindMember(everyone, a.pos[0])
	if err != nil {
		return r.failErr(err)
	}
	title := strings.Join(a.pos[1:], " ")
	var view protocol.ProjectView
	if err := cl.Call(r.ctx, "project.set_title", protocol.ProjectSetTitle{Project: pid, Member: m.ID, Title: title}, &view); err != nil {
		return r.failErr(err)
	}
	r.printf("%s: %s\n", m.Name, view.Project.Titles[m.ID])
	return 0
}

func runProjectRemove(r *inv, a *parsed) int {
	c, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	everyone, err := c.Everyone(r.ctx, pid, "")
	if err != nil {
		return r.failErr(err)
	}
	m, err := core.FindMember(everyone, a.pos[0])
	if err != nil {
		return r.failErr(err)
	}
	var view protocol.ProjectView
	if err := cl.Call(r.ctx, "project.get", protocol.ProjectGet{Project: pid}, &view); err != nil {
		return r.failErr(err)
	}
	if !a.has("yes") {
		if ok, why := r.confirm(fmt.Sprintf("Remove %s from %s? Their project keys stop working.", m.Name, view.Project.Name)); !ok {
			return r.fail(why)
		}
	}
	if err := cl.Call(r.ctx, "project.remove_member", protocol.RemoveMember{Project: pid,
		ExpectedVersion: view.Project.Version, Member: m.ID}, nil); err != nil {
		return r.failErr(err)
	}
	c.Sync(r.ctx, core.HookTimeout, pid)
	r.printf("removed %s from %s\n", m.Name, view.Project.Name)
	return 0
}

// textOf is a flag's text, or its file's (never both).
func (r *inv) textOf(a *parsed, flag string) (string, bool, error) {
	file := flag + "-file"
	switch {
	case a.has(flag) && a.has(file):
		return "", false, usageError{"give --" + flag + " or --" + file + ", not both"}
	case a.has(file):
		b, err := os.ReadFile(a.get(file))
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return "", false, fmt.Errorf("cannot read %s: %v", home.Display(a.get(file)), err)
		}
		return string(b), true, nil
	case a.has(flag):
		return a.get(flag), true, nil
	}
	return "", false, nil
}

func runProjectPlan(r *inv, a *parsed) int {
	plan, newPlan, err := r.textOf(a, "plan")
	if err == nil {
		var newInstr bool
		var instr string
		if instr, newInstr, err = r.textOf(a, "instruction"); err == nil {
			return r.writePlan(a, plan, newPlan, instr, newInstr)
		}
	}
	var ue usageError
	if errors.As(err, &ue) {
		return usageFail(r, "project plan", ue.msg)
	}
	return r.failErr(err)
}

func (r *inv) writePlan(a *parsed, plan string, newPlan bool, instr string, newInstr bool) int {
	c, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	var view protocol.ProjectView
	if err := cl.Call(r.ctx, "project.get", protocol.ProjectGet{Project: pid}, &view); err != nil {
		return r.failErr(err)
	}
	if !newPlan && !newInstr {
		r.printf("%s, state v%d\nPlan:\n%s\nInstruction:\n%s\n", view.Project.Name, view.State.Version, view.State.Plan, view.State.Instruction)
		return 0
	}
	if !newPlan {
		plan = view.State.Plan
	}
	if !newInstr {
		instr = view.State.Instruction
	}
	if !a.has("yes") {
		if ok, why := r.confirm(fmt.Sprintf("Replace the plan and instruction of %s (state v%d) for everyone?", view.Project.Name, view.State.Version)); !ok {
			return r.fail(why)
		}
	}
	var out protocol.ProjectView
	if err := cl.Call(r.ctx, "project.write_state", protocol.WriteState{Project: pid, BaseVersion: view.State.Version,
		Plan: plan, Instruction: instr}, &out); err != nil {
		return r.failErr(err)
	}
	c.Sync(r.ctx, core.HookTimeout, pid)
	r.printf("wrote state v%d of %s\n", out.State.Version, out.Project.Name)
	return 0
}

// ------------------------------------------------------------------ org title

var orgTitleCmd = &command{name: "title", args: "<org> <member> <title>", help: "Set a member's title in the org (the org's admin; empty: clear it).",
	nargs: atLeast(2), run: runOrgTitle}

func init() { orgCmd.sub = append(orgCmd.sub, orgTitleCmd) }

func runOrgTitle(r *inv, a *parsed) int {
	org := a.pos[0]
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	everyone, err := c.Everyone(r.ctx, "", org)
	if err != nil {
		return r.failErr(err)
	}
	m, err := core.FindMember(everyone, a.pos[1])
	if err != nil {
		return r.failErr(err)
	}
	cl, err := c.AuthAPI(core.DefaultTimeout)
	if err != nil {
		return r.failErr(err)
	}
	var out protocol.Member
	if err := cl.Call(r.ctx, "org.set_title", protocol.OrgSetTitle{Org: org, Member: m.ID, Title: strings.Join(a.pos[2:], " ")}, &out); err != nil {
		return r.failErr(err)
	}
	r.printf("%s in %s: %s\n", out.Name, org, out.Title)
	return 0
}

// ------------------------------------------------------------------ profile

var profileCmd = &command{name: "profile", help: "Your name and alias (the same in every org); `profile avatar` sets your picture.",
	flags: []flagSpec{{name: "name", help: "A new name"}, {name: "alias", help: "A new alias (empty: clear it)"}},
	nargs: exactly(0), run: runProfile, sub: []*command{
		{name: "avatar", args: "[<file>]", help: "Set your picture (PNG, JPEG or WebP, at most 200 KB), or --remove it.",
			flags: []flagSpec{{name: "remove", kind: flag, help: "Remove your picture"}}, nargs: between(0, 1), run: runAvatar},
	}}

func runProfile(r *inv, a *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	cl, err := c.AuthAPI(core.DefaultTimeout)
	if err != nil {
		return r.failErr(err)
	}
	if !a.has("name") && !a.has("alias") {
		var me protocol.AccountView
		if err := cl.Call(r.ctx, "account.me", nil, &me); err != nil {
			return r.failErr(err)
		}
		picture := "none"
		if me.Account.Avatar != nil {
			picture = "set"
		}
		r.printf("name: %s\nalias: %s\nemail: %s\npicture: %s\n", me.Account.Name, me.Account.Alias, me.Account.Email, picture)
		return 0
	}
	var p protocol.ProfileUpdate
	if a.has("name") {
		name := a.get("name")
		if strings.TrimSpace(name) == "" {
			return r.fail("a name cannot be empty")
		}
		p.Name = &name
	}
	if a.has("alias") {
		alias := a.get("alias")
		p.Alias = &alias
	}
	var acc protocol.Account
	if err := cl.Call(r.ctx, "account.update_profile", p, &acc); err != nil {
		return r.failErr(err)
	}
	r.printf("name: %s\nalias: %s\n", acc.Name, acc.Alias)
	return 0
}

var avatarTypes = []string{"image/png", "image/jpeg", "image/webp"}

func runAvatar(r *inv, a *parsed) int {
	remove := a.has("remove")
	if remove == (len(a.pos) == 1) {
		return usageFail(r, "profile avatar", "give a picture file, or --remove")
	}
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	cl, err := c.AuthAPI(core.DefaultTimeout)
	if err != nil {
		return r.failErr(err)
	}
	if remove {
		if err := cl.Call(r.ctx, "account.remove_avatar", nil, nil); err != nil {
			return r.failErr(err)
		}
		r.println("removed your picture")
		return 0
	}
	p, err := artifacts.Prepare(a.pos[0], artifacts.Options{Home: c.Dir}) // never a file in Teamflow's home
	var big *artifacts.TooLargeError
	if errors.As(err, &big) {
		p, err = big.Prepared, nil
	}
	if err != nil {
		return r.failErr(err)
	}
	if p.Size > protocol.AvatarMaxBytes {
		return r.fail(fmt.Sprintf("%s is over the 200 KB limit for a picture", artifacts.SizeText(&p.Size)))
	}
	media := http.DetectContentType(p.Data)
	if !slices.Contains(avatarTypes, media) {
		return r.fail("a picture is a PNG, JPEG or WebP image")
	}
	var acc protocol.Account
	if err := cl.Call(r.ctx, "account.set_avatar", protocol.AvatarSet{MediaType: media,
		ContentB64: base64.StdEncoding.EncodeToString(p.Data)}, &acc); err != nil {
		return r.failErr(err)
	}
	r.println("set your picture")
	return 0
}

// ------------------------------------------------------------------ tasks

var tasksCmd = &command{name: "tasks", help: "The project's tasks (plan updates): kind, state and attempts.",
	flags: []flagSpec{projectFlag, {name: "kind", help: "Only tasks of this kind (ROLLING_UPDATE_PROJECT)"},
		{name: "state", help: "Only tasks in this state: QUEUED, RUNNING, DONE or FAILED"}}, nargs: exactly(0), run: runTasks}

func runTasks(r *inv, a *parsed) int {
	params := protocol.TaskList{}
	if k := strings.ToUpper(a.get("kind")); k != "" {
		if k != string(protocol.TaskKindRollingUpdateProject) {
			return usageFail(r, "tasks", "Invalid value for '--kind': "+a.get("kind"))
		}
		kind := protocol.TaskKind(k)
		params.Kind = &kind
	}
	if s := strings.ToUpper(a.get("state")); s != "" {
		states := []string{string(protocol.TaskStateQueued), string(protocol.TaskStateRunning),
			string(protocol.TaskStateDone), string(protocol.TaskStateFailed)}
		if !slices.Contains(states, s) {
			return usageFail(r, "tasks", "Invalid value for '--state': "+a.get("state"))
		}
		state := protocol.TaskState(s)
		params.State = &state
	}
	_, pid, cl, ok := r.projectAPI(a)
	if !ok {
		return 1
	}
	params.Project = pid
	var ts protocol.Tasks
	if err := cl.Call(r.ctx, "task.list", params, &ts); err != nil {
		return r.failErr(err)
	}
	for _, tv := range ts.Tasks {
		t := tv.Task
		r.printf("%s\t%s\t%s\tattempts %d\t%s\n", t.ID, t.Kind, t.State, t.Attempts, t.RequestedAt)
	}
	return 0
}
