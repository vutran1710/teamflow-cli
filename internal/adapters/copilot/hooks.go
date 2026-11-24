package copilot

// Copilot CLI's hooks, mapped to Teamflow's:
//
//	sessionStart         the session recorded against its Copilot process; the full context when shared
//	                     (additionalContext); the background sync started again
//	userPromptSubmitted  recorded and synced, nothing claimed (Copilot drops a command hook's output here)
//	preToolUse           a plain teamflow command allowed (permissionDecision allow, no prompt); a tool call
//	                     naming the Teamflow home refused (deny); nothing else
//	postToolUse          what is new since the session was last told (additionalContext), from the replica,
//	                     the network at most once per 10 s
//	agentStop            a new approval request keeps Copilot going once, to ask (decision block)
//	sessionEnd           the spool sent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

// input is what Copilot sends a hook (camelCase events; the snake_case session_id of its VS Code-compatible
// payload is taken too).
type input struct {
	SessionID      *string         `json:"sessionId"`
	SessionIDSnake *string         `json:"session_id"`
	Cwd            string          `json:"cwd"`
	Source         string          `json:"source"`
	ToolName       string          `json:"toolName"`
	ToolArgs       json.RawMessage `json:"toolArgs"`
	StopHookActive bool            `json:"stop_hook_active"`
}

func parse(stdin []byte) (input, string, error) {
	var in input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return input{}, "", fmt.Errorf("hook input is not a JSON object: %w", errors.Unwrap(err))
	}
	for _, id := range []*string{in.SessionID, in.SessionIDSnake} {
		if id != nil && *id != "" {
			return in, *id, nil
		}
	}
	return input{}, "", errors.New("hook input has no sessionId")
}

// SessionID is Copilot's session id in a hook's input.
func (*Adapter) SessionID(stdin []byte) (string, error) {
	_, sid, err := parse(stdin)
	return sid, err
}

// agentPID is the Copilot process a hook runs under: the hook's parent is the copilot process itself. Copilot sets
// COPILOT_CLI, COPILOT_CLI_BINARY_VERSION, COPILOT_PROJECT_DIR and COPILOT_HOME for its hooks, but no pid (checked
// on 1.0.63).
func agentPID() int { return adapters.AgentPID("", "copilot") }

// Hook answers one of Copilot's hook calls (nil: nothing to add).
func (*Adapter) Hook(event string, stdin []byte, c *core.Core) ([]byte, error) {
	switch event {
	case "sessionStart", "userPromptSubmitted", "preToolUse", "postToolUse", "agentStop", "sessionEnd":
	default:
		return nil, nil
	}
	in, sid, err := parse(stdin)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	switch event {
	case "sessionStart":
		adapters.EnsureSync()
		// source: startup, resume, or new (a fresh `copilot -p` session says "new": checked on 1.0.63).
		text, err := c.Start(ctx, sid, Name, agentPID(), in.Cwd, in.Source)
		return additional(text), err
	case "userPromptSubmitted":
		return nil, c.BeforePrompt(ctx, sid, Name, agentPID(), in.Cwd)
	case "preToolUse":
		exe, _ := os.Executable()
		if adapters.NamesHome(in.ToolName+" "+string(in.ToolArgs), exe) {
			return ojson.Marshal(ojson.Obj{{Key: "permissionDecision", Value: "deny"},
				{Key: "permissionDecisionReason", Value: adapters.HomeRefusal}})
		}
		if cmd := shellCommand(in.ToolArgs); cmd != "" && adapters.IsTeamflowCommand(cmd, exe) {
			return ojson.Marshal(ojson.Obj{{Key: "permissionDecision", Value: "allow"}})
		}
		return nil, nil
	case "postToolUse":
		text, err := c.AfterTool(ctx, sid)
		return additional(text), err
	case "agentStop":
		if in.StopHookActive {
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

// additional is Copilot's answer adding text to the model's context (nil for no text).
func additional(text string) []byte {
	if text == "" {
		return nil
	}
	b, _ := ojson.Marshal(ojson.Obj{{Key: "additionalContext", Value: text}})
	return b
}

// shellCommand is the command of a shell tool call's arguments ({"command": …}, or that object as a JSON string);
// "" for any other tool.
func shellCommand(args json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &v) == nil && v.Command != "" {
		return v.Command
	}
	var s string
	if json.Unmarshal(args, &s) == nil && json.Unmarshal([]byte(s), &v) == nil {
		return v.Command
	}
	return ""
}
