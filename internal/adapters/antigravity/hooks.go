package antigravity

// Antigravity's hooks, mapped to Teamflow's:
//
//	SessionStart   the session recorded against its agy process; the full context when shared (injectSteps); the
//	               background sync started again
//	PreInvocation  before each model call: at the first of a turn (invocationNum 0) what is new since the last
//	               prompt, synced; at later ones (after tools) what is new, from the replica, the network at most
//	               once per 10 s (injectSteps)
//	Stop           a new approval request keeps the agent going once, to ask (decision continue)
//
// There is no session-end hook: what waits in the spool is sent by the background sync or the next hook. The
// session id is conversationId. All from https://antigravity.google/docs/hooks.md: verify on a real install (in
// particular what invocationNum counts, and that a hook may print nothing to leave the default).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

type input struct {
	ConversationID *string  `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
	LastUserInput  string   `json:"lastUserInput"`
	InvocationNum  *int     `json:"invocationNum"`
}

func parse(stdin []byte) (input, string, error) {
	var in input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return input{}, "", fmt.Errorf("hook input is not a JSON object: %w", errors.Unwrap(err))
	}
	if in.ConversationID == nil || *in.ConversationID == "" {
		return input{}, "", errors.New("hook input has no conversationId")
	}
	return in, *in.ConversationID, nil
}

// SessionID is Antigravity's conversation id in a hook's input.
func (*Adapter) SessionID(stdin []byte) (string, error) {
	_, sid, err := parse(stdin)
	return sid, err
}

func (in input) cwd() string {
	if len(in.WorkspacePaths) > 0 {
		return in.WorkspacePaths[0]
	}
	return ""
}

// agentPID is the agy process a hook runs under (no pid marker is documented: verify on a real install).
func agentPID() int { return adapters.AgentPID("", "agy") }

// Hook answers one of Antigravity's hook calls (nil: nothing to add).
func (*Adapter) Hook(event string, stdin []byte, c *core.Core) ([]byte, error) {
	switch event {
	case "SessionStart", "PreInvocation", "Stop":
	default:
		return nil, nil
	}
	in, sid, err := parse(stdin)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	switch event {
	case "SessionStart":
		adapters.EnsureSync()
		text, err := c.Start(ctx, sid, Name, agentPID(), in.cwd(), "startup")
		return inject(text), err
	case "PreInvocation":
		if in.InvocationNum == nil || *in.InvocationNum == 0 { // the turn's first model call
			adapters.EnsureSync() // not running (a reboot, a restart, its quiet exit): started again
			text, err := c.Prompt(ctx, sid, Name, agentPID(), in.cwd(), in.LastUserInput)
			return inject(text), err
		}
		text, err := c.AfterTool(ctx, sid)
		return inject(text), err
	}
	reason := c.AfterTurn(ctx, sid)
	if reason == "" {
		return nil, nil
	}
	return ojson.Marshal(ojson.Obj{{Key: "decision", Value: "continue"}, {Key: "reason", Value: reason}})
}

// inject is Antigravity's answer adding text to the conversation as a system message (nil for no text).
func inject(text string) []byte {
	if text == "" {
		return nil
	}
	b, _ := ojson.Marshal(ojson.Obj{{Key: "injectSteps", Value: []any{
		ojson.Obj{{Key: "systemMessage", Value: ojson.Obj{{Key: "systemMessage", Value: text}}}},
	}}})
	return b
}
