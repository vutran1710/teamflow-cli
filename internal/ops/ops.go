// Package ops is every Teamflow action an agent (or the person) takes through a `teamflow` command, as a function
// of its input that answers with an ordered JSON object: the former MCP tools' logic, kept field for field, now run
// by the CLI (`--json` prints the answer as it is; plain output is drawn from it).
//
// Each run gets the client opened for it (config, keys and store read from disk, so a sign-in made elsewhere is
// seen at once) and the agent session it runs under, when one is found. The server fills in ids, times, the
// project version and the actor: AGENT for what the agent records inside an agent session, MEMBER for the person's
// verdicts (always the person's own) and for anything run outside an agent session. Topics and people are written
// by name.
//
// Human acts (verdicts, drift resolve and dismiss) are sent alone, never in a batch.
package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// ErrNoSession is an action that needs the agent session, run where none is found.
var ErrNoSession = errors.New("no agent session found above this process; run this from inside an agent session " +
	"(is Teamflow installed for this agent?)")

// Call is one run: the client opened for it, and the agent session found (when there is one).
type Call struct {
	ctx  context.Context
	c    *core.Core
	sess *store.Session
	err  error // why no session was found
}

// New is a run on c, under sess (nil: none found; why, when the lookup failed).
func New(ctx context.Context, c *core.Core, sess *store.Session, why error) *Call {
	return &Call{ctx: ctx, c: c, sess: sess, err: why}
}

// Names are the actions, by name.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}

// Do runs the action name with its input (any value that encodes to the action's JSON input object). Input it does
// not know is refused, as is an action that does not exist.
func (k *Call) Do(name string, input any) (any, error) {
	fn, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("no action %s", name)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if input == nil || string(raw) == "null" {
		raw = []byte("{}")
	}
	return fn(k, raw)
}

// op is an action taking its input as In: decoded strictly (an unknown field is refused, with its name).
func op[In any](fn func(k *Call, in In) (any, error)) func(k *Call, raw json.RawMessage) (any, error) {
	return func(k *Call, raw json.RawMessage) (any, error) {
		var in In
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, bad("invalid input: %s", strings.TrimPrefix(err.Error(), "json: "))
		}
		return fn(k, in)
	}
}

// session is the agent session this run is under.
func (k *Call) session() (store.Session, error) {
	if k.sess == nil {
		if k.err != nil {
			return store.Session{}, k.err
		}
		return store.Session{}, ErrNoSession
	}
	return *k.sess, nil
}

// project is the project named, or the session's.
func (k *Call) project(project *string) (string, error) {
	if project != nil && *project != "" {
		return *project, nil
	}
	sess, err := k.session()
	if err != nil {
		return "", err
	}
	if sess.Project == "" {
		return "", core.ErrSessionNotShared
	}
	return sess.Project, nil
}

// shared is the session, shared with a project.
func (k *Call) shared() (store.Session, error) {
	sess, err := k.session()
	if err != nil {
		return sess, err
	}
	if sess.Project == "" {
		return sess, core.ErrSessionNotShared
	}
	return sess, nil
}

// News is what is new for a session since it last heard: approval requests waiting for the person (the approval
// queue's text) and its other notices. Both are claimed in one transaction, so two commands running at once never
// both carry one.
type News struct {
	ApprovalRequests string
	Notices          []string
}

// Empty says there is nothing new.
func (n News) Empty() bool { return n.ApprovalRequests == "" && len(n.Notices) == 0 }

// Obj is the news as fields of a JSON answer: approval_requests and notices, each only when there is some.
func (n News) Obj() ojson.Obj {
	var o ojson.Obj
	if n.ApprovalRequests != "" {
		o = append(o, ojson.KV{Key: "approval_requests", Value: n.ApprovalRequests})
	}
	if len(n.Notices) > 0 {
		o = append(o, ojson.KV{Key: "notices", Value: n.Notices})
	}
	return o
}

// NewsFor is the session's news, claimed. Checking never fails the command itself.
func NewsFor(ctx context.Context, c *core.Core, sess *store.Session) News {
	if sess == nil {
		return News{}
	}
	approvals, notices := c.News(ctx, sess.ID)
	return News{ApprovalRequests: approvals, Notices: notices}
}

// With is an answer with the news added: an object gains the fields; anything else is put under "result" first.
func With(out any, news News) any {
	if news.Empty() {
		return out
	}
	o, isObj := out.(ojson.Obj)
	if isObj {
		o = append(ojson.Obj{}, o...)
	} else {
		o = ojson.Obj{{Key: "result", Value: out}}
	}
	for _, kv := range news.Obj() {
		o = o.Set(kv.Key, kv.Value)
	}
	return o
}

// Blocks is an answer as text blocks, as the Python MCP tools gave it: a string as it is, a list one item per
// block (an empty list: none), anything else as JSON indented by two spaces.
func Blocks(out any) ([]string, error) {
	switch v := out.(type) {
	case nil:
		return []string{}, nil
	case string:
		return []string{v}, nil
	case []string:
		return append([]string{}, v...), nil
	case []ojson.Obj:
		blocks := []string{}
		for _, o := range v {
			text, err := ojson.Indent(o)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, text)
		}
		return blocks, nil
	}
	text, err := ojson.Indent(out)
	if err != nil {
		return nil, err
	}
	return []string{text}, nil
}

// ErrText is a failure as it reads: the refusal's own words.
func ErrText(err error) string {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return strings.TrimSpace(err.Error())
}

// actor is who records what this run sends (never a verdict: those are always the person's, MEMBER): the agent
// inside an agent session, else the person.
func (k *Call) actor() protocol.Actor {
	if k.sess != nil {
		return protocol.ActorAgent
	}
	return protocol.ActorMember
}
