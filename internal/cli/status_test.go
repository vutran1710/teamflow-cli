package cli

// `teamflow project status`, /teamf status (the relay's status: this machine's lines, then the project at a
// glance), /teamf upgrade and its alias update, and `teamflow config auto-update`.
//
// Sad paths covered: no project and no session, a session on no project, outside any session, offline (with a last
// sync, and a project never synced), a project the replica lacks while offline, a project this member is not in,
// signed out with a replica, the org's name unknown (asked once, then kept), the dashboard told once of an update;
// for upgrade: up to date, offline, rate limited, a bad signature (nothing changed), a binary folder that can't be
// written, the background sync failing to stop, extra words, run by an agent without the relay (exit 1), --json;
// for config: a bad word, no word, the env forcing it off.

import (
	"context"
	goflag "flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/adapters/claudecode"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate/selfupdatetest"
)

var updateGolden = goflag.Bool("update", false, "rewrite the golden files in testdata")

func goldenFile(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs\n--- got\n%s--- want\n%s", name, got, want)
	}
}

// ict is the person's zone in these tests.
var ict = time.FixedZone("ICT", 7*3600)

func setClock(t *testing.T, at time.Time) {
	t.Helper()
	old := clock
	clock = func() time.Time { return at }
	t.Cleanup(func() { clock = old })
}

// dashMachine is a machine signed in with its key for P (Pricing, in Acme) as huy (M1): the plan v1, the golden
// Deliverables v1 (Design, due 2026-01-25: A1 lan's, due 01-20; A2 no owner, due 01-23. Ship, due 02-04: A3
// huy's, due 01-30), an approval asked of huy, one open drift; the clock at 2026-01-24 10:00 in UTC+7.
func dashMachine(t *testing.T) *machine {
	t.Helper()
	m := newMachine(t, true)
	m.key()
	m.project()
	m.seed(goldenEvent(t, `event.project.updated~"plan":"Ship v1"`), goldenEvent(t, `event.signal.accepted~"PENDING"`),
		goldenEvent(t, `event.drift.found~"PLAN_CONFLICT"`))
	withDeliverables(m)
	m.names(map[string]string{M1: "huy", M2: "lan"})
	m.srv.on("account.projects", "account.projects")
	setClock(t, time.Date(2026, 1, 24, 10, 0, 0, 0, ict))
	return m
}

// ------------------------------------------------------------------ teamflow project status

func TestProjectStatus(t *testing.T) {
	m := dashMachine(t)
	r := m.run("", "project", "status", "--project", P)
	wantCode(t, r, 0)
	goldenFile(t, "project-status.txt", r.out)
	if len(m.srv.sent("sync.poll")) == 0 {
		t.Fatal("drawn without a sync first")
	}
	onlyCall(t, m.srv, "account.projects") // the org's name: asked once
	r = m.run("", "project", "status", "--project", P)
	wantCode(t, r, 0)
	onlyCall(t, m.srv, "account.projects") // then kept
	contains(t, "stdout", r.out, "Pricing (Acme)")
	if r.err != "" {
		t.Fatalf("stderr: %s", r.err)
	}
}

// Inside an agent session on the project: its member is you, its topics are named, and --project is not needed.
func TestProjectStatusInASession(t *testing.T) {
	m := dashMachine(t)
	m.inSession("s1", P, []string{T1})
	r := m.run("", "project", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "you: huy\n", "\nThis session: topics Rounding, Plan (built-in, always followed)\n",
		"Waiting for your approval: 1    Open drifts: 1\n")
}

