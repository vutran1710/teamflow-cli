package core

// The outbox: everything this machine records on a project's shared record leaves in the order it was made.
//
//   - Signals (agent signals, artifact links, action results and revokes) and human acts (verdicts, drift resolve
//     and dismiss) are queued in the project key's outbox (the store's spool) and sent by a flush, in order.
//   - A human act is queued at the end, then the flush sends everything before it in batches, then the act alone.
//     If the flush stops before it (offline, a stuck item), the act waits in its place: Queued says so.
//   - Other writes to the shared record (artifact write and archive, Deliverables, topic and plan changes, members)
//     are not queued: they pass the client's gate, which flushes the outbox first and refuses to send (NotSent)
//     while anything is still queued, so they never overtake it.
//   - The outbox belongs to a sign-in and a project, not a key: what was queued goes out under whatever key that
//     sign-in holds for the project when it is sent (after a logout and login, a refused key fetched again);
//     another sign-in's items are kept, never sent by this one, and listed (`teamflow outbox`, `teamflow status`).
//   - One sender at a time per outbox, across processes: the sender lock (sendlock) in the Teamflow home.
//     A sync (every command, every hook) waits SyncLockWait for it, then leaves the outbox to the holder; a
//     command sending its own items waits SendLockWait, then leaves them queued.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/sendlock"
	"github.com/vutran1710/teamflow-cli/internal/store"
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// SyncLockWait is how long a sync waits for the sender lock before leaving the outbox to its holder.
var SyncLockWait = 250 * time.Millisecond

// KeyRetryAfter is how long a sync waits before asking again for the key of an outbox whose membership the server
// refused (items wait for it; asking on every command and hook would only be refused again).
const KeyRetryAfter = 10 * time.Minute

// SendLockWait is how long a command sending its own items (a signal, a human act, a write) waits for the lock.
var SendLockWait = 3 * time.Second

// SharedWrites are the methods that change a project's shared record. signal.emit, drift.resolve and
// drift.dismiss are queued in the outbox; the others flush it first and never overtake it.
var SharedWrites = map[string]bool{
	"signal.emit": true, "drift.resolve": true, "drift.dismiss": true,
	"artifact.write": true, "artifact.archive": true,
	"deliverables.publish": true, "deliverables.update_milestone": true, "deliverables.rename": true,
	"deliverables.archive": true, "deliverables.delete": true,
	"project.add_members": true, "project.remove_member": true, "project.set_title": true,
	"project.write_state": true, "project.request_update": true,
	"topic.create": true, "topic.update": true, "topic.delete": true,
}

// NotSharedWrites are every other server method: reads, the account and the org (no project's outbox), a new
// project (nothing queued for it yet), the live ticket, the runner's and the admin's.
var NotSharedWrites = map[string]bool{
	"auth.login": true, "auth.logout": true, "account.me": true, "account.projects": true, "key.fetch": true,
	"org.members": true, "org.add_member": true, "org.remove_member": true, "org.set_title": true,
	"account.update_profile": true, "account.set_avatar": true, "account.remove_avatar": true, "people.avatar": true,
	"member.me": true, "member.list": true, "project.create": true, "project.get": true, "project.list": true,
	"project.history": true, "project.provenance": true, "task.get": true, "task.list": true, "drift.list": true,
	"signal.get": true, "signal.list": true, "artifact.get": true, "artifact.list": true, "deliverables.get": true,
	"deliverables.history": true, "sync.poll": true, "live.ticket": true, "runner.started": true,
	"runner.complete": true, "admin.account_add": true, "admin.account_password": true,
	"admin.account_disable": true, "admin.accounts": true, "admin.org_add": true, "admin.org_set_admin": true,
	"admin.org_add_member": true, "admin.orgs": true, "admin.repair": true, "admin.export": true,
	"admin.org_import": true, "admin.directory_import": true,
}

// outbox is the outbox of project pid under this machine's sign-in.
func (c *Core) outbox(pid string) (store.Outbox, error) {
	b, err := c.binding()
	if err != nil {
		return store.Outbox{}, err
	}
	return store.OutboxOf(b, pid), nil
}

// Queued is a human act (or a signal) kept in the outbox behind Ahead earlier items: recorded, and sent in its
// place by a later flush (the next command, hook or sync). It is not a failure, unless Hard.
type Queued struct {
	Item  string // the queued item's id
	Ahead int    // items waiting before it
	Cause error  // why the flush stopped before it (offline, another sender busy, …), or nil
}

func (q *Queued) Error() string {
	if q.Hard() {
		return why(q.Cause) + " (" + queuedMessage(q.Ahead, nil) + ", once that is fixed)"
	}
	return queuedMessage(q.Ahead, q.Cause)
}

// Hard says whether the wait needs the person: the server refused this client or this key (update teamflow, the
// membership revoked, a key refused and no new one to be had), not a server that is unreachable, busy or briefly failing. A command reports it as a
// failure; the item still waits in the outbox, never lost. (NOT_ALLOWED on an item is that item's refusal: it
// leaves the outbox, never waits.)
func (q *Queued) Hard() bool { return hard(q.Cause) }

