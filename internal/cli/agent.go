package cli

// The commands an agent runs (protocol/agent/commands.json): each one's usage line and description come from there,
// word for word; every one takes --json (its answer as one JSON object); and every one's output ends with what is new
// for the agent session it runs under (approval requests and notices, claimed so no other command carries them): a
// delimited trailer on stdout, or the approval_requests and notices fields of the JSON answer. A failure carries
// them too.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/artifacts"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
	"github.com/vutran1710/teamflow-cli/internal/ops"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

// answer is a JSON answer that exits with code (a refusal the answer itself describes: a draft with problems, a
// signal refused).
type answer struct {
	out  any
	code int
}

// jsonFn is a command's JSON answer.
type jsonFn func(r *inv, a *parsed) (any, error)

// bindAgentCommands gives each agent command its line from commands.json (usage, description, input help). A
// command named there and missing here is caught by the commands.json test.
func bindAgentCommands() {
	for i := range protocol.AgentCommands {
		spec := &protocol.AgentCommands[i]
		if c := findCommand(spec.Name); c != nil {
			c.spec = spec
		}
	}
}

// findCommand is the command at a path of words ("deliverables publish"), or nil.
func findCommand(path string) *command {
	cs, c := commands, (*command)(nil)
	for _, n := range strings.Fields(path) {
		if c = find(cs, n); c == nil {
			return nil
		}
		cs = c.sub
	}
	return c
}

// teamSpec is the commands.json line of `team <sub>` (team project, team approvals), when it has one.
func teamSpec(sub string) *protocol.AgentCommand {
	for i := range protocol.AgentCommands {
		if protocol.AgentCommands[i].Name == "team "+sub {
			return &protocol.AgentCommands[i]
		}
	}
	return nil
}

// specUsage prints an agent command's help: its usage line and description from commands.json, the exact shape of
// its JSON input when it takes one, then its options.
func specUsage(w io.Writer, spec *protocol.AgentCommand, c *command) {
	fmt.Fprintln(w, "usage: "+spec.Usage)
	fmt.Fprintln(w, "\n"+spec.Description)
	if spec.Help != "" {
		fmt.Fprintln(w, "\n"+spec.Help)
	}
	if c != nil && len(c.sub) > 0 {
		fmt.Fprintln(w, "\nCommands:")
		for _, s := range c.sub {
			fmt.Fprintf(w, "  %-18s %s\n", s.name, s.help)
		}
	}
	fs := []flagSpec{jsonFlag}
	if c != nil {
		fs = append(append([]flagSpec(nil), c.flags...), jsonFlag)
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].name < fs[j].name })
	fmt.Fprintln(w, "\nOptions:")
	for _, f := range fs {
		name := "--" + f.name
		if f.short != "" {
			name = "-" + f.short + ", " + name
		}
		if f.kind != flag {
			name += " TEXT"
		}
		fmt.Fprintf(w, "  %-24s %s\n", name, f.help)
	}
	fmt.Fprintln(w, "\nEvery command's output ends with what is new for this session: approval requests and notices.")
}

// ------------------------------------------------------------------ running one

// runAgent runs an agent command: its JSON answer with --json, else its plain output; then what is new for the
// session, after a failure too.
func runAgent(r *inv, c *command, a *parsed) int {
	if a.has("json") {
		r.json = true
		out, err := c.jsonRun(r, a)
		news := r.news()
		if err != nil {
			code := 1
			var ue usageError
			if errors.As(err, &ue) {
				code = 2
			}
			r.printJSON(ops.With(ojson.Obj{{Key: "error", Value: r.errorText(c, err)}}, news))
			return code
		}
		code := 0
		if ans, ok := out.(answer); ok {
			out, code = ans.out, ans.code
		}
		r.printJSON(envelope(out, news))
		return code
	}
	var code int
	if c.run != nil {
		code = c.run(r, a)
	} else {
		code = r.plain(c, a)
	}
	r.trailer(r.news())
	return code
}

