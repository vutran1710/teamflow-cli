// Package mcpserver is Teamflow's MCP server (`teamflow mcp`, over stdio): the same tools for every agent, with the
// one agent-neutral instruction text as the server's instructions. A port of mcp/tf_mcp/server.py.
//
// Each call opens the client afresh (config, keys and store read from disk, so a sign-in made elsewhere is seen at
// once) and finds the agent session it runs under through a SessionFinder, so it follows a cleared session and
// never mixes up two sessions on one machine. The server fills in ids, times, the project version and the actor:
// AGENT for what the agent records, MEMBER for the person's verdicts (always the person's own). Topics and people
// are written by name.
//
// Every tool result carries what is new for the session: approval requests waiting for the person
// ("approval_requests", as the Python tools did) and the session's other notices not yet shown ("notices"), so an
// agent with no hooks is still told at each tool call.
//
// Human acts (verdict, drift_resolve, drift_dismiss) are sent alone, never in a batch.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// Name is the server's name, as agents list it.
const Name = "teamflow"

// SessionFinder finds the agent session a tool call runs under (the adapters provide one per agent; the default
// walks the process tree). ok is false when there is none.
type SessionFinder interface {
	Current(st *store.Store) (sess store.Session, ok bool, err error)
}

// Opener opens the client for one call: the Teamflow home, its config and its store.
type Opener func() (*core.Core, error)

// ErrNoSession is a tool that needs the agent session, called where none is found.
var ErrNoSession = errors.New("no agent session found; is Teamflow installed for this agent?")

// Run serves the tools over in and out (the agent's pipes: stdin and stdout) until the agent closes them.
func Run(ctx context.Context, open Opener, find SessionFinder, version string, in io.Reader, out io.Writer) error {
	err := New(open, find, version).Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
	if err == nil || errors.Is(err, io.EOF) {
		return nil // the agent closed the pipe: done
	}
	return err
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// New is the server with every tool of protocol.AgentTools.
func New(open Opener, find SessionFinder, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: Name, Version: version},
		&mcp.ServerOptions{Instructions: protocol.AgentInstructions})
	t := &tools{open: open, find: find}
	for _, at := range protocol.AgentTools {
		reg, ok := registry[at.Name]
		if !ok {
			panic("mcpserver: no handler for the tool " + at.Name)
		}
		reg(s, t, &mcp.Tool{Name: at.Name, Description: at.Description})
	}
	return s
}

// call is one tool call: the client opened for it, and the session found (when there is one).
type call struct {
	ctx  context.Context
	c    *core.Core
	find SessionFinder
	sess *store.Session
	err  error // why no session was found
}

type tools struct {
	open Opener
	find SessionFinder
}

// session is the agent session this call runs under.
func (k *call) session() (store.Session, error) {
	if k.sess == nil {
		if k.err != nil {
			return store.Session{}, k.err
		}
		return store.Session{}, ErrNoSession
	}
	return *k.sess, nil
}

// project is the project named, or the session's.
func (k *call) project(project *string) (string, error) {
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
func (k *call) shared() (store.Session, error) {
	sess, err := k.session()
	if err != nil {
		return sess, err
	}
	if sess.Project == "" {
		return sess, core.ErrSessionNotShared
	}
	return sess, nil
}

// run runs a tool: it opens the client, finds the session, calls fn, and adds what is new for the session. A
// failure is a tool error the agent reads.
func (t *tools) run(ctx context.Context, fn func(k *call) (any, error)) (*mcp.CallToolResult, any, error) {
	c, err := t.open()
	if err != nil {
		return nil, nil, toolError(err)
	}
	defer c.Close()
	k := &call{ctx: ctx, c: c, find: t.find}
	if t.find != nil {
		sess, ok, err := t.find.Current(c.St)
		switch {
		case err != nil:
			k.err = fmt.Errorf("could not look up the agent session: %w", err)
		case ok:
			k.sess = &sess
		}
	}
	out, err := fn(k)
	if err != nil { // a failure carries what is new too: every result does
		text := toolError(err).Error()
		if news := k.news(); len(news) > 0 {
			if extra, jerr := ojson.Indent(news); jerr == nil {
				text += "\n\n" + extra
			}
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}
	if news := k.news(); len(news) > 0 {
		o, isObj := out.(ojson.Obj)
		if !isObj {
			o = ojson.Obj{{"result", out}}
		} else {
			o = append(ojson.Obj{}, o...)
		}
		for _, kv := range news {
			o = o.Set(kv.Key, kv.Value)
		}
		out = o
	}
	content, err := contents(out)
	if err != nil {
		return nil, nil, toolError(err)
	}
	return &mcp.CallToolResult{Content: content}, nil, nil
}

// news is what is new for the session, claimed so no other call gets it too: approval requests that reached it
// (so the agent asks the person mid-turn) and its other unshown notices. Checking never breaks the call itself.
func (k *call) news() ojson.Obj {
	if k.sess == nil {
		return nil
	}
	var o ojson.Obj
	if k.sess.Project != "" {
		if news := k.c.AfterTurn(k.ctx, k.sess.ID); news != "" {
			o = append(o, ojson.KV{Key: "approval_requests", Value: news})
		}
	}
	if notices := k.c.NewNotices(k.sess.ID); len(notices) > 0 {
		o = append(o, ojson.KV{Key: "notices", Value: notices})
	}
	return o
}

// contents is a result as the Python MCP server gave it: a string as it is, a list one item per block (an empty
// list: none), anything else as JSON indented by two spaces.
func contents(out any) ([]mcp.Content, error) {
	switch v := out.(type) {
	case nil:
		return []mcp.Content{}, nil
	case string:
		return []mcp.Content{&mcp.TextContent{Text: v}}, nil
	case []string:
		blocks := []mcp.Content{}
		for _, s := range v {
			blocks = append(blocks, &mcp.TextContent{Text: s})
		}
		return blocks, nil
	case []ojson.Obj:
		blocks := []mcp.Content{}
		for _, o := range v {
			text, err := ojson.Indent(o)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, &mcp.TextContent{Text: text})
		}
		return blocks, nil
	}
	text, err := ojson.Indent(out)
	if err != nil {
		return nil, err
	}
	return []mcp.Content{&mcp.TextContent{Text: text}}, nil
}

// toolError is a failure as the agent reads it: the refusal's own words.
func toolError(err error) error {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return errors.New(ae.Error())
	}
	return errors.New(strings.TrimSpace(err.Error()))
}
