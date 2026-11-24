package protocolgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const okCommands = `{"about": "a", "every_command": {"json": "j"}, "commands": [
 {"name": "whoami", "usage": "teamflow whoami", "description": "Which member this session acts as.", "consequential": false, "input": null, "help": null, "new": true},
 {"name": "signal emit", "usage": "teamflow signal emit --file <path>", "description": "Record \"exactly\".", "consequential": false, "input": "json", "help": "--file <path>: a list", "new": true},
 {"name": "approve", "usage": "teamflow approve <sg_id> [<note>]", "description": "Only on the person's explicit words.", "consequential": true, "input": null, "help": null, "new": false}]}`

func agentFiles(instructions, teamf, commands string) AgentFiles {
	return AgentFiles{Instructions: []byte(instructions), Teamf: []byte(teamf), Commands: []byte(commands)}
}

func TestTheAgentTextsGenerate(t *testing.T) {
	src, err := GenerateAgent(agentFiles("# Teamflow\n\nRecord `as you go`.\n", "Run `{TEAMFLOW} team --relay {ARGUMENTS}`\n", okCommands))
	if err != nil {
		t.Fatal(err)
	}
	out := string(src)
	for _, want := range []string{
		"// Code generated",
		`const AgentInstructions = "# Teamflow\n\nRecord ` + "`as you go`" + `.\n"`,
		`const AgentTeamf = "Run ` + "`{TEAMFLOW} team --relay {ARGUMENTS}`" + `\n"`,
		`{Name: "whoami", Usage: "teamflow whoami", Description: "Which member this session acts as.", Consequential: false, Input: "", Help: "", New: true}`,
		`{Name: "signal emit", Usage: "teamflow signal emit --file <path>", Description: "Record \"exactly\".", Consequential: false, Input: "json", Help: "--file <path>: a list", New: true}`,
		`Consequential: true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agent_gen.go lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, `"whoami"`) > strings.Index(out, `"signal emit"`) {
		t.Error("the commands must keep commands.json's order")
	}
	if strings.Contains(out, "AgentTools") || strings.Contains(out, "PythonInputSchemas") || strings.Contains(out, "mcp") {
		t.Error("no MCP tool list is generated any more")
	}
}

func TestTheAgentTextsAreRefusedWhenBroken(t *testing.T) {
	one := func(c string) string { return `{"commands": [` + c + `]}` }
	cases := map[string]struct {
		files AgentFiles
		want  string
	}{
		"no instructions":         {agentFiles("", "x", okCommands), "instructions.md is empty"},
		"no teamf":                {agentFiles("x", " \n", okCommands), "teamf.md is empty"},
		"commands not JSON":       {agentFiles("x", "x", `{"commands": [`), "commands.json"},
		"commands a list":         {agentFiles("x", "x", `[{"name": "a"}]`), "commands.json"},
		"no commands":             {agentFiles("x", "x", `{"commands": []}`), "no commands"},
		"a command without name":  {agentFiles("x", "x", one(`{"name": "", "usage": "teamflow ", "description": "d"}`)), "without a name"},
		"a bad name":              {agentFiles("x", "x", one(`{"name": "A b", "usage": "teamflow A b", "description": "d"}`)), "not a command name"},
		"a repeated command":      {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow a", "description": "d"}, {"name": "a", "usage": "teamflow a", "description": "d"}`)), "twice"},
		"usage not its own":       {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow b", "description": "d"}`)), "usage must start"},
		"usage a longer name":     {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow ab", "description": "d"}`)), "usage must start"},
		"an empty description":    {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow a", "description": " "}`)), "no description"},
		"an unknown input":        {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow a", "description": "d", "input": "yaml"}`)), "input"},
		"json input without help": {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow a --file <path>", "description": "d", "input": "json"}`)), "help"},
		"help on a plain command": {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow a", "description": "d", "help": "h"}`)), "help"},
		"an unknown key":          {agentFiles("x", "x", one(`{"name": "a", "usage": "teamflow a", "description": "d", "tool": "x"}`)), "unknown field"},
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
	write("agent/commands.json", okCommands)
	out := t.TempDir()
	if err := WriteAll(dir, out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"types_gen.go", "constants_gen.go", "agent_gen.go"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	// An old export's tools.json and mcp_tools.json are not read: their absence changes nothing.
	if err := os.Remove(filepath.Join(dir, "agent", "commands.json")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "commands.json") {
		t.Fatalf("a missing commands.json: %v", err)
	}
}

// Each enum gets the list of its values, in the schema's order, for the places that offer them (command flags),
// so none retypes them.
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
