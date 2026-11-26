package live

// The scheduled self-update: the background sync checks for a newer release about First after it starts, then
// every Every (give or take Jitter); the last check is kept in the store, so a restart does not check again. A
// newer signed release is installed by the same code as `teamflow self-update` (package selfupdate: the signature,
// the tag it was signed for and the checksum are checked; never a downgrade or a pre-release; an atomic rename),
// while every outbox's sender lock is held, so a send in flight finishes first. Then Run returns Updated and the
// caller restarts the sync on the new binary; each session hears "teamflow was updated to vX.Y.Z.".
//
// Every check is recorded whatever comes of it, so nothing is retried before the next one: offline, GitHub's rate
// limit, a release that fails a check. A binary folder that can't be written leaves one notice per release ("a new
// teamflow vX.Y.Z is available: run /teamf upgrade"). It is off with `teamflow config auto-update off` or
// TEAMFLOW_AUTO_UPDATE=0, and for a build that is not a release (dev). What it does goes to the sync log, with
// home paths masked; nothing secret is in it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/selfupdate"
	"github.com/vutran1710/teamflow-cli/internal/sendlock"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// The schedule's defaults.
const (
	DefaultUpdateFirst  = 10 * time.Minute
	DefaultUpdateEvery  = 24 * time.Hour
	DefaultUpdateJitter = time.Hour
	DefaultLockWait     = 30 * time.Second
	updateTimeout       = 10 * time.Minute
)

// AutoUpdate is the scheduled self-update (Options.AutoUpdate; nil: none).
type AutoUpdate struct {
	Updater  *selfupdate.Updater // the running binary's self-update (its Hold and Out are set here)
	Current  string              // this build's version
	First    time.Duration       // the first check, this long after the sync starts (10 min)
	Every    time.Duration       // then this often (24 h) ...
	Jitter   time.Duration       // ... give or take up to this (1 h)
	LockWait time.Duration       // the swap waits this long for each outbox's sender lock (30 s)
	Rand     func() float64      // in [0, 1): the jitter (math/rand when nil)
}

func (a *AutoUpdate) withDefaults() *AutoUpdate {
	c := *a
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.First, DefaultUpdateFirst)
	def(&c.Every, DefaultUpdateEvery)
	def(&c.Jitter, DefaultUpdateJitter)
	def(&c.LockWait, DefaultLockWait)
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	return &c
}

// Updated ends Run when the binary on disk is newer than this sync: the scheduled self-update put a release there
// (Version), or another install or update did (Version ""). The caller restarts the sync on Exe.
type Updated struct {
	Exe     string // the binary to run (as the sync was started)
	Version string // the release the scheduled update put there ("": not this sync's doing)
}

func (u *Updated) Error() string {
	if u.Version == "" {
		return "the teamflow on disk changed: the background sync restarts on it"
	}
	return "teamflow was updated to " + u.Version + ": the background sync restarts on the new binary"
}

// Notices of the scheduled self-update (client-only types: the server knows nothing of them), about the release.
const (
	NoticeUpdated   protocol.NoticeType = "TEAMFLOW_UPDATED"
	NoticeAvailable protocol.NoticeType = "TEAMFLOW_UPDATE_AVAILABLE"
)

// UpdatedText and AvailableText are what each session hears.
func UpdatedText(version string) string { return "teamflow was updated to " + version + "." }
func AvailableText(version string) string {
	return "a new teamflow " + version + " is available: run /teamf upgrade"
}

// updater is the schedule's state in one run.
type updater struct {
	a      *AutoUpdate
	last   time.Time // the last check (the store's, then each made)
	jitter time.Duration
	loaded bool
	exe    string      // the binary this sync started from (a link resolved) …
	start  os.FileInfo // … as it was then (nil: it could not be read)
}

// newUpdater is the schedule of a sync started now from a.Updater.Exe: that binary's identity is recorded, so an
// update never replaces another binary put there since.
func newUpdater(a *AutoUpdate) *updater {
	u := &updater{a: a.withDefaults()}
	if exe, err := filepath.EvalSymlinks(a.Updater.Exe); err == nil && a.Updater.Exe != "" {
		if fi, err := os.Stat(exe); err == nil {
			u.exe, u.start = exe, fi
		}
	}
	return u
}

// same says whether the binary on disk is still the one this sync started from: the same file, saying the same
// version.
func (u *updater) same(string) error {
	if u.start == nil {
		return errors.New("the binary this sync started from could not be read")
	}
	if now, err := os.Stat(u.exe); err != nil || !os.SameFile(u.start, now) {
		return errors.New("the binary on disk is not the one this background sync started from")
	}
	return selfupdate.ExpectVersion(u.a.Current)(u.exe)
}

func (u *updater) draw() {
	u.jitter = time.Duration((2*u.a.Rand() - 1) * float64(u.a.Jitter))
}

// due says whether a check is due at now: First after the sync started, and Every ± the jitter after the last
// check (none yet, or one ahead of now from a clock set back: due).
func (u *updater) due(now, started time.Time) bool {
	if now.Before(started.Add(u.a.First)) {
		return false
	}
	if u.last.IsZero() || u.last.After(now) {
		return true
	}
	return !now.Before(u.last.Add(u.a.Every + u.jitter))
}