func hard(err error) bool {
	var ae *api.APIError
	if !errors.As(err, &ae) {
		return false
	}
	return errors.Is(err, api.ErrUpgradeRequired) || errors.Is(err, api.ErrKeyRefused) ||
		ae.Reason == string(protocol.RejectReasonNotAMember)
}

func queuedMessage(ahead int, cause error) string {
	switch {
	case errors.Is(cause, sendlock.ErrBusy):
		// Another process holds the lock: it may already have sent it, so this never claims it waits.
		return "handed to the outbox: another teamflow is sending this project's signals and sends it in its turn " +
			"(the session hears when it reaches the team)"
	case ahead == 0:
		return "queued: will be sent when the server is back"
	}
	msg := fmt.Sprintf("queued: will be sent after %d earlier signals", ahead)
	if ahead == 1 {
		msg = "queued: will be sent after 1 earlier signal"
	}
	if cause != nil {
		msg += " (" + why(cause) + ")"
	}
	return msg
}

func why(err error) string {
	if errors.Is(err, api.ErrOffline) {
		return "the server is unreachable"
	}
	return err.Error()
}

// NotSent is a write that was not sent because Waiting earlier items are still queued and it must not overtake
// them. Cause is why they could not be sent (offline, another sender busy, …).
type NotSent struct {
	Method  string
	Waiting int
	Cause   error
}

func (e *NotSent) Error() string {
	s := "s are"
	if e.Waiting == 1 {
		s = " is"
	}
	msg := fmt.Sprintf("not sent: %d earlier signal%s still queued for this project and %s must not overtake them",
		e.Waiting, s, e.Method)
	if e.Cause != nil {
		msg += " (" + why(e.Cause) + ")"
	}
	return msg + "; try again once they are sent (`teamflow outbox` lists them)"
}

func (e *NotSent) Unwrap() error { return e.Cause }

// lockOutbox takes the sender lock of the outbox ob.
func (c *Core) lockOutbox(ctx context.Context, ob store.Outbox, wait time.Duration) (func(), error) {
	return sendlock.Acquire(ctx, c.Dir, ob.Ref(), wait)
}

// pushOutbox sends the outbox ob with key (the sign-in's current key for its project) when the sender lock is free
// within SyncLockWait, all of it within timeout (a hook's budget); otherwise its holder sends it, and pushOutbox
// does nothing. It returns the key the project has now: a new one when the server refused key itself (see
// flushKeyed).
func (c *Core) pushOutbox(ctx context.Context, key string, ob store.Outbox, timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	release, err := c.lockOutbox(ctx, ob, SyncLockWait)
	if errors.Is(err, sendlock.ErrBusy) {
		return key, nil
	}
	if err != nil {
		return key, err
	}
	defer release()
	_, key, err = c.flushKeyed(ctx, ob, key, timeout, nil)
	return key, err
}

// flushKeyed flushes the outbox ob with key (the caller holds its sender lock). When the server refuses the key
// itself (api.ErrKeyRefused: it no longer knows it, after a restore or the member re-added), every item stays
// queued, the key is dropped, a new one is fetched with the auth key and the flush runs once more under it (never
// more than once: a new key refused as well waits for a later sync). It returns what both flushes sent and the
// key the project has now ("" when none could be fetched).
func (c *Core) flushKeyed(ctx context.Context, ob store.Outbox, key string, timeout time.Duration,
	mine map[string]bool) (syncer.Flushed, string, error) {
	f, err := syncer.Flush(ctx, c.St, c.client(key, timeout), ob, mine)
	if !errors.Is(err, api.ErrKeyRefused) {
		return f, key, err
	}
	nk, kerr := c.renewKey(ctx, ob, key)
	if kerr != nil {
		if errors.Is(kerr, api.ErrOffline) {
			return f, "", kerr
		}
		return f, "", err // the membership refused too: the items wait, the key is asked for after KeyRetryAfter
	}
	g, err := syncer.Flush(ctx, c.St, c.client(nk, timeout), ob, mine)
	g.Sent += f.Sent
	for id, a := range f.Answers {
		g.Answers[id] = a
	}
	return g, nk, err
}

// renewKey drops key, the sign-in's key for ob's project that the server refused for itself, and fetches a new
// one with the auth key (another process that renewed it already leaves its new key, which is used). A refused
// fetch (the membership gone) waits KeyRetryAfter before the key is asked for again.
func (c *Core) renewKey(ctx context.Context, ob store.Outbox, key string) (string, error) {
	b, err := c.binding()
	if err != nil {
		return "", err
	}
	if cur, _, _, _, ok := c.St.ProjectKey(b, ob.Project); ok && cur == key {
		if err := c.St.DropProjectKey(b, ob.Project); err != nil {
			return "", err
		}
	}
	nk, err := c.ProjectKey(ctx, ob.Project)
	if err != nil && !errors.Is(err, api.ErrOffline) {
		_ = c.St.SetKeyFetchRefused(ob, c.Now().Add(KeyRetryAfter))
	}
	return nk, err
}

