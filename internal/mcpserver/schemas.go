package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

// Input schemas. A tool the Python MCP server had takes its exact input schema (protocol.PythonInputSchemas,
// exported from Python); a new tool's is inferred from its Go input type below, with the protocol's enums. Every
// input is checked against its schema before the tool runs: a malformed call is refused with what is wrong.

// The enums a tool's input may name.
type (
	verdictKind string // APPROVE or REJECT
	taskKind    string
	taskState   string
)

func enum(values ...string) *jsonschema.Schema {
	out := &jsonschema.Schema{Type: "string"}
	for _, v := range values {
		out.Enum = append(out.Enum, v)
	}
	return out
}

func values[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

var enums = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[verdictKind](): enum(protocol.HumanActsVerdictSignalKinds...),
	reflect.TypeFor[taskKind]():    enum(values(protocol.TaskKindValues)...),
	reflect.TypeFor[taskState]():   enum(values(protocol.TaskStateValues)...),
}

// defaults are the new tools' parameter defaults, shown in their schemas.
var defaults = map[string]map[string]any{
	"deliverables_archive": {"archived": true},
	"artifact_archive":     {"archived": true},
}

// additions are parameters a Python tool gains here, added to its exported schema.
var additions = map[string]map[string]any{
	"artifact_get": {"overwrite": map[string]any{"type": "boolean", "default": false, "title": "Overwrite",
		"description": "Replace a file that already exists at save_to"}},
}

// maxLengths are the new tools' text limits, as the server's models have them.
var maxLengths = map[string]map[string]int{
	"drift_resolve":        {"note": 240},
	"drift_dismiss":        {"note": 240},
	"deliverables_archive": {"note": 240},
	"artifact_archive":     {"note": 240},
}

// inputSchema is the tool's input schema.
func inputSchema[In any](name string) (any, error) {
	if raw, ok := protocol.PythonInputSchemas[name]; ok {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("the Python input schema of %s: %w", name, err)
		}
		if add := additions[name]; add != nil {
			props, _ := m["properties"].(map[string]any)
			for k, v := range add {
				props[k] = v
			}
		}
		return m, nil
	}
	s, err := jsonschema.For[In](&jsonschema.ForOptions{TypeSchemas: enums})
	if err != nil {
		return nil, err
	}
	s.Title = name + "Arguments"
	for prop, n := range maxLengths[name] {
		s.Properties[prop].MaxLength = &n
	}
	for prop, v := range defaults[name] {
		raw, _ := json.Marshal(v)
		s.Properties[prop].Default = raw
	}
	return s, nil
}

// adder registers one tool.
type adder func(s *mcp.Server, t *tools, tool *mcp.Tool)

// tool is a tool's registration: its input type In, checked against its schema, then fn.
func tool[In any](fn func(k *call, in In) (any, error)) adder {
	return func(s *mcp.Server, t *tools, tl *mcp.Tool) {
		schema, err := inputSchema[In](tl.Name)
		if err != nil {
			panic(fmt.Sprintf("mcpserver: %s: %v", tl.Name, err))
		}
		tl.InputSchema = schema
		mcp.AddTool(s, tl, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			return t.run(ctx, func(k *call) (any, error) { return fn(k, in) })
		})
	}
}

// ------------------------------------------------------------------ the new tools' inputs

type verdictIn struct {
	Signal  string      `json:"signal" jsonschema:"The sg_ id of the signal that asks the person to approve"`
	Verdict verdictKind `json:"verdict" jsonschema:"APPROVE or REJECT, as the person said"`
	Note    string      `json:"note,omitempty" jsonschema:"The person's note; for a REJECT, why (required)"`
}

type driftIn struct {
	Drift      string   `json:"drift" jsonschema:"The drift's dr_ id"`
	Note       string   `json:"note" jsonschema:"How it was settled, in the person's words"`
	ResolvedBy []string `json:"resolved_by,omitempty" jsonschema:"The sg_ ids of the signals that settled it"`
	Project    *string  `json:"project,omitempty" jsonschema:"Project id (default: the drift's, else this session's)"`
}

type driftDismissIn struct {
	Drift   string  `json:"drift" jsonschema:"The drift's dr_ id"`
	Note    string  `json:"note" jsonschema:"Why it is no real conflict, in the person's words"`
	Project *string `json:"project,omitempty" jsonschema:"Project id (default: the drift's, else this session's)"`
}

type projectIn struct {
	Project *string `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type archiveIn struct {
	Archived *bool   `json:"archived,omitempty" jsonschema:"true to archive, false to restore"`
	Note     string  `json:"note,omitempty" jsonschema:"Why, at most 240 characters"`
	Project  *string `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type setTitleIn struct {
	Member  string  `json:"member" jsonschema:"The member, by name (or mb_ id)"`
	Title   string  `json:"title" jsonschema:"Their title; empty to clear it"`
	Project *string `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type removeMemberIn struct {
	Member  string  `json:"member" jsonschema:"The member, by name (or mb_ id)"`
	Project *string `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type writeStateIn struct {
	BaseVersion int64   `json:"base_version" jsonschema:"The state version you read (project_get's state_version)"`
	Plan        string  `json:"plan" jsonschema:"The whole new plan"`
	Instruction string  `json:"instruction" jsonschema:"The whole new instruction"`
	Project     *string `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type orgTitleIn struct {
	Member string  `json:"member" jsonschema:"The member, by name, email (or mb_ id)"`
	Title  string  `json:"title" jsonschema:"Their title in the org; empty to clear it"`
	Org    *string `json:"org,omitempty" jsonschema:"The org's name (default: your only org)"`
}

type profileIn struct {
	Name  *string `json:"name,omitempty" jsonschema:"The person's name"`
	Alias *string `json:"alias,omitempty" jsonschema:"The person's alias (empty to clear it)"`
}

type taskListIn struct {
	Kind    *taskKind  `json:"kind,omitempty" jsonschema:"Only tasks of this kind"`
	State   *taskState `json:"state,omitempty" jsonschema:"Only tasks in this state"`
	Project *string    `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type topicDeleteIn struct {
	Topic           string  `json:"topic" jsonschema:"Topic name (or tp_ id)"`
	ExpectedVersion int64   `json:"expected_version" jsonschema:"The project version you read (project_get's version)"`
	Project         *string `json:"project,omitempty" jsonschema:"Project id (default: this session's)"`
}

type artifactArchiveIn struct {
	Artifact string `json:"artifact" jsonschema:"The artifact's id"`
	Archived *bool  `json:"archived,omitempty" jsonschema:"true to archive, false to restore"`
	Note     string `json:"note,omitempty" jsonschema:"Why, at most 240 characters"`
}

type orgAddIn struct {
	Email string  `json:"email" jsonschema:"The email of their existing Teamflow account"`
	Title string  `json:"title,omitempty" jsonschema:"Their title in the org"`
	Org   *string `json:"org,omitempty" jsonschema:"The org's name (default: your only org)"`
}

type orgRemoveIn struct {
	Member string  `json:"member" jsonschema:"The member, by name, email (or mb_ id)"`
	Org    *string `json:"org,omitempty" jsonschema:"The org's name (default: your only org)"`
}

type none struct{}