func TestProjectStatusJSON(t *testing.T) {
	m := dashMachine(t)
	r := m.run("", "project", "status", "--project", P, "--json")
	wantCode(t, r, 0)
	o := jsonObject(t, r)
	for _, k := range []string{"project", "you", "today", "plan_version", "deliverables", "progress", "milestones",
		"counts", "needs_you", "due_soon", "approvals", "drifts", "session", "offline", "synced_at", "auto_update", "text"} {
		if _, ok := o[k]; !ok {
			t.Fatalf("no %q: %s", k, r.out)
		}
	}
	if o["approvals"] != float64(1) || o["drifts"] != float64(1) || o["you"] != "huy" || o["today"] != "2026-01-24" {
		t.Fatalf("%s", r.out)
	}
	if n := o["needs_you"].([]any); len(n) != 1 || n[0].(map[string]any)["name"] != "Implement rounding" {
		t.Fatalf("needs_you: %v", n)
	}
	// No project: the failure is JSON too.
	r = m.run("", "project", "status", "--json")
	wantCode(t, r, 1)
	if o := jsonObject(t, r); o["error"] != whichProject {
		t.Fatalf("%s", r.out)
	}
}

func TestProjectStatusSadPaths(t *testing.T) {
	t.Run("no project, no session", func(t *testing.T) {
		m := dashMachine(t)
		r := m.run("", "project", "status")
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, whichProject)
	})
	t.Run("offline: from the replica, as of its last sync", func(t *testing.T) {
		m := dashMachine(t)
		if err := m.store().SetSyncedAt(P, time.Date(2026, 1, 24, 2, 12, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		m.offline()
		r := m.run("", "project", "status", "--project", P)
		wantCode(t, r, 0)
		if !strings.HasPrefix(r.out, "(offline: as of 2026-01-24 09:12)\nPricing   ") || strings.Contains(r.out, "(Acme)") {
			t.Fatalf("%s", r) // the org: never named yet, and offline
		}
		contains(t, "stdout", r.out, "Needs you (1)")
	})
	t.Run("offline, never synced", func(t *testing.T) {
		m := dashMachine(t)
		m.offline()
		r := m.run("", "project", "status", "--project", P)
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "(offline: never synced)\n")
	})
	t.Run("a project the replica lacks, offline", func(t *testing.T) {
		m := newMachine(t, true)
		m.offline()
		r := m.run("", "project", "status", "--project", P)
		wantCode(t, r, 1)
		lacks(t, "stdout", r.out, "Milestones")
	})
	t.Run("a project you are not in", func(t *testing.T) {
		m := newMachine(t, true)
		m.srv.on("key.fetch", "key.fetch!NOT_A_MEMBER")
		r := m.run("", "project", "status", "--project", "pj_00000000000000000000000009")
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "NOT_A_MEMBER")
	})
	t.Run("signed out, with a replica", func(t *testing.T) {
		m := dashMachine(t)
		if err := config.Save(m.dir, config.Config{Server: m.srv.URL()}); err != nil {
			t.Fatal(err)
		}
		r := m.run("", "project", "status", "--project", P)
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "Pricing\n", "Milestones")
		if n := len(m.srv.methods()); n != 0 {
			t.Fatalf("signed out, it called %v", m.srv.methods())
		}
	})
}

