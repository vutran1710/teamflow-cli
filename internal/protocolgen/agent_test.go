package protocolgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	okTools    = `[{"name": "whoami", "description": "Which member this session acts as."}, {"name": "verdict", "description": "Record the person's verdict \"exactly\"."}]`
	okMCPTools = `[{"name": "whoami", "description": "old", "input_schema": {"properties": {}, "title": "whoamiArguments", "type": "object"}}]`
)

func agentFiles(instructions, teamf, tools, mcpTools string) AgentFiles {
	return AgentFiles{Instructions: []byte(instructions), Teamf: []byte(teamf), Tools: []byte(tools), MCPTools: []byte(mcpTools)}
}

func TestTheAgentTextsGenerate(t *testing.T) {
	src, err := GenerateAgent(agentFiles("# Teamflow\n\nRecord `as you go`.\n", "Run `{TEAMFLOW} team --relay {ARGUMENTS}`\n", okTools, okMCPTools))
	if err != nil {
		t.Fatal(err)
	}
	out := string(src)
	for _, want := range []string{
		"// Code generated",
		`const AgentInstructions = "# Teamflow\n\nRecord ` + "`as you go`" + `.\n"`,
		`const AgentTeamf = "Run ` + "`{TEAMFLOW} team --relay {ARGUMENTS}`" + `\n"`,
		`{Name: "whoami", Description: "Which member this session acts as."}`,
		`{Name: "verdict", Description: "Record the person's verdict \"exactly\"."}`,
		`"whoami": ` + "`" + `{"properties":{},"title":"whoamiArguments","type":"object"}` + "`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agent_gen.go lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, `"whoami"`) > strings.Index(out, `"verdict"`) {
		t.Error("the tools must keep tools.json's order")
	}
}

func TestTheAgentTextsAreRefusedWhenBroken(t *testing.T) {
	cases := map[string]struct {
		files AgentFiles
		want  string
	}{
		"no instructions":       {agentFiles("", "x", okTools, okMCPTools), "instructions.md is empty"},
		"no teamf":              {agentFiles("x", " \n", okTools, okMCPTools), "teamf.md is empty"},
		"tools not JSON":        {agentFiles("x", "x", `[{`, okMCPTools), "tools.json"},
		"tools not a list":      {agentFiles("x", "x", `{"name": "a"}`, okMCPTools), "tools.json"},
		"no tools":              {agentFiles("x", "x", `[]`, okMCPTools), "no tools"},
		"a tool without a name": {agentFiles("x", "x", `[{"name": "", "description": "d"}]`, okMCPTools), "without a name"},
		"a repeated tool":       {agentFiles("x", "x", `[{"name": "a", "description": "d"}, {"name": "a", "description": "e"}]`, okMCPTools), "twice"},
		"an empty description":  {agentFiles("x", "x", `[{"name": "a", "description": " "}]`, okMCPTools), "no description"},
		"a bad tool name":       {agentFiles("x", "x", `[{"name": "a b", "description": "d"}]`, okMCPTools), "not a tool name"},
		"mcp tools not JSON":    {agentFiles("x", "x", okTools, `nope`), "mcp_tools.json"},
		"a schema not an object": {agentFiles("x", "x", okTools, `[{"name": "whoami", "input_schema": {"type": "array"}}]`),
			"not an object schema"},
		"a schema for no tool": {agentFiles("x", "x", okTools, `[{"name": "gone", "input_schema": {"type": "object"}}]`),
			"not in tools.json"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := GenerateAgent(c.files)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}

func TestWriteAllReadsTheExportFolder(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("schema.json", schemaWith(`"Empty": {"type": "object", "properties": {}}`, `"MAX_BATCH": 50`))
	write("methods.json", okMethods)
	write("agent/instructions.md", "i")
	write("agent/teamf.md", "t")
	write("agent/tools.json", okTools)
	write("mcp_tools.json", okMCPTools)
	out := t.TempDir()
	if err := WriteAll(dir, out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"types_gen.go", "constants_gen.go", "agent_gen.go"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "agent", "tools.json")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "tools.json") {
		t.Fatalf("a missing tools.json: %v", err)
	}
}

// Each enum gets the list of its values, in the schema's order, for the places that offer them (tool schemas,
// command flags), so none retypes them.
func TestEnumsListTheirValues(t *testing.T) {
	files, err := Generate([]byte(schemaWith(`"TaskState": {"type": "string", "enum": ["QUEUED", "DONE"]}, "Empty": {"type": "object", "properties": {}}`,
		`"MAX_BATCH": 50`)), []byte(okMethods))
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(strings.Fields(string(files["types_gen.go"])), " ")
	if !strings.Contains(out, "var TaskStateValues = []TaskState{TaskStateQueued, TaskStateDone}") {
		t.Errorf("no value list:\n%s", out)
	}
}
