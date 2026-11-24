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
//   - One sender at a time per project key, across processes: the sender lock (sendlock) in the Teamflow home.
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
	"github.com/vutran1710/teamflow-cli/internal/syncer"
)

// SyncLockWait is how long a sync waits for the sender lock before leaving the outbox to its holder.
var SyncLockWait = 250 * time.Millisecond

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

// Queued is a human act (or a signal) kept in the outbox behind Ahead earlier items: recorded, and sent in its
// place by a later flush (the next command, hook or sync). It is not a failure.
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

// Hard says whether the wait needs the person: the server refused this client or key (update teamflow, the
// membership revoked), not a server that is unreachable, busy or briefly failing. A command reports it as a
// failure; the item still waits in the outbox, never lost.
func (q *Queued) Hard() bool { return hard(q.Cause) }

func hard(err error) bool {
	var ae *api.APIError
	if !errors.As(err, &ae) {
		return false
	}
	return errors.Is(err, api.ErrUpgradeRequired) || ae.Reason == string(protocol.RejectReasonNotAMember) ||
		ae.Reason == string(protocol.RejectReasonNotAllowed)
}

func queuedMessage(ahead int, cause error) string {
	msg := "queued: will be sent when the server is back"
	switch {
	case ahead == 1:
		msg = "queued: will be sent after 1 earlier signal"
	case ahead > 1:
		msg = fmt.Sprintf("queued: will be sent after %d earlier signals", ahead)
	case errors.Is(cause, sendlock.ErrBusy):
		msg = "queued: another teamflow is sending this project's signals; it goes out in order"
	}
	if cause != nil && !errors.Is(cause, sendlock.ErrBusy) && ahead > 0 {
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
	if e.Waiting == 0 {
		return fmt.Sprintf("not sent: %s (%s)", e.Method, why(e.Cause))
	}
	s := "s are"
	if e.Waiting == 1 {
		s = " is"
	}
	msg := fmt.Sprintf("not sent: %d earlier signal%s still queued for this project and %s must not overtake them",
		e.Waiting, s, e.Method)
	if e.Cause != nil {
		msg += " (" + why(e.Cause) + ")"
	}
	return msg + "; try again once they are sent"
}

func (e *NotSent) Unwrap() error { return e.Cause }

// lockOutbox takes the sender lock of the outbox of key.
func (c *Core) lockOutbox(ctx context.Context, key string, wait time.Duration) (func(), error) {
	return sendlock.Acquire(ctx, c.Dir, api.KeyRef(key), wait)
}

// pushOutbox sends the outbox of a's key when the sender lock is free within SyncLockWait; otherwise its holder
// sends it, and pushOutbox does nothing.
func (c *Core) pushOutbox(ctx context.Context, a *api.Client, key string) error {
	release, err := c.lockOutbox(ctx, key, SyncLockWait)
	if errors.Is(err, sendlock.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	_, err = syncer.Push(ctx, c.St, a.Ungated())
	return err
}

// sendMine queues the caller's own items (add) and flushes the outbox of key, holding the sender lock across both
// (waiting SendLockWait for it), so no other sender takes them and answers them elsewhere. It returns the
// answers to mine that came back and why the flush stopped (sendlock.ErrBusy: another sender holds the lock; the
// items are queued for it). err is a failure to queue them: nothing was queued.
func (c *Core) sendMine(ctx context.Context, key string, timeout time.Duration, mine map[string]bool,
	add func() error) (f syncer.Flushed, stopped, err error) {
	f = syncer.Flushed{Answers: map[string]syncer.Answer{}}
	release, lerr := c.lockOutbox(ctx, key, SendLockWait)
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
	f, stopped = syncer.Flush(ctx, c.St, c.client(key, timeout), mine)
	return f, stopped, nil
}

// gate is the gate of a project key's client: a write to the shared record that is not queued is sent only with
// the outbox empty, under the sender lock, after flushing it.
func (c *Core) gate(key string) api.Gate {
	return func(ctx context.Context, method string, send func() error) error {
		if !SharedWrites[method] {
			return send()
		}
		release, err := c.lockOutbox(ctx, key, SendLockWait)
		if err != nil {
			n, _ := c.St.Waiting(api.KeyRef(key))
			return &NotSent{Method: method, Waiting: n, Cause: err}
		}
		defer release()
		_, ferr := syncer.Flush(ctx, c.St, c.client(key, DefaultTimeout), nil)
		n, err := c.St.Waiting(api.KeyRef(key))
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
// answer is the server's result, or its refusal; a *Queued when the flush stopped before it (it waits in its place,
// never sent ahead): Hard when the stop needs the person (an old client, a key no longer good).
func (c *Core) humanAct(ctx context.Context, pid, sid, id, method string, params any) (json.RawMessage, error) {
	key, err := c.ProjectKey(ctx, pid)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	ref := api.KeyRef(key)
	f, stopped, err := c.sendMine(ctx, key, DefaultTimeout, map[string]bool{id: true}, func() error {
		return c.St.OutboxAdd(id, method, sid, ref, string(raw), c.Now())
	})
	if err != nil {
		return nil, err
	}
	if a, ok := f.Answers[id]; ok {
		return a.Result, a.Err
	}
	ahead, queued, err := c.St.Ahead(ref, id)
	if err != nil {
		return nil, err
	}
	if !queued { // another sender took it meanwhile: its session hears of it
		return nil, &Queued{Item: id, Cause: stopped}
	}
	return nil, &Queued{Item: id, Ahead: ahead, Cause: stopped}
}

// isOffline says whether a flush stopped because the server could not be reached (a queued answer, not an error).
func isOffline(err error) bool {
	return errors.Is(err, api.ErrOffline) || errors.Is(err, sendlock.ErrBusy)
}