// The release the background sync updated teamflow to is told once; auto-update off is said while off.
func TestProjectStatusFooter(t *testing.T) {
	m := dashMachine(t)
	if err := m.store().SetUpdatedTo("v0.1.2"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetAutoUpdate(m.dir, false); err != nil {
		t.Fatal(err)
	}
	r := m.run("", "project", "status", "--project", P)
	contains(t, "stdout", r.out, "\nauto-update: off\nteamflow was updated to v0.1.2.\n")
	r = m.run("", "project", "status", "--project", P)
	contains(t, "stdout", r.out, "\nauto-update: off\n")
	lacks(t, "stdout", r.out, "was updated")
	t.Setenv(config.AutoUpdateEnv, "")
	if err := config.SetAutoUpdate(m.dir, true); err != nil {
		t.Fatal(err)
	}
	lacks(t, "stdout", m.run("", "project", "status", "--project", P).out, "auto-update")
	t.Setenv(config.AutoUpdateEnv, "0")
	contains(t, "stdout", m.run("", "project", "status", "--project", P).out, "auto-update: off")
}

// project status is a work command: an agent runs it with no prompt; config is a setup command.
func TestProjectStatusIsAWorkCommand(t *testing.T) {
	for _, cmd := range []string{"teamflow project status", "teamflow project status --json", "teamflow project status --project " + P,
		"'/opt/team flow/teamflow' project status"} {
		if tf, auto := adapters.TeamflowCommand(cmd, "/opt/team flow/teamflow"); !tf || !auto {
			t.Errorf("%q: teamflow %v auto %v", cmd, tf, auto)
		}
	}
	for _, cmd := range []string{"teamflow config auto-update off", "teamflow project status && rm -rf x", "teamflow project status > out"} {
		if _, auto := adapters.TeamflowCommand(cmd, ""); auto {
			t.Errorf("%q is auto-allowed", cmd)
		}
	}
	if !slices.Contains(claudecode.AllowRules(""), "Bash(teamflow project:*)") {
		t.Fatal("Claude Code's allow rules do not cover project status")
	}
	if !slices.Contains(adapters.SetupCommands, "config") || slices.Contains(adapters.WorkCommands, "config") {
		t.Fatal("config must be a setup command")
	}
}

// ------------------------------------------------------------------ /teamf status

func TestTeamStatusShowsTheProject(t *testing.T) {
	m := dashMachine(t)
	m.srv.on("account.me", lanMe)
	m.inSession("s1", P, nil)
	r := m.run("", "team", "--relay", "status")
	wantCode(t, r, 0)
	status, board, ok := strings.Cut(r.out, "\n\nPricing (Acme)")
	if !ok {
		t.Fatalf("no project after the status lines:\n%s", r.out)
	}
	contains(t, "status lines", status, "config: ", "this session: s1 (shared with "+P+")", "claude-code: ")
	contains(t, "the project", board, "you: huy\n", // the session's topics wrapped to 64 columns
		"\nThis session:\n  all topics: Rounding, Currency, Plan (built-in, always\n  followed)\n")
	lacks(t, "stdout", r.out, pickProject)
	// The same through the plain team command, and as JSON (an agent's run).
	contains(t, "stdout", m.run("", "team", "status").out, "Pricing (Acme)")
	r = m.run("", "team", "status", "--json")
	o := jsonObject(t, r)
	if p, _ := o["project"].(map[string]any); p == nil || p["drifts"] != float64(1) || o["status"] == nil {
		t.Fatalf("%s", r.out)
	}
}

func TestTeamStatusWithoutAProject(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.me", lanMe)
	m.inSession("s1", "", nil)
	r := m.run("", "team", "--relay", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "this session: s1 (not shared)\n", "\n"+pickProject+"\n")
	lacks(t, "stdout", r.out, "Milestones", "=====")
	// Outside any session: the status lines, and how to pick one.
	m = newMachine(t, true)
	m.srv.on("account.me", lanMe)
	r = m.run("", "team", "--relay", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "config: ", pickProject)
	r = m.run("", "team", "status", "--json")
	if o := jsonObject(t, r); o["message"] != pickProject || o["project"] != nil || o["auto_update"] != true {
		t.Fatalf("%s", r.out)
	}
	// With no project drawn, the update notes still end it: auto-update off, and an update told once.
	if err := config.SetAutoUpdate(m.dir, false); err != nil {
		t.Fatal(err)
	}
	if err := m.store().SetUpdatedTo("v0.1.2"); err != nil {
		t.Fatal(err)
	}
	r = m.run("", "team", "--relay", "status")
	if !strings.HasSuffix(r.out, pickProject+"\nauto-update: off\nteamflow was updated to v0.1.2.\n") {
		t.Fatalf("%s", r)
	}
	if r = m.run("", "team", "--relay", "status"); !strings.HasSuffix(r.out, pickProject+"\nauto-update: off\n") {
		t.Fatalf("told twice: %s", r)
	}
}

// The project can't be drawn (offline, never synced, no key): the status lines stand, and it says why.
func TestTeamStatusProjectNotDrawn(t *testing.T) {
	m := newMachine(t, true)
	m.inSession("s1", P, nil)
	m.offline()
	r := m.run("", "team", "--relay", "status")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "config: ", "The project at a glance could not be drawn: ")
}

// ------------------------------------------------------------------ /teamf upgrade (and update)

