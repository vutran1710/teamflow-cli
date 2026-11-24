package cli

// The commands an agent runs, against protocol/agent/commands.json read from the file: every command exists, its
// --help prints its usage line (and, for a JSON-input command, the exact help text), its options are the usage
// line's; every one answers --json with one JSON object; consequential ones ask on a terminal or take --yes; and
// every output ends with what is new for the session, claimed once even across commands running in parallel.
//
// Sad paths covered here: signed out, an expired auth key, offline, a revoked membership, a stale Deliverables
// base, malformed --file JSON (not JSON, the wrong shape, unknown fields), a --file in the Teamflow home, a missing
// --file, no agent session, an unshared session, parallel commands, a refusal mid-command (with its news), and a
// fresh process finding everything again (restart).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/adapters"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
)

const (
	A1 = "ai_00000000000000000000000001"
	A2 = "ai_00000000000000000000000002"
	D1 = "dr_00000000000000000000000001"
)

// agentCommandsFile is protocol/agent/commands.json, read from the file (not the generated Go).
func agentCommandsFile(t *testing.T) []struct {
	Name, Usage   string
	Consequential bool
	Input, Help   *string
} {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(goldenDir), "agent", "commands.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Commands []struct {
			Name, Usage   string
			Consequential bool
			Input, Help   *string
		} `json:"commands"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Commands) != len(protocol.AgentCommands) {
		t.Fatalf("commands.json has %d commands, the generated list %d: run go run ./tools/gen", len(doc.Commands), len(protocol.AgentCommands))
	}
	return doc.Commands
}

// usageFlags are the options a usage line names (--name, or -o).
var usageFlag = regexp.MustCompile(`(?:^|[\s\[|(])(--?[a-z][a-z-]*)`)

// TestEveryCommandOfCommandsJSON: each command exists; --help prints its usage line first, the JSON help text
// exactly for a JSON-input command; the options it takes are the ones its usage line names (and --json).
func TestEveryCommandOfCommandsJSON(t *testing.T) {
	m := newMachine(t, true)
	for _, spec := range agentCommandsFile(t) {
		t.Run(spec.Name, func(t *testing.T) {
			r := m.run("", append(strings.Fields(spec.Name), "--help")...)
			wantCode(t, r, 0)
			first, _, _ := strings.Cut(r.out, "\n")
			if first != "usage: "+spec.Usage {
				t.Fatalf("--help's first line\n got %q\nwant %q", first, "usage: "+spec.Usage)
			}
			if spec.Help != nil && !strings.Contains(r.out, "\n"+*spec.Help+"\n") {
				t.Fatalf("--help lacks the exact JSON help:\n%s", r.out)
			}
			if spec.Input != nil && *spec.Input == "json" && !strings.Contains(spec.Usage, "--file <path>") {
				t.Fatalf("a JSON-input command reads --file: %s", spec.Usage)
			}
			if strings.HasPrefix(spec.Name, "team ") { // behind /teamf: its words as typed
				return
			}
			c := findCommand(spec.Name)
			if c == nil || c.jsonRun == nil {
				t.Fatalf("no agent command %q", spec.Name)
			}
			var want []string
			for _, mm := range usageFlag.FindAllStringSubmatch(strings.TrimPrefix(spec.Usage, "teamflow "+spec.Name), -1) {
				want = append(want, mm[1])
			}
			var have []string
			for _, f := range c.flags {
				name := "--" + f.name
				if f.short != "" && !slices.Contains(want, name) {
					name = "-" + f.short
				}
				have = append(have, name)
			}
			slices.Sort(want)
			want = slices.Compact(want)
			slices.Sort(have)
			if !slices.Equal(want, have) {
				t.Fatalf("options\n usage %v\n  have %v", want, have)
			}
		})
	}
}

// agentMachine is a machine signed in with its key for P, P in the replica (with its artifacts), names, the
// golden Deliverables v1, and an agent session s1 shared with P on every topic, run from inside it. The server
// answers every method with its first golden result.
func agentMachine(t *testing.T) *machine {
	t.Helper()
	m := newMachine(t, true)
	m.key()
	m.replica()
	m.names(map[string]string{M1: "huy", M2: "lan"})
	m.inSession("s1", P, nil)
	for _, method := range []string{"account.projects", "member.me", "project.add_members", "project.history",
		"project.request_update", "project.set_title", "project.remove_member", "project.write_state", "project.create",
		"topic.create", "topic.update", "topic.delete", "signal.list", "artifact.write", "artifact.list",
		"artifact.archive", "deliverables.get", "deliverables.history", "deliverables.archive", "deliverables.delete",
		"deliverables.publish", "deliverables.update_milestone", "deliverables.rename", "drift.list", "drift.resolve",
		"drift.dismiss", "org.members", "org.add_member", "org.remove_member", "org.set_title", "account.update_profile",
		"task.list", "member.list"} {
		m.srv.on(method, method)
	}
	m.srv.on("project.get", pricingGet)
	m.srv.on("account.me", lanMe)
	m.srv.on("signal.emit", accepted)
	m.srv.on("artifact.get", pullAnswer([]byte("# Pricing\nv2\n")))
	t.Chdir(t.TempDir())
	return m
}

// withDeliverables puts the golden Deliverables v1 (Design: A1 Huy's, A2 no owner; Ship) in the replica.
func withDeliverables(m *machine) {
	m.seed(goldenEvent(m.t, `event.deliverables.published~"name":"Design"~"version":1,`))
}

func writeFile(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	goodMilestones = `[{"name": "Design", "due": "2099-01-25", "actions": [{"name": "Draft rules", "topic": "Rounding", "due": "2099-01-20"}]}]`
	oneSignal      = `[{"kind": "DECISION", "topic": "Rounding", "title": "Round half up", "summary": "Half cents round up", "points": ["half up"]}]`
)

// agentRuns are a valid run of every agent command (from inside the shared session), with what it needs first.
var agentRuns = map[string]struct {
	args  []string
	setup func(m *machine)
}{
	"team project":   {[]string{"team", "project"}, nil},
	"team approvals": {[]string{"team", "approvals"}, nil},
	"status":         {[]string{"status"}, nil},
	"whoami":         {[]string{"whoami"}, nil},
	"inbox":          {[]string{"inbox"}, nil},
	"stop":           {[]string{"stop"}, nil},
	"projects":       {[]string{"projects"}, nil},
	"project get":    {[]string{"project", "get"}, nil},
	"project create": {[]string{"project", "create", "--file", "p.json"}, func(m *machine) {
		writeFile(m.t, "p.json", `{"name": "Billing", "topics": [{"name": "Tax", "description": "VAT"}], "members": ["huy"]}`)
	}},
	"project add-member":            {[]string{"project", "add-member", "huy"}, nil},
	"project title":                 {[]string{"project", "title", "huy", "Tech", "lead"}, nil},
	"project remove-member":         {[]string{"project", "remove-member", "huy", "--yes"}, nil},
	"project plan":                  {[]string{"project", "plan", "--plan", "Ship v2", "--yes"}, nil},
	"project history":               {[]string{"project", "history"}, nil},
	"project request-update":        {[]string{"project", "request-update", "--note", "the VAT change"}, nil},
	"topic create":                  {[]string{"topic", "create", "Tax", "--description", "VAT"}, nil},
	"topic update":                  {[]string{"topic", "update", "Currency", "--close"}, nil},
	"topic delete":                  {[]string{"topic", "delete", "Rounding", "--yes"}, nil},
	"signal emit":                   {[]string{"signal", "emit", "--file", "s.json"}, func(m *machine) { writeFile(m.t, "s.json", oneSignal) }},
	"signal get":                    {[]string{"signal", "get", S1}, nil},
	"signal list":                   {[]string{"signal", "list", "--topic", "Rounding", "--text", "round"}, nil},
	"signal cite":                   {[]string{"signal", "cite", S1}, nil},
	"artifact write":                {[]string{"artifact", "write", "spec.md", "--topic", "Rounding", "--summary", "The spec"}, func(m *machine) { writeFile(m.t, "spec.md", "# Spec\n") }},
	"artifact link":                 {[]string{"artifact", "link", "https://media.example.com/demo.mp4", "--name", "demo.mp4", "--topic", "Rounding", "--summary", "A demo"}, nil},
	"artifact pull":                 {[]string{"artifact", "pull", S1, "-o", "out.md"}, nil},
	"artifact list":                 {[]string{"artifact", "list"}, nil},
	"artifact archive":              {[]string{"artifact", "archive", S1, "--note", "replaced"}, nil},
	"deliverables":                  {[]string{"deliverables"}, nil},
	"deliverables validate":         {[]string{"deliverables", "validate", "--file", "d.json"}, func(m *machine) { writeFile(m.t, "d.json", goodMilestones) }},
	"deliverables publish":          {[]string{"deliverables", "publish", "--file", "d.json"}, func(m *machine) { writeFile(m.t, "d.json", goodMilestones) }},
	"deliverables update-milestone": {[]string{"deliverables", "update-milestone", "--base-version", "1", "--file", "ms.json"}, func(m *machine) { withDeliverables(m); writeFile(m.t, "ms.json", shipIt) }},
	"deliverables rename":           {[]string{"deliverables", "rename", "--base-version", "1", "--file", "n.json"}, func(m *machine) { withDeliverables(m); writeFile(m.t, "n.json", `{"`+A1+`": "Draft the rules"}`) }},
	"deliverables history":          {[]string{"deliverables", "history"}, nil},
	"deliverables archive":          {[]string{"deliverables", "archive", "--note", "paused", "--yes"}, nil},
	"deliverables delete":           {[]string{"deliverables", "delete", "--yes"}, nil},
	"action result":                 {[]string{"action", "result", A2, "PARTIAL", "--summary", "half done", "--point", "EUR listed"}, withDeliverables},
	"action revoke":                 {[]string{"action", "revoke", A1, "Rules", "ignore", "half", "cents"}, withDeliverables},
	"drift list":                    {[]string{"drift", "list", "--status", "OPEN"}, nil},
	"drift resolve":                 {[]string{"drift", "resolve", D1, "settled", "--yes"}, nil},
	"drift dismiss":                 {[]string{"drift", "dismiss", D1, "no", "conflict", "--yes"}, nil},
	"approve":                       {[]string{"approve", S1}, func(m *machine) { m.seed(goldenEvent(m.t, `event.signal.accepted~"PENDING"`)) }},
	"reject":                        {[]string{"reject", S1, "rounding", "is", "wrong"}, func(m *machine) { m.seed(goldenEvent(m.t, `event.signal.accepted~"PENDING"`)) }},
	"org members":                   {[]string{"org", "members", "Acme"}, nil},
	"org add-member":                {[]string{"org", "add-member", "Acme", "new@acme.test", "--title", "Ops"}, nil},
	"org remove-member":             {[]string{"org", "remove-member", "Acme", "huy@acme.test", "--yes"}, nil},
	"org title":                     {[]string{"org", "title", "Acme", "huy", "Tech", "lead"}, nil},
	"profile":                       {[]string{"profile", "--alias", "H"}, nil},
	"tasks":                         {[]string{"tasks", "--state", "DONE"}, nil},
}

const shipIt = `{"id": "ms_00000000000000000000000002", "name": "Ship it", "due": "2099-02-04", "actions": [{"id": "ai_00000000000000000000000003", "name": "Implement rounding", "topic": "Rounding", "due": "2099-01-30", "owner": "lan"}]}`

// jsonObject is stdout as one JSON object.
func jsonObject(t *testing.T, r result) map[string]any {
	t.Helper()
	var o map[string]any
	if err := json.Unmarshal([]byte(r.out), &o); err != nil {
		t.Fatalf("stdout is not one JSON object (%v): %s", err, r)
	}
	return o
}

// TestEveryAgentCommandAnswersJSON: --json on every agent command gives one JSON object, with no error.
func TestEveryAgentCommandAnswersJSON(t *testing.T) {
	for _, spec := range agentCommandsFile(t) {
		run, ok := agentRuns[spec.Name]
		if !ok {
			t.Errorf("no example run of %s", spec.Name)
			continue
		}
		t.Run(spec.Name, func(t *testing.T) {
			m := agentMachine(t)
			if run.setup != nil {
				run.setup(m)
			}
			r := m.run("", append(run.args, "--json")...)
			o := jsonObject(t, r)
			if r.code != 0 || o["error"] != nil {
				t.Fatalf("--json failed: %s", r)
			}
			if r.err != "" {
				t.Fatalf("--json wrote to stderr: %s", r)
			}
		})
	}
}

// TestConsequentialCommandsConfirm: a consequential command whose usage takes --yes asks on a terminal (no:
// nothing sent), is refused with no terminal and no --yes (nothing sent), with --json too; the others need the
// person's own words as arguments (a reject's reason, a revoke's reason).
func TestConsequentialCommandsConfirm(t *testing.T) {
	for _, spec := range agentCommandsFile(t) {
		if !spec.Consequential {
			continue
		}
		run := agentRuns[spec.Name]
		if !strings.Contains(spec.Usage, "--yes") {
			continue
		}
		args := slices.DeleteFunc(slices.Clone(run.args), func(a string) bool { return a == "--yes" })
		t.Run(spec.Name, func(t *testing.T) {
			for _, mode := range []string{"plain", "json"} {
				m := agentMachine(t)
				if run.setup != nil {
					run.setup(m)
				}
				a := args
				if mode == "json" {
					a = append(slices.Clone(args), "--json")
				}
				before := len(m.srv.methods())
				r := m.run("", a...)
				wantCode(t, r, 1)
				contains(t, mode, r.out+r.err, "pass --yes")
				if mode == "json" && jsonObject(t, r)["error"] == nil {
					t.Fatalf("the refusal is not in the JSON answer: %s", r)
				}
				for _, c := range m.srv.methods()[before:] {
					if !slices.Contains([]string{"sync.poll", "member.list", "project.get", "key.fetch", "org.members", "account.me"}, c) {
						t.Fatalf("%s sent %s without confirmation", spec.Name, c)
					}
				}
				terminal(t, true, "")
				r = m.run("n\n", a...)
				wantCode(t, r, 1)
				contains(t, mode, r.out+r.err, "Aborted")
				terminal(t, false, "")
			}
		})
	}
	// The person's own words: a reject without its reason, a revoke without one, are refused.
	m := agentMachine(t)
	m.seed(goldenEvent(t, `event.signal.accepted~"PENDING"`))
	wantCode(t, m.run("", "reject", S1), 2)
	wantCode(t, m.run("", "action", "revoke", A1), 2)
	noCall(t, m.srv, "signal.emit")
}

// ------------------------------------------------------------------ news on every command

// noticed puts a notice for s1, and S1 "Needs Huy" from lan asking huy (M1) to approve.
func noticed(t *testing.T, m *machine, text string) {
	t.Helper()
	m.seed(goldenEvent(t, `event.signal.accepted~"PENDING"`))
	if err := m.store().Notice("s1", protocol.NoticeTypeDriftFound, D1, text, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestNewsEndsEveryCommand: plain output ends with a delimited trailer of the approval requests and notices; the
// same in --json fields; once only; a failure carries them too; nothing outside an agent session.
func TestNewsEndsEveryCommand(t *testing.T) {
	m := agentMachine(t)
	noticed(t, m, "Drift found (PLAN_CONFLICT): rounding [dr_1]")
	r := m.run("", "signal", "cite", S1)
	wantCode(t, r, 0)
	head, tail, ok := strings.Cut(r.out, trailerBegin)
	if !ok || !strings.HasSuffix(r.out, trailerEnd+"\n") || !strings.Contains(head, "cited") {
		t.Fatalf("no trailer after the output:\n%s", r.out)
	}
	contains(t, "trailer", tail, "a new approval request arrived", S1, "«Needs Huy»", "Notices:\n- Drift found (PLAN_CONFLICT)")
	r = m.run("", "signal", "cite", S1)
	lacks(t, "stdout", r.out, trailerBegin) // claimed once

	m = agentMachine(t)
	noticed(t, m, "notice-json")
	o := jsonObject(t, m.run("", "whoami", "--json"))
	if !strings.Contains(fmt.Sprint(o["approval_requests"]), S1) || fmt.Sprint(o["notices"]) != "[notice-json]" || o["account"] != "lan" {
		t.Fatalf("--json news %v", o)
	}
	if o := jsonObject(t, m.run("", "whoami", "--json")); o["notices"] != nil || o["approval_requests"] != nil {
		t.Fatalf("news twice: %v", o)
	}
	// A list answer is under "result", with the news beside it.
	m = agentMachine(t)
	noticed(t, m, "notice-list")
	if o := jsonObject(t, m.run("", "projects", "--json")); o["result"] == nil || o["notices"] == nil {
		t.Fatalf("a list with news: %v", o)
	}

	// A failure carries the news: plain (the refusal on stderr, the news on stdout) and --json.
	m = agentMachine(t)
	noticed(t, m, "notice-on-failure")
	m.srv.on("drift.list", "drift.list!NOT_A_MEMBER")
	r = m.run("", "drift", "list")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "NOT_A_MEMBER")
	contains(t, "stdout", r.out, trailerBegin, "notice-on-failure")
	m = agentMachine(t)
	noticed(t, m, "notice-on-json-failure")
	m.srv.on("drift.list", "drift.list!NOT_A_MEMBER")
	r = m.run("", "drift", "list", "--json")
	wantCode(t, r, 1)
	if o := jsonObject(t, r); !strings.Contains(fmt.Sprint(o["error"]), "NOT_A_MEMBER") || fmt.Sprint(o["notices"]) != "[notice-on-json-failure]" {
		t.Fatalf("a failure without its news: %v", o)
	}
	// A usage error too.
	m = agentMachine(t)
	noticed(t, m, "notice-on-usage")
	r = m.run("", "signal", "list", "--limit", "many", "--json")
	wantCode(t, r, 2)
	if o := jsonObject(t, r); o["error"] == nil || fmt.Sprint(o["notices"]) != "[notice-on-usage]" {
		t.Fatalf("a usage error without its news: %v", o)
	}

	// Outside an agent session: no news, and nothing claimed.
	m = newMachine(t, true)
	m.key()
	m.replica()
	m.inSession("s1", P, nil)
	noticed(t, m, "kept")
	m.srv.on("account.projects", "account.projects")
	st := m.store()
	if _, err := st.UpsertSession("s1", "claude-code", 1, m.dir, time.Now()); err != nil { // its agent is another process
		t.Fatal(err)
	}
	r = m.run("", "projects")
	lacks(t, "stdout", r.out, trailerBegin)
	if ns, _ := m.store().Notices("s1", true); len(ns) == 0 {
		t.Fatal("a notice was claimed by a command outside its session")
	}
}

// TestNewsNeverDoubledAcrossParallelCommands: eight commands at once (an agent's parallel shell calls), some
// failing: each piece of news reaches exactly one output.
func TestNewsNeverDoubledAcrossParallelCommands(t *testing.T) {
	m := agentMachine(t)
	noticed(t, m, "notice-one")
	if err := m.store().Notice("s1", protocol.NoticeTypeTopicDeleted, T2, "notice-two", time.Now()); err != nil {
		t.Fatal(err)
	}
	m.srv.on("drift.list", "drift.list!NOT_A_MEMBER") // half of them fail
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []string
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := []string{"signal", "cite", S1}
			if i%2 == 0 {
				args = []string{"drift", "list", "--json"}
			}
			r := m.run("", args...)
			mu.Lock()
			all = append(all, r.out+r.err)
			mu.Unlock()
		}()
	}
	wg.Wait()
	joined := strings.Join(all, "\n")
	for _, want := range []string{"a new approval request arrived", "notice-one", "notice-two"} {
		if n := strings.Count(joined, want); n != 1 {
			t.Errorf("%q reached %d outputs, want 1:\n%s", want, n, joined)
		}
	}
}

// ------------------------------------------------------------------ the new commands

func TestSignalEmit(t *testing.T) {
	m := agentMachine(t)
	writeFile(t, "s.json", `[{"kind": "DECISION", "topic": "rounding", "title": "Round half up", "summary": "s", "points": ["p"],
		"refs": ["`+S1+`"], "approvers": ["lan"], "required": 1, "due": "2099-01-01T00:00:00Z"}]`)
	r := m.run("", "signal", "emit", "--file", "s.json", "--json")
	wantCode(t, r, 0)
	c := onlyCall(t, m.srv, "signal.emit")
	p := params(t, c)
	sig := p["signal"].(map[string]any)
	rfa := sig["request_for_approval"].(map[string]any)
	if p["actor"] != "AGENT" || p["client_session"] != "s1" || sig["topic"] != T1 || sig["kind"] != "DECISION" ||
		fmt.Sprint(rfa["approvers"]) != "["+M2+"]" || rfa["required"] != 1.0 || fmt.Sprint(sig["refs"]) != "["+S1+"]" {
		t.Fatalf("params %v", p)
	}
	if res := jsonObject(t, r)["result"].([]any); res[0].(map[string]any)["accepted"] != true {
		t.Fatalf("answer %s", r.out)
	}
	// Plain output: one answer per signal.
	r = m.run("", "signal", "emit", "--file", "s.json")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "accepted: true")
}

func TestSignalEmitSadPaths(t *testing.T) {
	m := agentMachine(t)
	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"not JSON":          {`[{"kind": `, 1, "s.json is not JSON"},
		"an object":         {`{"kind": "NOTE"}`, 1, "must hold a list of signals"},
		"empty":             {`[]`, 1, "no signals in s.json"},
		"an unknown field":  {`[{"kind": "NOTE", "topic": "Rounding", "title": "t", "summary": "s", "points": ["p"], "secret": "x"}]`, 1, "secret"},
		"a wrong type":      {`[{"kind": "NOTE", "topic": "Rounding", "title": 7}]`, 1, "title"},
		"a verdict slipped": {`[{"kind": "APPROVE", "topic": "Rounding", "title": "t", "summary": "s", "points": ["p"]}]`, 1, "a verdict is recorded on its own"},
		"an unknown topic":  {`[{"kind": "NOTE", "topic": "Taxes", "title": "t", "summary": "s", "points": ["p"]}]`, 1, "no topic 'Taxes'"},
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, "s.json", tc.body)
			r := m.run("", "signal", "emit", "--file", "s.json")
			wantCode(t, r, tc.code)
			contains(t, "output", r.out+r.err, tc.want)
		})
	}
	noCall(t, m.srv, "signal.emit")
	r := m.run("", "signal", "emit")
	wantCode(t, r, 2)
	contains(t, "stderr", r.err, "--file")
	r = m.run("", "signal", "emit", "--file", "missing.json")
	wantCode(t, r, 1)
	// A file in the Teamflow home is never read (it holds the keys), however the path names it.
	for name, p := range homeForms(t, m) {
		r := m.run("", "signal", "emit", "--file", p)
		wantCode(t, r, 1)
		contains(t, name, r.err, "Teamflow's home")
		lacks(t, name, r.out+r.err, auth)
	}
	// Offline: queued in the spool, said so, and sent once the server is back (a later command, a fresh process).
	writeFile(t, "s.json", oneSignal)
	down := agentMachine(t)
	writeFile(t, "s.json", oneSignal)
	down.srv.offline(true)
	r = down.run("", "signal", "emit", "--file", "s.json", "--json")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, `"queued": true`)
	if n, _ := down.store().SpoolSize(); n != 1 {
		t.Fatalf("spool %d", n)
	}
	// No session, an unshared one, signed out, an expired key.
	m = agentMachine(t)
	writeFile(t, "s.json", oneSignal)
	if err := m.store().Share("s1", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	r = m.run("", "signal", "emit", "--file", "s.json")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "this session is not shared")
	m = newMachine(t, true)
	t.Chdir(t.TempDir())
	writeFile(t, "s.json", oneSignal)
	r = m.run("", "signal", "emit", "--file", "s.json")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no agent session found")
	m = newMachine(t, false)
	m.project()
	m.inSession("s1", P, nil)
	r = m.run("", "signal", "emit", "--file", "s.json")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "not signed in")
}

func TestSignalReadAndCite(t *testing.T) {
	m := agentMachine(t)
	r := m.run("", "signal", "get", S1)
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, S1, "ARTIFACT")
	r = m.run("", "signal", "get", S9)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no signal "+S9)

	m.srv.on("signal.list", "signal.list")
	r = m.run("", "signal", "list", "--topic", "Rounding", "--kind", "decision", "--in-force", "--text", "round", "--after", "3", "--limit", "5", "--json")
	wantCode(t, r, 0)
	p := params(t, m.srv.sent("signal.list")[0])
	if p["topic"] != T1 || p["kind"] != "DECISION" || p["in_force_only"] != true || p["query"] != "round" || p["after"] != 3.0 || p["limit"] != 5.0 {
		t.Fatalf("signal.list params %v", p)
	}
	for _, bad := range [][]string{{"--limit", "x"}, {"--after", "-1"}} {
		wantCode(t, m.run("", append([]string{"signal", "list"}, bad...)...), 2)
	}
	r = m.run("", "signal", "list", "--kind", "MAYBE")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no signal kind 'MAYBE'")

	o := jsonObject(t, m.run("", "signal", "cite", S1, S9, "--json"))
	if fmt.Sprint(o["cited"]) != "["+S1+"]" || fmt.Sprint(o["unknown"]) != "["+S9+"]" {
		t.Fatalf("cite %v", o)
	}
}

func TestWhoamiAndInbox(t *testing.T) {
	m := agentMachine(t)
	o := jsonObject(t, m.run("", "whoami", "--json"))
	if o["account"] != "lan" || o["member"] != M1 || o["project"] != P {
		t.Fatalf("whoami %v", o)
	}
	m.srv.on("account.me", "account.me!NOT_ALLOWED") // an expired auth key
	r := m.run("", "whoami")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "NOT_ALLOWED")

	r = m.run("", "inbox")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "No notices for this session.")
	noticed(t, m, "an old notice")
	_ = m.run("", "status") // claims it
	r = m.run("", "inbox")
	contains(t, "stdout", r.out, "an old notice") // the inbox keeps every notice, shown or not
	m = newMachine(t, true)
	r = m.run("", "inbox")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no agent session found")
}

func TestProjectAgentCommands(t *testing.T) {
	m := agentMachine(t)
	o := jsonObject(t, m.run("", "project", "get", "--json"))
	if o["id"] != P || o["initiator"] != "lan" || o["plan"] == nil {
		t.Fatalf("project get %v", o)
	}
	writeFile(t, "p.json", `{"name": "Billing", "topics": [{"name": "Tax", "description": "VAT"}], "plan": "Bill", "instruction": "Be brief", "members": ["huy"]}`)
	r := m.run("", "project", "create", "--file", "p.json")
	wantCode(t, r, 0)
	p := params(t, onlyCall(t, m.srv, "project.create"))
	if p["org"] != "Acme" || p["name"] != "Billing" || fmt.Sprint(p["members"]) != "["+M2+"]" {
		t.Fatalf("project.create %v", p)
	}
	for body, want := range map[string]string{`[]`: "must hold one project", `{"name": "X", "owner": "me"}`: "owner", `{"name": `: "not JSON"} {
		writeFile(t, "bad.json", body)
		r := m.run("", "project", "create", "--file", "bad.json")
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, want)
	}
	r = m.run("", "project", "add-member", "huy", "lan")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "project.add_members")); fmt.Sprint(p["members"]) != "["+M2+" "+M1+"]" && fmt.Sprint(p["members"]) != "["+M1+" "+M2+"]" {
		t.Fatalf("add_members %v", p)
	}
	r = m.run("", "project", "add-member", "nobody")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no member 'nobody'")
	r = m.run("", "project", "history", "--json")
	wantCode(t, r, 0)
	r = m.run("", "project", "request-update", "--note", strings.Repeat("x", 241))
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "at most 240")
	m.srv.on("project.request_update", "project.request_update!TASK_IN_PROGRESS")
	r = m.run("", "project", "request-update")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "TASK_IN_PROGRESS")
	// Another project by id, from outside any session.
	m = newMachine(t, true)
	m.key()
	m.srv.on("project.get", pricingGet)
	if o := jsonObject(t, m.run("", "project", "get", "--project", P, "--json")); o["id"] != P {
		t.Fatalf("by id %v", o)
	}
	r = m.run("", "project", "get")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, whichProject)
}

func TestTopicCreateAndUpdate(t *testing.T) {
	m := agentMachine(t)
	r := m.run("", "topic", "create", "Tax", "--description", "VAT rules")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "topic.create")); p["name"] != "Tax" || p["description"] != "VAT rules" || p["expected_version"] != 1.0 {
		t.Fatalf("topic.create %v", p)
	}
	wantCode(t, m.run("", "topic", "create", "Tax"), 2) // --description is required
	r = m.run("", "topic", "update", "Currency", "--name", "FX", "--reopen")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "topic.update")); p["topic"] != T2 || p["name"] != "FX" || p["closed"] != false {
		t.Fatalf("topic.update %v", p)
	}
	wantCode(t, m.run("", "topic", "update", "Currency", "--close", "--reopen"), 2)
	wantCode(t, m.run("", "topic", "update", "Currency"), 2)
	r = m.run("", "topic", "update", "Nope", "--close")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no topic 'Nope'")
	m.srv.on("topic.create", "topic.create!NOT_ALLOWED")
	r = m.run("", "topic", "create", "Tax", "--description", "x")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "NOT_ALLOWED")
}

func TestDeliverablesPublishing(t *testing.T) {
	m := agentMachine(t)
	writeFile(t, "d.json", goodMilestones)
	r := m.run("", "deliverables", "publish", "--file", "d.json", "--json")
	wantCode(t, r, 0)
	if o := jsonObject(t, r); o["published"] != true {
		t.Fatalf("publish %v", o)
	}
	if p := params(t, onlyCall(t, m.srv, "deliverables.publish")); p["base_version"] != nil {
		t.Fatalf("the first needs no base: %v", p)
	}
	// With Deliverables v1 here: the base is required and must be current (a stale base sends nothing).
	m = agentMachine(t)
	withDeliverables(m)
	writeFile(t, "d.json", goodMilestones)
	r = m.run("", "deliverables", "publish", "--file", "d.json")
	wantCode(t, r, 1)
	contains(t, "stdout", r.out, "published: false", "must say which version it is based on")
	r = m.run("", "deliverables", "publish", "--base-version", "7", "--file", "d.json")
	wantCode(t, r, 1)
	contains(t, "stdout", r.out, "at version 1, not version 7")
	wantCode(t, m.run("", "deliverables", "publish", "--base-version", "one", "--file", "d.json"), 2)
	noCall(t, m.srv, "deliverables.publish")
	writeFile(t, "ms.json", shipIt)
	r = m.run("", "deliverables", "update-milestone", "--base-version", "1", "--file", "ms.json")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "deliverables.update_milestone")); p["base_version"] != 1.0 || p["milestone"].(map[string]any)["name"] != "Ship it" {
		t.Fatalf("update_milestone %v", p)
	}
	writeFile(t, "list.json", goodMilestones)
	r = m.run("", "deliverables", "update-milestone", "--base-version", "1", "--file", "list.json")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "must hold one milestone")
	writeFile(t, "n.json", `{"`+A1+`": "Draft the rules"}`)
	r = m.run("", "deliverables", "rename", "--base-version", "1", "--file", "n.json")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "deliverables.rename")); fmt.Sprint(p["names"]) != "map["+A1+":Draft the rules]" {
		t.Fatalf("rename %v", p)
	}
	writeFile(t, "n.json", `["x"]`)
	r = m.run("", "deliverables", "rename", "--base-version", "1", "--file", "n.json")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "the new names by id")
	// Offline: not queued, and said so.
	m.srv.offline(true)
	writeFile(t, "n.json", `{"`+A1+`": "Again"}`)
	r = m.run("", "deliverables", "rename", "--base-version", "1", "--file", "n.json")
	wantCode(t, r, 1)
	contains(t, "stdout", r.out, "Deliverables are not queued")
}

func TestActionCommands(t *testing.T) {
	m := agentMachine(t)
	withDeliverables(m)
	r := m.run("", "action", "result", A2, "blocked", "--summary", "waiting", "--blocker", "no FX feed")
	wantCode(t, r, 0)
	sig := params(t, onlyCall(t, m.srv, "signal.emit"))["signal"].(map[string]any)
	body := sig["body"].(map[string]any)
	if sig["kind"] != "ACTION_RESULT" || body["outcome"] != "BLOCKED" || body["action"] != A2 || body["blocker"] != "no FX feed" {
		t.Fatalf("action result %v", sig)
	}
	wantCode(t, m.run("", "action", "result", A2, "MAYBE", "--summary", "s"), 2)
	wantCode(t, m.run("", "action", "result", A2, "DONE"), 2) // --summary is required
	r = m.run("", "action", "result", "Nope", "DONE", "--summary", "s", "--json")
	wantCode(t, r, 1)
	if o := jsonObject(t, r); o["reason"] != "INVALID" {
		t.Fatalf("an unknown action %v", o)
	}
	r = m.run("", "action", "revoke", A1, "the", "rules", "changed")
	wantCode(t, r, 0)
	if sig := params(t, m.srv.sent("signal.emit")[1])["signal"].(map[string]any); sig["kind"] != "ACTION_REVOKE" {
		t.Fatalf("revoke %v", sig)
	}
	// Offline: queued, said so.
	m.srv.offline(true)
	r = m.run("", "action", "result", A2, "PARTIAL", "--summary", "s", "--json")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, `"queued": true`)
}

func TestDriftList(t *testing.T) {
	m := agentMachine(t)
	r := m.run("", "drift", "list", "--status", "open", "--json")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "drift.list")); p["status"] != "OPEN" {
		t.Fatalf("drift.list %v", p)
	}
	r = m.run("", "drift", "list", "--status", "maybe")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no drift status 'maybe'")
}

func TestArtifactApprovalsAndTitle(t *testing.T) {
	m := agentMachine(t)
	writeFile(t, "spec.md", "# Spec\n")
	r := m.run("", "artifact", "write", "spec.md", "--topic", "Rounding", "--summary", "The spec", "--title", "Pricing spec",
		"--approver", "lan", "--approver", "huy", "--required", "1")
	wantCode(t, r, 0)
	p := params(t, onlyCall(t, m.srv, "artifact.write"))
	sig := p["signal"].(map[string]any)
	rfa := sig["request_for_approval"].(map[string]any)
	if sig["title"] != "Pricing spec" || rfa["required"] != 1.0 || len(rfa["approvers"].([]any)) != 2 || p["actor"] != "AGENT" {
		t.Fatalf("artifact.write %v", p)
	}
	r = m.run("", "artifact", "link", "https://media.example.com/talk", "--name", "talk", "--topic", "Rounding", "--summary", "s",
		"--media-type", "video/mp4", "--approver", "lan")
	wantCode(t, r, 0)
	sig = params(t, onlyCall(t, m.srv, "signal.emit"))["signal"].(map[string]any)
	body := sig["body"].(map[string]any)
	if body["media_type"] != "video/mp4" || body["kind"] != "VIDEO" || sig["request_for_approval"].(map[string]any)["required"] != 1.0 {
		t.Fatalf("link %v", sig)
	}
	for _, bad := range [][]string{{"--required", "3", "--approver", "lan"}, {"--required", "1"}, {"--required", "x", "--approver", "lan"}} {
		r := m.run("", append([]string{"artifact", "write", "spec.md", "--topic", "Rounding", "--summary", "s"}, bad...)...)
		if r.code == 0 {
			t.Fatalf("%v accepted: %s", bad, r)
		}
	}
	r = m.run("", "artifact", "write", "spec.md", "--topic", "Rounding", "--summary", "s", "--approver", "nobody")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "nobody")
	if n := len(m.srv.sent("artifact.write")); n != 1 {
		t.Fatalf("refused writes were sent: %d", n)
	}
}

// TestRestartFindsEverything: a fresh process (each run opens the home anew) finds the sign-in, the key, the session
// by its agent and its notices; a signal queued while offline is sent by a later command once, after a restart.
func TestRestartFindsEverything(t *testing.T) {
	m := agentMachine(t)
	writeFile(t, "s.json", oneSignal)
	m.srv.offline(true)
	r := m.run("", "signal", "emit", "--file", "s.json")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "queued: true")
	m.srv.offline(false)
	// later: the news's sync is not held back by the earlier failure
	st := m.store()
	if err := st.SetHookOffline(P, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimHookSync(P, time.Now().Add(time.Hour), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Notice("s1", protocol.NoticeTypeDriftFound, D1, "after-restart", time.Now()); err != nil {
		t.Fatal(err)
	}
	r = m.run("", "whoami")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "member: "+M1, "after-restart")
	if n, _ := m.store().SpoolSize(); n != 0 {
		t.Fatalf("spool after the restart %d", n)
	}
	if n := len(m.srv.sent("signal.emit")); n != 1 {
		t.Fatalf("sent %d times", n)
	}
	_ = m.run("", "whoami")
	if n := len(m.srv.sent("signal.emit")); n != 1 {
		t.Fatalf("sent again: %d", n)
	}
}

// TestArtifactPullNeverReachesTheHome: `artifact pull -o` (the MCP save_to's successor) never writes into the
// Teamflow home: not through a link to a file there, a linked folder (new file, existing file, a folder made under
// it), a hard link to one of its files, a case variant of its path, or the home itself; --json says why. A plain
// file the person names is written, and replaced when named again.
func TestArtifactPullNeverReachesTheHome(t *testing.T) {
	m := agentMachine(t)
	config := filepath.Join(m.dir, "config.json")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	linkFile := filepath.Join(work, "out.json")
	must(t, os.Symlink(config, linkFile))
	linkDir := filepath.Join(work, "into-home")
	must(t, os.Symlink(m.dir, linkDir))
	hard := filepath.Join(work, "hard.json")
	must(t, os.Link(config, hard))
	cases := map[string]string{
		"a link to a file in the home":             linkFile,
		"a new file through a linked folder":       filepath.Join(linkDir, "new.md"),
		"an existing file through a linked folder": filepath.Join(linkDir, "config.json"),
		"a hard link to a file in the home":        hard,
		"the home itself":                          filepath.Join(m.dir, "x.md"),
		"a folder made under a link":               filepath.Join(linkDir, "sub", "x.md"),
	}
	if upper := strings.ToUpper(m.dir); upper != m.dir {
		if fi, err := os.Stat(upper); err == nil { // a case-insensitive file system (macOS)
			if h, _ := os.Stat(m.dir); os.SameFile(fi, h) {
				cases["a case variant of the home"] = filepath.Join(upper, "y.md")
			}
		}
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			r := m.run("", "artifact", "pull", S1, "-o", out, "--json")
			wantCode(t, r, 1)
			if o := jsonObject(t, r); o["error"] == nil || o["saved_to"] != nil {
				t.Fatalf("written: %v", o)
			}
		})
	}
	if after, _ := os.ReadFile(config); string(after) != string(before) {
		t.Fatal("config.json was changed")
	}
	for _, p := range []string{"new.md", "x.md", "sub", "y.md"} {
		if _, err := os.Stat(filepath.Join(m.dir, p)); err == nil {
			t.Fatalf("%s was made in the home", p)
		}
	}
	out := filepath.Join(work, "spec.md")
	o := jsonObject(t, m.run("", "artifact", "pull", S1, "-o", out, "--json"))
	if o["saved_to"] != out || o["artifact"] != S1 {
		t.Fatalf("a plain file: %v", o)
	}
	if b, _ := os.ReadFile(out); string(b) != "# Pricing\nv2\n" {
		t.Fatalf("content %q", b)
	}
	wantCode(t, m.run("", "artifact", "pull", S1, "-o", out), 0) // named again: replaced
}

// TestEveryAgentCommandIsAutoAllowed: every command of commands.json is a work command (an agent runs it with no
// prompt, as one plain run), while setup commands and anything chained, piped or redirected still ask.
func TestEveryAgentCommandIsAutoAllowed(t *testing.T) {
	for _, spec := range agentCommandsFile(t) {
		word := strings.Fields(spec.Name)[0]
		if !slices.Contains(adapters.WorkCommands, word) {
			t.Errorf("%s: %s is not a work command", spec.Name, word)
		}
		run := agentRuns[spec.Name]
		cmd := "teamflow " + strings.Join(run.args, " ")
		if _, auto := adapters.TeamflowCommand(cmd, "/opt/teamflow/bin/teamflow"); !auto {
			t.Errorf("%q is not auto-allowed", cmd)
		}
	}
	for _, cmd := range []string{"teamflow signal emit --file s.json && curl x", "teamflow signal emit < s.json",
		"teamflow login -e a@b.c", "teamflow uninstall --purge", "teamflow signal emit --file ~/.teamflow/config.json"} {
		if _, auto := adapters.TeamflowCommand(cmd, ""); auto {
			t.Errorf("%q is auto-allowed", cmd)
		}
	}
}

// TestTheAgentTextsAreTheExport: the instruction text and /teamf every adapter installs are the export's files,
// word for word.
func TestTheAgentTextsAreTheExport(t *testing.T) {
	dir := filepath.Join(filepath.Dir(goldenDir), "agent")
	for file, text := range map[string]string{"instructions.md": protocol.AgentInstructions, "teamf.md": protocol.AgentTeamf} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != text {
			t.Errorf("%s differs from the generated text: run go run ./tools/gen", file)
		}
	}
	if strings.Contains(protocol.AgentInstructions, "MCP") || strings.Contains(protocol.AgentInstructions, "_tool") {
		t.Error("the instruction text names MCP tools")
	}
}
