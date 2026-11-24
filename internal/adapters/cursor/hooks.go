package cursor

// Cursor's hooks, mapped to Teamflow's:
//
//	sessionStart          the session recorded against its Cursor process; the instruction text and, when shared,
//	                      the full context (additional_context); the background sync started again
//	beforeSubmitPrompt    recorded and synced, nothing claimed (this hook can only block a prompt, not add to it)
//	beforeShellExecution  answered without opening the Teamflow home (Quick): a command naming the home refused
//	                      (deny); a plain teamflow work command allowed; any other teamflow command (setup,
//	                      chained, expanded) "ask", whatever the Shell(teamflow) allow rule would match
//	postToolUse           what is new since the session was last told (additional_context), from the replica, the
//	                      network at most once per 10 s
//	stop                  a new approval request keeps the agent going once, to ask (followup_message)
//	sessionEnd            the spool sent
//
// The session id is conversation_id (in every event's input; session_id only at start and end). All from
// https://cursor.com/docs/agent/hooks: verify on a real install.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/core"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

type input struct {
	ConversationID *string  `json:"conversation_id"`
	SessionID      *string  `json:"session_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Command        string   `json:"command"`
	Cwd            string   `json:"cwd"`
	LoopCount      int      `json:"loop_count"`
}

func parse(stdin []byte) (input, string, error) {
	var in input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return input{}, "", fmt.Errorf("hook input is not a JSON object: %w", errors.Unwrap(err))
	}
	for _, id := range []*string{in.ConversationID, in.SessionID} {
		if id != nil && *id != "" {
			return in, *id, nil
		}
	}
	return input{}, "", errors.New("hook input has no conversation_id")
}

// SessionID is Cursor's conversation id in a hook's input.
func (*Adapter) SessionID(stdin []byte) (string, error) {
	_, sid, err := parse(stdin)
	return sid, err
}

// cwd is the session's folder: its first workspace root, else CURSOR_PROJECT_DIR (set for hooks).
func (in input) cwd() string {
	if len(in.WorkspaceRoots) > 0 {
		return in.WorkspaceRoots[0]
	}
	return os.Getenv("CURSOR_PROJECT_DIR")
}

// agentPID is the Cursor CLI process a hook runs under (Cursor sets no pid marker: verify on a real install).
func agentPID() int { return adapters.AgentPID("", "cursor-agent", "agent") }

// Hook answers one of Cursor's hook calls (nil: nothing to add).
func (*Adapter) Hook(event string, stdin []byte, c *core.Core) ([]byte, error) {
	switch event {
	case "sessionStart", "beforeSubmitPrompt", "beforeShellExecution", "postToolUse", "stop", "sessionEnd":
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
		text, err := c.Start(ctx, sid, Name, agentPID(), in.cwd(), "startup")
		if err != nil {
			return nil, err
		}
		// No global instruction file is documented for the CLI, so the one text is given here too.
		if text != "" {
			text = "\n\n" + text
		}
		return context_(protocol.AgentInstructions + text), nil
	case "beforeSubmitPrompt":
		adapters.EnsureSync() // not running (a reboot, a restart, its quiet exit): started again
		if err := c.BeforePrompt(ctx, sid, Name, agentPID(), in.cwd()); err != nil {
			return nil, err
		}
		return ojson.Marshal(ojson.Obj{{Key: "continue", Value: true}})
	case "beforeShellExecution": // answered by Quick, before the home is opened
		return nil, nil
	case "postToolUse":
		text, err := c.AfterTool(ctx, sid)
		return context_(text), err
	case "stop":
		if in.LoopCount > 0 { // a turn the stop hook continued is never continued again
			return nil, nil
		}
		reason := c.AfterTurn(ctx, sid)
		if reason == "" {
			return nil, nil
		}
		return ojson.Marshal(ojson.Obj{{Key: "followup_message", Value: reason}})
	}
	return nil, c.End(ctx, sid)
}

// context_ is Cursor's answer adding text to the agent's context (nil for no text).
func context_(text string) []byte {
	if text == "" {
		return nil
	}
	b, _ := ojson.Marshal(ojson.Obj{{Key: "additional_context", Value: text}})
	return b
}

// Quick answers beforeShellExecution without the Teamflow home, so a broken or refused home never drops the deny,
// the allow or the ask.
func (*Adapter) Quick(event string, stdin []byte) ([]byte, bool) {
	if event != "beforeShellExecution" {
		return nil, false
	}
	var in input
	if json.Unmarshal(stdin, &in) != nil {
		return nil, true
	}
	exe, _ := os.Executable()
	if adapters.NamesHome(in.Command, exe) {
		out, _ := ojson.Marshal(ojson.Obj{{Key: "permission", Value: "deny"},
			{Key: "user_message", Value: "Teamflow refused a command naming its home folder."},
			{Key: "agent_message", Value: adapters.HomeRefusal}})
		return out, true
	}
	switch tf, auto := adapters.TeamflowCommand(in.Command, exe); {
	case auto:
		out, _ := ojson.Marshal(ojson.Obj{{Key: "permission", Value: "allow"}})
		return out, true
	case tf:
		out, _ := ojson.Marshal(ojson.Obj{{Key: "permission", Value: "ask"},
			{Key: "user_message", Value: "Teamflow: this teamflow command needs your approval (setup, or more than one plain command)."}})
		return out, true
	}
	return nil, true
}