// realUpdate points self-update at a fake release (rel; nil: an unreachable GitHub) for a binary of version 0.1.0
// in its own folder, and counts the stops of the background sync.
func realUpdate(t *testing.T, rel *selfupdatetest.Release) (exe string, stops *int) {
	t.Helper()
	exe = selfupdatetest.Installed(t, t.TempDir(), "0.1.0")
	n := 0
	oldU, oldS := update, stopSync
	update = func(ctx context.Context, out io.Writer) (selfupdate.Result, error) {
		u := selfupdatetest.OfflineUpdater(t, exe)
		if rel != nil {
			u = rel.Updater(exe, out)
		}
		return u.Install(ctx, "0.1.0")
	}
	stopSync = func(string) (bool, error) { n++; return true, nil }
	t.Cleanup(func() { update, stopSync = oldU, oldS })
	return exe, &n
}

func binaryIs(t *testing.T, exe, version string) {
	t.Helper()
	if b, _ := os.ReadFile(exe); string(b) != selfupdatetest.Script(version) {
		t.Fatalf("the binary is %q, not %s's", b, version)
	}
}

func TestTeamUpgrade(t *testing.T) {
	for _, word := range []string{"upgrade", "update"} {
		t.Run(word+": updated", func(t *testing.T) {
			m := newMachine(t, true)
			exe, stops := realUpdate(t, selfupdatetest.New(t, "v0.1.1", ""))
			r := m.run("", "team", "--relay", word)
			wantCode(t, r, 0)
			if r.out != "Updated teamflow v0.1.0 -> v0.1.1. New sessions use it right away.\n" {
				t.Fatalf("%s", r)
			}
			binaryIs(t, exe, "0.1.1")
			if *stops != 1 {
				t.Fatalf("the background sync was stopped %d times", *stops)
			}
		})
		t.Run(word+": up to date", func(t *testing.T) {
			m := newMachine(t, true)
			rel := selfupdatetest.New(t, "v0.1.0", "")
			exe, stops := realUpdate(t, rel)
			r := m.run("", "team", "--relay", word)
			wantCode(t, r, 0)
			if r.out != "teamflow is up to date (v0.1.0).\n" || *stops != 0 || rel.Downloaded() {
				t.Fatalf("%s stops %d", r, *stops)
			}
			binaryIs(t, exe, "0.1.0")
		})
	}
	t.Run("offline", func(t *testing.T) {
		m := newMachine(t, true)
		exe, stops := realUpdate(t, nil)
		r := m.run("", "team", "--relay", "upgrade")
		wantCode(t, r, 0)
		if r.out != "Could not update teamflow: GitHub could not be reached to find the latest release (offline?). Nothing was changed.\n" {
			t.Fatalf("%s", r)
		}
		binaryIs(t, exe, "0.1.0")
		if *stops != 0 {
			t.Fatal("stopped the sync on a refusal")
		}
	})
	t.Run("rate limited", func(t *testing.T) {
		m := newMachine(t, true)
		rel := selfupdatetest.New(t, "v0.1.1", "")
		rel.Answer(http.StatusTooManyRequests)
		exe, _ := realUpdate(t, rel)
		r := m.run("", "team", "--relay", "upgrade")
		contains(t, "stdout", r.out, "Could not update teamflow: GitHub's API rate limit is reached", "Nothing was changed.")
		binaryIs(t, exe, "0.1.0")
	})
	t.Run("bad signature: nothing changed", func(t *testing.T) {
		m := newMachine(t, true)
		rel := selfupdatetest.New(t, "v0.1.1", "")
		rel.SignedByAnotherKey()
		exe, stops := realUpdate(t, rel)
		r := m.run("", "team", "--relay", "upgrade")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "Could not update teamflow: release v0.1.1's checksums.txt failed its signature check",
			"Nothing was changed.")
		binaryIs(t, exe, "0.1.0")
		if *stops != 0 || rel.Downloaded() {
			t.Fatal("went on after a bad signature")
		}
		entries, _ := os.ReadDir(filepath.Dir(exe))
		if len(entries) != 1 {
			t.Fatalf("left beside the binary: %v", entries)
		}
	})
	t.Run("a binary folder that can't be written", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		m := newMachine(t, true)
		rel := selfupdatetest.New(t, "v0.1.1", "")
		exe, _ := realUpdate(t, rel)
		dir := filepath.Dir(exe)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		r := m.run("", "team", "--relay", "upgrade")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "Could not update teamflow: cannot write to ", "Nothing was changed.")
		binaryIs(t, exe, "0.1.0")
		if rel.Asked() != 0 {
			t.Fatal("asked GitHub with nowhere to put the binary")
		}
	})
	t.Run("the background sync does not stop", func(t *testing.T) {
		m := newMachine(t, true)
		realUpdate(t, selfupdatetest.New(t, "v0.1.1", ""))
		stopSync = func(string) (bool, error) { return false, io.ErrUnexpectedEOF }
		r := m.run("", "team", "--relay", "upgrade")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "Updated teamflow v0.1.0 -> v0.1.1.", "\nThe background sync could not be stopped (unexpected EOF): run `teamflow sync --stop`")
	})
	t.Run("extra words", func(t *testing.T) {
		m := newMachine(t, true)
		_, _ = realUpdate(t, nil)
		r := m.run("", "team", "--relay", "upgrade now")
		wantCode(t, r, 0)
		contains(t, "stdout", r.out, "Teamflow refused: usage: ")
	})
	t.Run("an agent's run, no relay: a refusal exits 1", func(t *testing.T) {
		m := newMachine(t, true)
		_, _ = realUpdate(t, nil)
		r := m.run("", "team", "upgrade")
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, "Nothing was changed.")
		r = m.run("", "team", "upgrade", "--json")
		wantCode(t, r, 1)
		contains(t, "error", jsonObject(t, r)["error"].(string), "Nothing was changed.")
	})
	t.Run("--json", func(t *testing.T) {
		m := newMachine(t, true)
		realUpdate(t, selfupdatetest.New(t, "v0.1.1", ""))
		r := m.run("", "team", "update", "--json")
		wantCode(t, r, 0)
		o := jsonObject(t, r)
		if o["updated"] != true || o["from"] != "v0.1.0" || o["to"] != "v0.1.1" {
			t.Fatalf("%s", r.out)
		}
	})
}

