package claudecode

// Claude Code's hooks, mapped to Teamflow's: SessionStart (the session recorded against its Claude Code process;
// the full context when shared; the background sync started again), UserPromptSubmit (what is new: notices,
// approval requests, signals), PreToolUse (a new approval request mid-turn, added to the tool call's context once,
// from the replica; no guard), Stop (a new approval request at the turn's end keeps Claude going once, to ask), SessionEnd (the spool
// sent).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

// EnvPID is the agent marker Claude Code sets for the processes it starts (hooks, its shell, MCP servers): its own
// process id. CLAUDECODE=1 is set too (checked on Claude Code 2.1.331).
const EnvPID = "CLAUDE_PID"

// input is what Claude Code sends a hook (the fields Teamflow reads).
type input struct {
	SessionID      *string `json:"session_id"`
	Cwd            string  `json:"cwd"`
	Source         string  `json:"source"`
	Prompt         string  `json:"prompt"`
	StopHookActive bool    `json:"stop_hook_active"`
}

func parse(stdin []byte) (input, error) {
	var in input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return input{}, fmt.Errorf("hook input is not a JSON object: %w", errors.Unwrap(err))
	}
	if in.SessionID == nil || *in.SessionID == "" {
		return input{}, errors.New("hook input has no session_id")
	}
	return in, nil
}

// SessionID is Claude Code's session id in a hook's input.
func (*Adapter) SessionID(stdin []byte) (string, error) {
	in, err := parse(stdin)
	if err != nil {
		return "", err
	}
	return *in.SessionID, nil
}

// Hook answers one of Claude Code's hook calls (nil: nothing to add).
func (*Adapter) Hook(event string, stdin []byte, c *core.Core) ([]byte, error) {
	switch event {
	case "SessionStart", "UserPromptSubmit", "PreToolUse", "Stop", "SessionEnd":
	default:
		return nil, nil
	}
	in, err := parse(stdin)
	if err != nil {
		return nil, err
	}
	ctx, sid := context.Background(), *in.SessionID
	switch event {
	case "SessionStart":
		adapters.EnsureSync()
		text, err := c.Start(ctx, sid, Name, adapters.AgentPID(EnvPID, "claude"), in.Cwd, in.Source)
		return addContext(event, text), err
	case "UserPromptSubmit":
		text, err := c.Prompt(ctx, sid, Name, adapters.AgentPID(EnvPID, "claude"), in.Cwd, in.Prompt)
		return addContext(event, text), err
	case "PreToolUse": // every step of the turn: from the replica, the network at most once per 10 s
		return addContext(event, c.MidTurn(ctx, sid)), nil
	case "Stop":
		if in.StopHookActive { // a turn the Stop hook continued is never blocked again
			return nil, nil
		}
		reason := c.AfterTurn(ctx, sid)
		if reason == "" {
			return nil, nil
		}
		return ojson.Marshal(ojson.Obj{{Key: "decision", Value: "block"}, {Key: "reason", Value: reason}})
	}
	return nil, c.End(ctx, sid)
}

// addContext is Claude Code's answer adding text to the model's context (nil for no text).
func addContext(event, text string) []byte {
	if text == "" {
		return nil
	}
	b, _ := ojson.Marshal(ojson.Obj{{Key: "hookSpecificOutput", Value: ojson.Obj{
		{Key: "hookEventName", Value: event}, {Key: "additionalContext", Value: text}}}})
	return b
}