// selfUpdate checks for a newer release when one is due and auto-update is on; a new binary in place ends the run
// (Updated).
func (r *runner) selfUpdate(ctx context.Context) *Updated {
	u := r.update
	if u == nil {
		return nil
	}
	if !u.loaded {
		u.last, u.loaded = r.st.UpdateCheckedAt(), true
		u.draw()
	}
	now := r.now()
	if !u.due(now, r.started) {
		return nil
	}
	if on, why := config.AutoUpdate(r.cfg); !on {
		r.logOnce("auto-update off", "teamflow sync: auto-update is off (%s)", why)
		return nil
	}
	if !selfupdate.IsRelease(u.a.Current) {
		r.logOnce("auto-update dev", "teamflow sync: this build (%s) is not a release: no scheduled update", u.a.Current)
		return nil
	}
	if err := u.same(""); err != nil { // another binary was put there since this sync started: it is not ours to replace
		return r.restartOnDisk(err)
	}
	// Recorded first, whatever comes of it: nothing is tried again before the next check.
	if err := r.tx(func(tx *store.Tx) error { return tx.SetUpdateCheckedAt(now) }); err != nil {
		r.o.Logf("teamflow sync: could not record the update check: %v", err)
		return nil // a busy store: tried again at the next round
	}
	u.last = now
	u.draw()
	up := *u.a.Updater
	up.Out, up.Replaced, up.Expect = io.Discard, nil, u.same
	up.LockFile = filepath.Join(r.dir, "update.lock") // takes turns with self-update and /teamf upgrade
	up.Hold = func(ctx context.Context) (func(), error) { return r.holdSenders(ctx, u.a.LockWait) }
	uctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	res, err := up.Install(uctx, u.a.Current)
	switch {
	case err == nil && res.Updated:
		r.o.Logf("teamflow sync: updated teamflow %s -> %s; restarting on the new binary", u.a.Current, res.Latest)
		r.noticeAll(NoticeUpdated, res.Latest, UpdatedText(res.Latest), now)
		if err := r.tx(func(tx *store.Tx) error { return tx.SetUpdatedTo(res.Latest) }); err != nil {
			r.o.Logf("teamflow sync: could not record the update for /teamf status: %v", err)
		}
		exe := up.Exe
		if exe == "" {
			exe = res.Exe
		}
		return &Updated{Exe: exe, Version: res.Latest}
	case err == nil:
		r.o.Logf("teamflow sync: teamflow %s is up to date (the latest release is %s)", u.a.Current, res.Latest)
	case selfupdate.KindOf(err) == selfupdate.Changed:
		return r.restartOnDisk(err)
	case selfupdate.KindOf(err) == selfupdate.NotWritable:
		r.o.Logf("teamflow sync: a new release can't be installed here: %s", home.Mask(err.Error()))
		latest, lerr := up.Latest(uctx)
		if lerr != nil || !selfupdate.Newer(latest, u.a.Current) {
			return nil
		}
		first, ferr := r.st.FirstNoticeOf(latest)
		if ferr == nil && first {
			r.noticeAll(NoticeAvailable, latest, AvailableText(latest), now)
		}
	default:
		r.o.Logf("teamflow sync: no update this time (%s); the next check is in about %v", home.Mask(err.Error()),
			u.a.Every.Round(time.Hour))
	}
	return nil
}

// restartOnDisk ends the run so the sync restarts on the binary now on disk (put there by another install or
// update since this sync started): nothing is replaced, nothing is told.
func (r *runner) restartOnDisk(why error) *Updated {
	r.o.Logf("teamflow sync: %s; restarting on it", home.Mask(why.Error()))
	exe := r.update.a.Updater.Exe
	if exe == "" {
		exe = r.update.exe
	}
	return &Updated{Exe: exe}
}

// holdSenders takes the sender lock of every outbox of this sign-in, waiting up to wait for each: no send is in
// flight while it is held.
func (r *runner) holdSenders(ctx context.Context, wait time.Duration) (func(), error) {
	projects, err := r.st.KeyProjects(r.binding)
	if err != nil {
		return nil, err
	}
	waiting, err := r.st.OutboxProjects(r.binding)
	if err != nil {
		return nil, err
	}
	for _, p := range waiting {
		if !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
	}
	slices.Sort(projects)
	var held []func()
	release := func() {
		for _, h := range held {
			h()
		}
	}
	for _, p := range projects {
		h, err := sendlock.Acquire(ctx, r.dir, store.OutboxOf(r.binding, p).Ref(), wait)
		if err != nil {
			release()
			return nil, fmt.Errorf("a send was still in flight after %v (%v); the next check tries again", wait, err)
		}
		held = append(held, h)
	}
	return release, nil
}

// noticeAll queues a notice for every session seen lately, and for the person's own commands outside one.
func (r *runner) noticeAll(typ protocol.NoticeType, about, text string, now time.Time) {
	sessions, err := r.st.SessionsSince(now.Add(-agentWindow))
	if err != nil {
		r.o.Logf("teamflow sync: could not queue the notice %q: %v", text, err)
		return
	}
	ids := []string{""}
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	err = r.tx(func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.Notice(id, typ, about, text, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.o.Logf("teamflow sync: could not queue the notice %q: %v", text, err)
	}
}