// The relay's usage and the team command's help name upgrade; /teamf's text lists it, and tells no agent to
// upgrade on its own.
func TestTeamUpgradeIsListed(t *testing.T) {
	m := newMachine(t, true)
	r := m.run("", "team", "--relay", "dance")
	contains(t, "stdout", r.out, "| upgrade")
	contains(t, "help", m.run("", "team", "--help").out, "upgrade")
	b, err := os.ReadFile(filepath.Join(filepath.Dir(goldenDir), "agent", "teamf.md"))
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "teamf.md", string(b), "| upgrade", "code block")
	b, err = os.ReadFile(filepath.Join(filepath.Dir(goldenDir), "agent", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	lacks(t, "instructions.md", strings.ToLower(string(b)), "upgrade", "self-update")
}

// ------------------------------------------------------------------ teamflow config auto-update

func TestConfigAutoUpdate(t *testing.T) {
	m := newMachine(t, true)
	t.Setenv(config.AutoUpdateEnv, "")
	r := m.run("", "config", "auto-update")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "auto-update: on")
	r = m.run("", "config", "auto-update", "off")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "auto-update: off;")
	cfg, _ := config.Load(m.dir)
	if on, _ := config.AutoUpdate(cfg); on || cfg.AuthKey != auth {
		t.Fatalf("%+v", cfg)
	}
	contains(t, "stdout", m.run("", "config", "auto-update", "ON").out, "auto-update: on")
	r = m.run("", "config", "auto-update", "maybe")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "say on or off, not maybe")
	r = m.run("", "config", "auto-update", "on", "off")
	wantCode(t, r, 2)
	t.Setenv(config.AutoUpdateEnv, "0")
	contains(t, "stdout", m.run("", "config", "auto-update", "on").out, "auto-update: off (TEAMFLOW_AUTO_UPDATE=0 in this shell)")
}
