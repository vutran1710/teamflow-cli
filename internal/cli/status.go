package cli

// The project at a glance (`teamflow project status`, and /teamf status after this machine's status lines),
// /teamf upgrade (self-update's code, told in a line or two) and `teamflow config auto-update`.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/dashboard"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// clock is the person's clock: the dashboard's dates are its local dates (replaced in tests).
var clock = time.Now

// ------------------------------------------------------------------ the project at a glance

// board is the dashboard of --project, else of the project the session this runs under works on.
func (r *inv) board(a *parsed) (dashboard.Board, error) {
	c, err := r.core()
	if err != nil {
		return dashboard.Board{}, err
	}
	var sess *store.Session
	if s, ok := findSession(c.St); ok {
		sess = &s
	}
	pid := a.get("project")
	if pid == "" && sess != nil {
		pid = sess.Project
	}
	if pid == "" {
		return dashboard.Board{}, errors.New(whichProject)
	}
	return c.Dashboard(r.ctx, pid, sess, clock())
}

func runProjectStatus(r *inv, a *parsed) int {
	b, err := r.board(a)
	if err != nil {
		return r.failErr(err)
	}
	fmt.Fprint(r.out, b.Text())
	return 0
}

func jsonProjectStatus(r *inv, a *parsed) (any, error) {
	b, err := r.board(a)
	if err != nil {
		return nil, err
	}
	return b.JSON(), nil
}

// pickProject is what /teamf status says when the session works on no project.
const pickProject = "Pick a project: /teamf project"

// teamStatus is /teamf status: this machine's status lines, then the session's project at a glance (or how to pick
// one).
func (r *inv) teamStatus() int {
	if code := runStatus(r, nil); code != 0 {
		return code
	}
	c, _ := r.core() // open: runStatus opened it
	sess, ok := r.session()
	if !ok || sess.Project == "" {
		r.println(pickProject)
		r.updateNotes(c)
		return 0
	}
	b, err := c.Dashboard(r.ctx, sess.Project, &sess, clock())
	if err != nil {
		r.println("The project at a glance could not be drawn: " + errText(err))
		r.updateNotes(c)
		return 0
	}
	b.UpdatedTo = r.updatedTo(c)
	r.println("")
	fmt.Fprint(r.out, b.Text()) // its footer says them
	return 0
}

// updatedTo is the release the background sync updated teamflow to, taken by /teamf status (the person's own
// view: told once); "" for any other run (an agent's team status), which leaves it for /teamf status.
func (r *inv) updatedTo(c *core.Core) string {
	if !r.relay {
		return ""
	}
	v, _ := c.St.TakeUpdatedTo()
	return v
}

// updateNotes end /teamf status when no project is drawn: auto-update off, and the release the background sync
// updated teamflow to (told once), as the project's footer says them.
func (r *inv) updateNotes(c *core.Core) {
	if on, _ := config.AutoUpdate(c.Cfg); !on {
		r.println("auto-update: off")
	}
	if v := r.updatedTo(c); v != "" {
		r.println("teamflow was updated to " + v + ".")
	}
}

// teamStatusJSON is /teamf status as JSON: status's answer, with the session's project at a glance.
func (r *inv) teamStatusJSON() (any, error) {
	out, err := jsonStatus(r, nil)
	if err != nil {
		return nil, err
	}
	o, _ := out.(ojson.Obj)
	c, _ := r.core()
	sess, ok := r.session()
	if !ok || sess.Project == "" {
		on, _ := config.AutoUpdate(c.Cfg)
		return o.Set("project", nil).Set("message", pickProject).Set("auto_update", on), nil
	}
	b, err := c.Dashboard(r.ctx, sess.Project, &sess, clock())
	if err != nil {
		return nil, err
	}
	return o.Set("project", b.JSON()), nil
}

// ------------------------------------------------------------------ self-update, /teamf upgrade

// update is the self-update of the running binary, saying what it does on out (replaced in tests). It takes turns
// with the background sync's (the update lock), and replaces the binary only while it is still this build.
var update = func(ctx context.Context, out io.Writer) (selfupdate.Result, error) {
	u := updaterFor(out)
	u.Expect = selfupdate.ExpectVersion(api.Version)
	return u.Install(ctx, api.Version)
}

// updaterFor is the running binary's self-update (replaced in tests: a fake release).
var updaterFor = selfupdate.Default

// upgraded is what an update did, with the background sync (the old version) stopped after a new binary went in.
type upgraded struct {
	selfupdate.Result
	stopped bool  // the running background sync was stopped
	stopErr error // it could not be
}