// plain is a command's plain output drawn from its JSON answer (for the commands with no output of their own).
func (r *inv) plain(c *command, a *parsed) int {
	out, err := c.jsonRun(r, a)
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			return usageFail(r, r.path, ue.msg)
		}
		return r.fail(r.errorText(c, err))
	}
	code := 0
	if ans, ok := out.(answer); ok {
		out, code = ans.out, ans.code
	}
	if text := render(out); text != "" {
		r.println(text)
	}
	return code
}

// errorText is a failure as the command says it: "which project?" where --project would do.
func (r *inv) errorText(c *command, err error) string {
	if errors.Is(err, ops.ErrNoSession) && c != nil && c.hasFlag("project") {
		return whichProject
	}
	return ops.ErrText(err)
}

func (c *command) hasFlag(name string) bool {
	for _, f := range c.flags {
		if f.name == name {
			return true
		}
	}
	return false
}

// envelope is an answer as one JSON object with the news: an object gains the fields; anything else is put under
// "result".
func envelope(out any, news ops.News) ojson.Obj {
	o, isObj := out.(ojson.Obj)
	if !isObj {
		o = ojson.Obj{{Key: "result", Value: out}}
	}
	w, _ := ops.With(o, news).(ojson.Obj)
	return w
}

func (r *inv) printJSON(v any) {
	text, err := ojson.Indent(v)
	if err != nil {
		text = `{"error": "the answer could not be written as JSON"}`
	}
	fmt.Fprintln(r.out, text)
}

// news is what is new for the agent session this runs under, claimed: nothing outside one.
func (r *inv) news() ops.News {
	c, err := r.core()
	if err != nil {
		return ops.News{}
	}
	sess, ok := findSession(c.St)
	if !ok { // the person's own command: what became of acts they queued outside any session
		return ops.News{Notices: c.NewNotices("")}
	}
	return ops.NewsFor(r.ctx, c, &sess)
}

// The trailer's delimiters: an agent reading the output finds the news between them.
const (
	trailerBegin = "--- Teamflow: new for this session ---"
	trailerEnd   = "--- end of Teamflow news ---"
)

// trailer prints the news after a command's plain output (nothing when there is none).
func (r *inv) trailer(n ops.News) {
	if n.Empty() {
		return
	}
	var b strings.Builder
	b.WriteString("\n" + trailerBegin + "\n")
	if n.ApprovalRequests != "" {
		b.WriteString(n.ApprovalRequests + "\n")
	}
	if len(n.Notices) > 0 {
		b.WriteString("Notices:\n")
		for _, t := range n.Notices {
			b.WriteString("- " + strings.ReplaceAll(t, "\n", "\n  ") + "\n")
		}
	}
	b.WriteString(trailerEnd + "\n")
	fmt.Fprint(r.out, b.String())
}

// ------------------------------------------------------------------ actions

// call is an action run for this command: the client, and the agent session it runs under (when there is one).
func (r *inv) call() (*ops.Call, error) {
	c, err := r.core()
	if err != nil {
		return nil, err
	}
	var sess *store.Session
	if s, ok := findSession(c.St); ok {
		sess = &s
	}
	return ops.New(r.ctx, c, sess, nil), nil
}

// do runs an action with its input.
func (r *inv) do(name string, input any) (any, error) {
	k, err := r.call()
	if err != nil {
		return nil, err
	}
	return k.Do(name, input)
}

// act is a command whose answer is one action's, with its input drawn from the arguments.
func act(name string, input func(r *inv, a *parsed) (map[string]any, error)) jsonFn {
	return func(r *inv, a *parsed) (any, error) {
		in := map[string]any{}
		if input != nil {
			var err error
			if in, err = input(r, a); err != nil {
				return nil, err
			}
		}
		return r.do(name, in)
	}
}

// withProject adds --project to an action's input, when given.
func withProject(a *parsed, in map[string]any) map[string]any {
	if p := a.get("project"); p != "" {
		in["project"] = p
	}
	return in
}

// projectOnly is the input of an action that takes only the project.
func projectOnly(_ *inv, a *parsed) (map[string]any, error) {
	return withProject(a, map[string]any{}), nil
}