// sendMine queues the caller's own items (add) and flushes the outbox ob with key, holding the sender lock across
// both (waiting SendLockWait for it), so no other sender takes them and answers them elsewhere. It returns the
// answers to mine that came back and why the flush stopped (sendlock.ErrBusy: another sender holds the lock and
// sends them; keyErr: there is no key to send with, offline). err is a failure to queue them: nothing was queued.
func (c *Core) sendMine(ctx context.Context, ob store.Outbox, key string, keyErr error, timeout time.Duration,
	mine map[string]bool, add func() error) (f syncer.Flushed, stopped, err error) {
	f = syncer.Flushed{Answers: map[string]syncer.Answer{}}
	release, lerr := c.lockOutbox(ctx, ob, SendLockWait)
	if lerr != nil && !errors.Is(lerr, sendlock.ErrBusy) {
		return f, nil, lerr
	}
	if lerr == nil {
		defer release()
	}
	if err := add(); err != nil {
		return f, nil, err
	}
	if lerr != nil {
		return f, lerr, nil
	}
	if keyErr != nil { // no key to send with (offline): queued for the next sync
		return f, keyErr, nil
	}
	f, _, stopped = c.flushKeyed(ctx, ob, key, timeout, mine)
	return f, stopped, nil
}

// gate is the gate of a project key's client: a write to the shared record that is not queued is sent only with
// the outbox empty, under the sender lock, after flushing it. When another sender holds the lock past
// SendLockWait, the write goes only if nothing is queued (nothing to overtake).
func (c *Core) gate(ob store.Outbox, key string) api.Gate {
	return func(ctx context.Context, method string, send func() error) error {
		if !SharedWrites[method] {
			return send()
		}
		release, err := c.lockOutbox(ctx, ob, SendLockWait)
		if err != nil {
			n, werr := c.St.Waiting(ob)
			if werr == nil && n == 0 && errors.Is(err, sendlock.ErrBusy) {
				return send()
			}
			return &NotSent{Method: method, Waiting: n, Cause: err}
		}
		defer release()
		_, _, ferr := c.flushKeyed(ctx, ob, key, DefaultTimeout, nil)
		n, err := c.St.Waiting(ob)
		if err != nil {
			return err
		}
		if n > 0 {
			return &NotSent{Method: method, Waiting: n, Cause: ferr}
		}
		return send()
	}
}

// humanAct records a human act (a verdict's signal.emit, drift.resolve, drift.dismiss) on project pid: queued at
// the end of the project's outbox, then everything before it is flushed in batches and the act goes alone. Its
// answer is the server's result, or its refusal; a *Queued when the flush stopped before it (it waits in its
// place, never sent ahead): Hard when the stop needs the person (an old client, a key no longer good).
func (c *Core) humanAct(ctx context.Context, pid, sid, id, method string, params any) (json.RawMessage, error) {
	ob, err := c.outbox(pid)
	if err != nil {
		return nil, err
	}
	key, kerr := c.ProjectKey(ctx, pid) // offline with no key: queued all the same, sent once a key is fetched
	if kerr != nil && !errors.Is(kerr, api.ErrOffline) {
		return nil, kerr
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	f, stopped, err := c.sendMine(ctx, ob, key, kerr, DefaultTimeout, map[string]bool{id: true}, func() error {
		return c.St.OutboxAdd(id, method, sid, ob, string(raw), c.Now())
	})
	if err != nil {
		return nil, err
	}
	if a, ok := f.Answers[id]; ok {
		return a.Result, a.Err
	}
	ahead, _, err := c.St.Ahead(ob, id)
	if err != nil {
		return nil, err
	}
	return nil, &Queued{Item: id, Ahead: ahead, Cause: stopped}
}

// OutboxItem is an item waiting in an outbox, for the person (`teamflow outbox`, `teamflow status`).
type OutboxItem struct {
	store.OutboxRow
	Mine bool // queued under this sign-in (others are never sent by it)
}

// Outbox is every item waiting on this machine, every sign-in's, in order.
func (c *Core) Outbox() ([]OutboxItem, error) {
	rows, err := c.St.OutboxAll()
	if err != nil {
		return nil, err
	}
	b, berr := c.binding()
	out := make([]OutboxItem, len(rows))
	for i, r := range rows {
		out[i] = OutboxItem{OutboxRow: r, Mine: berr == nil && r.Server == b.Server && r.Account == b.Account}
	}
	return out, nil
}

// OutboxDrop takes one item out of the outbox (the person's decision: it is never sent), under its outbox's
// sender lock so a sender never has it on the wire meanwhile. False when no such item waits.
func (c *Core) OutboxDrop(ctx context.Context, id string) (bool, error) {
	rows, err := c.St.OutboxAll()
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Signal != id {
			continue
		}
		release, err := c.lockOutbox(ctx, r.Outbox, SendLockWait)
		if err != nil {
			return false, err
		}
		defer release()
		if _, ok, err := c.St.Ahead(r.Outbox, id); err != nil || !ok { // sent meanwhile
			return false, err
		}
		return true, c.St.SpoolDone(id)
	}
	return false, nil
}