// upgrade runs the self-update (the release's signature, the tag it was signed for and its checksum are checked
// first); once the new binary is in place, it stops the running background sync, so the next agent hook starts the
// new one. `teamflow self-update` and /teamf upgrade both run it.
func upgrade(ctx context.Context, out io.Writer) (upgraded, error) {
	res, err := update(ctx, out)
	if err != nil || !res.Updated {
		return upgraded{Result: res}, err
	}
	stopped, serr := stopSync(home.Dir())
	return upgraded{Result: res, stopped: stopped, stopErr: serr}, nil
}

// version names a release version as the person reads it: v0.1.1 (a build that is not one, as it is).
func version(v string) string {
	if v == "" || strings.HasPrefix(v, "v") || v[0] < '0' || v[0] > '9' {
		return v
	}
	return "v" + v
}

// upgradeLines are /teamf upgrade's answer: one or two friendly lines, and whether it was refused.
func upgradeLines(u upgraded, err error) ([]string, bool) {
	if err != nil {
		why := strings.TrimRight(home.Mask(err.Error()), ". ")
		if selfupdate.KindOf(err) == selfupdate.Offline {
			why = "GitHub could not be reached to find the latest release (offline?)"
		}
		return []string{"Could not update teamflow: " + why + ". Nothing was changed."}, true
	}
	if !u.Updated {
		v := version(u.Current)
		if !strings.HasPrefix(v, "v") && u.Latest != "" { // a build that is not a release: the latest release stays
			return []string{fmt.Sprintf("teamflow %s stays: the latest release (%s) is not newer.", v, u.Latest)}, false
		}
		return []string{"teamflow is up to date (" + v + ")."}, false
	}
	lines := []string{fmt.Sprintf("Updated teamflow %s -> %s. New sessions use it right away.", version(u.Current), u.Latest)}
	if u.stopErr != nil {
		lines = append(lines, "The background sync could not be stopped ("+u.stopErr.Error()+"): run `teamflow sync --stop`, "+
			"and the next agent hook starts the new one.")
	}
	return lines, false
}

// teamUpgrade is /teamf upgrade (or update): the person typing it is their consent. Only signed releases are
// installed, as with self-update.
func (r *inv) teamUpgrade() int {
	u, err := upgrade(r.ctx, io.Discard)
	lines, refused := upgradeLines(u, err)
	if refused && !r.relay {
		fmt.Fprintln(r.errw, strings.Join(lines, "\n"))
		return 1
	}
	r.println(strings.Join(lines, "\n"))
	return 0
}

func (r *inv) teamUpgradeJSON() (any, error) {
	u, err := upgrade(r.ctx, io.Discard)
	lines, refused := upgradeLines(u, err)
	if refused {
		return nil, errors.New(strings.Join(lines, " "))
	}
	return ojson.Obj{{Key: "updated", Value: u.Updated}, {Key: "from", Value: version(u.Current)},
		{Key: "to", Value: u.Latest}, {Key: "message", Value: strings.Join(lines, " ")}}, nil
}

// ------------------------------------------------------------------ teamflow config auto-update

var configCmd = &command{name: "config", help: "This machine's Teamflow settings.", sub: []*command{{
	name: "auto-update", args: "[on|off]",
	help: "Whether the background sync installs new signed releases on its own (about once a day). " +
		"With no argument: whether it does.",
	nargs: between(0, 1), run: runConfigAutoUpdate,
}}}

func runConfigAutoUpdate(r *inv, a *parsed) int {
	c, err := r.core()
	if err != nil {
		return r.failErr(err)
	}
	if len(a.pos) == 1 {
		var on bool
		switch strings.ToLower(a.pos[0]) {
		case "on":
			on = true
		case "off":
		default:
			return usageFail(r, "config auto-update", "say on or off, not "+a.pos[0])
		}
		if err := config.SetAutoUpdate(c.Dir, on); err != nil {
			return r.failErr(err)
		}
		if c.Cfg, err = config.Load(c.Dir); err != nil {
			return r.failErr(err)
		}
	}
	// The config switch is the state: TEAMFLOW_AUTO_UPDATE reaches only a background sync started with it in its
	// environment (a hook's, an install's), so it is said as a note.
	on := c.Cfg == nil || c.Cfg.AutoUpdate == nil || *c.Cfg.AutoUpdate
	if !on {
		r.println("auto-update: off; /teamf upgrade or `teamflow self-update` updates by hand")
		return 0
	}
	r.println("auto-update: on (the background sync installs new signed releases on its own, about once a day)")
	if envOn, why := config.AutoUpdate(nil); !envOn {
		r.println("note: " + why + " in this shell reaches only a background sync started with it in its environment; " +
			"`teamflow config auto-update off` is the switch that always holds")
	}
	return 0
}