// confirmed refuses unless --yes was given or the person confirms on the terminal.
func (r *inv) confirmed(a *parsed, question string) error {
	if a.has("yes") {
		return nil
	}
	if ok, why := r.confirm(question); !ok {
		return errors.New(why)
	}
	return nil
}

// fileJSON reads --file: a JSON document, never a file in the Teamflow home.
func fileJSON(a *parsed) (json.RawMessage, error) {
	path := a.get("file")
	if path == "" {
		return nil, usageError{"Missing option '--file'"}
	}
	b, err := artifacts.ReadText(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		var v any
		err := json.Unmarshal(b, &v)
		return nil, fmt.Errorf("%s is not JSON (%v); `teamflow <command> --help` shows its shape", path, err)
	}
	return json.RawMessage(b), nil
}

// jsonKind is the kind of a JSON document's top value: "list", "object" or "other".
func jsonKind(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	switch {
	case strings.HasPrefix(t, "["):
		return "list"
	case strings.HasPrefix(t, "{"):
		return "object"
	}
	return "other"
}

// baseVersion is --base-version: the Deliverables version read (none for the first).
func baseVersion(a *parsed) (*int64, error) {
	if !a.has("base-version") {
		return nil, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(a.get("base-version")), 10, 64)
	if err != nil || n < 1 {
		return nil, usageError{"Invalid value for '--base-version': " + a.get("base-version") + " (the version you read, a number)"}
	}
	return &n, nil
}

// intFlag is a whole-number flag (0 when not given).
func intFlag(a *parsed, name string) (int64, error) {
	if !a.has(name) {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(a.get(name)), 10, 64)
	if err != nil || n < 0 {
		return 0, usageError{"Invalid value for '--" + name + "': " + a.get(name)}
	}
	return n, nil
}

// ------------------------------------------------------------------ plain output

// render is an answer as plain text: a string as it is, a list item by item, an object field by field.
func render(v any) string {
	var b strings.Builder
	write(&b, v, "")
	return strings.TrimRight(b.String(), "\n")
}

func write(b *strings.Builder, v any, indent string) {
	switch x := v.(type) {
	case nil:
	case string:
		b.WriteString(indentText(x, indent) + "\n")
	case []string:
		for i, s := range x {
			if i > 0 && strings.Contains(s, "\n") {
				b.WriteString("\n")
			}
			b.WriteString(indentText(s, indent) + "\n")
		}
	case []ojson.Obj:
		for i, o := range x {
			if i > 0 {
				b.WriteString("\n")
			}
			write(b, o, indent)
		}
	case ojson.Obj:
		for _, kv := range x {
			switch val := kv.Value.(type) {
			case nil:
				b.WriteString(indent + kv.Key + ": -\n")
			case string:
				if strings.Contains(val, "\n") {
					b.WriteString(indent + kv.Key + ":\n" + indentText(val, indent+"  ") + "\n")
				} else {
					b.WriteString(indent + kv.Key + ": " + val + "\n")
				}
			case ojson.Obj, []ojson.Obj:
				b.WriteString(indent + kv.Key + ":\n")
				write(b, val, indent+"  ")
			case []string:
				if len(val) == 0 {
					b.WriteString(indent + kv.Key + ": -\n")
					continue
				}
				b.WriteString(indent + kv.Key + ":\n")
				for _, s := range val {
					b.WriteString(indent + "  - " + strings.ReplaceAll(s, "\n", "\n"+indent+"    ") + "\n")
				}
			default:
				raw, err := json.Marshal(val)
				if err != nil {
					raw = []byte(fmt.Sprint(val))
				}
				b.WriteString(indent + kv.Key + ": " + string(raw) + "\n")
			}
		}
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			raw = []byte(fmt.Sprint(x))
		}
		b.WriteString(indent + string(raw) + "\n")
	}
}

func indentText(s, indent string) string {
	if indent == "" {
		return s
	}
	return indent + strings.ReplaceAll(s, "\n", "\n"+indent)
}
