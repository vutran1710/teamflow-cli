package cli

// The commands for every action the web app has (no web-only actions): drift resolve|dismiss, deliverables
// archive|delete|history, project title|remove-member|plan, org title, profile (and its avatar), tasks; and
// `teamflow mcp`, the MCP server over stdio. Goldens are picked by what they hold, never by file number.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// golden is the answer a selector names (see goldenPath).
func golden(t *testing.T, sel string) string {
	t.Helper()
	b, err := os.ReadFile(goldenPath(t, sel))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return string(g.Response)
}

func okAnswer(t *testing.T, method string) string { return golden(t, method) }

func refusedAnswer(t *testing.T, method, reason string) string { return golden(t, method+"!"+reason) }

// ------------------------------------------------------------------ drift resolve | dismiss

func TestDriftResolveAndDismiss(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.srv.on("drift.resolve", okAnswer(t, "drift.resolve"))
	r := m.run("", "drift", "resolve", "dr_00000000000000000000000001", "settled", "in", "the", "meeting", "--project", P, "--yes")
	wantCode(t, r, 0)
	c := onlyCall(t, m.srv, "drift.resolve")
	p := params(t, c)
	if c.Batch || c.Auth != "Bearer "+projKey || p["drift"] != "dr_00000000000000000000000001" || p["note"] != "settled in the meeting" {
		t.Fatalf("drift.resolve %+v %v", c, p)
	}
	contains(t, "stdout", r.out, "dr_00000000000000000000000001")

	m.srv.on("drift.dismiss", okAnswer(t, "drift.dismiss"))
	terminal(t, true, "")
	r = m.run("y\n", "drift", "dismiss", "dr_00000000000000000000000001", "not a conflict", "--project", P)
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "drift.dismiss")); p["note"] != "not a conflict" {
		t.Fatalf("drift.dismiss %v", p)
	}
}

func TestDriftSadPaths(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	r := m.run("", "drift", "resolve", "dr_1", "x", "--project", P) // no terminal, no --yes
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "pass --yes")
	noCall(t, m.srv, "drift.resolve")
	terminal(t, true, "")
	r = m.run("n\n", "drift", "dismiss", "dr_1", "x", "--project", P)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "Aborted.")
	noCall(t, m.srv, "drift.dismiss")
	r = m.run("", "drift", "resolve", "dr_1")
	wantCode(t, r, 2)
	r = m.run("", "drift", "resolve", "dr_1", "x", "--yes") // no project, no session, not in the replica
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "which project?")
	m.srv.on("drift.resolve", refusedAnswer(t, "drift.resolve", "NOT_ALLOWED"))
	r = m.run("", "drift", "resolve", "dr_1", "x", "--project", P, "--yes")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "NOT_ALLOWED")
	m.offline()
	r = m.run("", "drift", "dismiss", "dr_1", "x", "--project", P, "--yes")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "unreachable")
	m2 := newMachine(t, false)
	r = m2.run("", "drift", "resolve", "dr_1", "x", "--project", P, "--yes")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "not signed in")
}

// ------------------------------------------------------------------ deliverables archive | delete | history

func TestDeliverablesArchiveDeleteHistory(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.srv.on("deliverables.archive", okAnswer(t, "deliverables.archive"))
	r := m.run("", "deliverables", "archive", "--project", P, "--note", "paused", "--yes")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "deliverables.archive")); p["archived"] != true || p["note"] != "paused" {
		t.Fatalf("archive %v", p)
	}
	contains(t, "stdout", r.out, "archived")
	r = m.run("", "deliverables", "archive", "--project", P, "--restore") // restoring needs no confirmation
	wantCode(t, r, 0)
	if got := m.srv.sent("deliverables.archive"); len(got) != 2 || params(t, got[1])["archived"] != false {
		t.Fatalf("restore %+v", got)
	}

	r = m.run("", "deliverables", "delete", "--project", P)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "pass --yes")
	noCall(t, m.srv, "deliverables.delete")
	m.srv.on("deliverables.delete", okAnswer(t, "deliverables.delete"))
	r = m.run("", "deliverables", "delete", "--project", P, "--yes")
	wantCode(t, r, 0)
	onlyCall(t, m.srv, "deliverables.delete")
	m.srv.on("deliverables.delete", refusedAnswer(t, "deliverables.delete", "UNKNOWN_REF"))
	r = m.run("", "deliverables", "delete", "--project", P, "--yes")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "UNKNOWN_REF")

	m.srv.on("deliverables.history", golden(t, `deliverables.history~"version":1`))
	r = m.run("", "deliverables", "history", "--project", P)
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "v1\t")
	r = m.run("", "deliverables", "history")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "which project?")
}

// ------------------------------------------------------------------ project title | remove-member | plan

func TestProjectCommands(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.srv.on("project.set_title", okAnswer(t, "project.set_title"))
	r := m.run("", "project", "title", "HUY", "Tech", "lead", "--project", P)
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "project.set_title")); p["member"] != M2 || p["title"] != "Tech lead" {
		t.Fatalf("project title %v", p)
	}
	r = m.run("", "project", "title", "Zed", "x", "--project", P)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no member 'Zed'")

	m.srv.on("project.get", pricingGet)
	m.srv.on("project.remove_member", okAnswer(t, "project.remove_member"))
	r = m.run("", "project", "remove-member", "huy", "--project", P)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "pass --yes")
	r = m.run("", "project", "remove-member", "huy", "--project", P, "--yes")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "project.remove_member")); p["member"] != M2 || p["expected_version"] != float64(1) {
		t.Fatalf("remove-member %v", p)
	}

	r = m.run("", "project", "plan", "--project", P) // shows the plan
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "state v1", "Ship v1")
	plan := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(plan, []byte("Ship v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.srv.on("project.write_state", okAnswer(t, "project.write_state"))
	r = m.run("", "project", "plan", "--project", P, "--plan-file", plan, "--yes")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "project.write_state")); p["plan"] != "Ship v2\n" || p["instruction"] != "Be brief" ||
		p["base_version"] != float64(1) {
		t.Fatalf("plan %v", p)
	}
	m.srv.on("project.write_state", refusedAnswer(t, "project.write_state", "STALE_STATE_VERSION"))
	r = m.run("", "project", "plan", "--project", P, "--instruction", "Be kind", "--yes")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "STALE_STATE_VERSION")
	r = m.run("", "project", "plan", "--project", P, "--plan-file", "/nonexistent/plan.md", "--yes")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "cannot read")
	r = m.run("", "project", "plan", "--project", P, "--plan", "x", "--plan-file", plan, "--yes")
	wantCode(t, r, 2)
}

// ------------------------------------------------------------------ org title, profile, tasks

func TestOrgTitle(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("org.members", "org.members")
	m.srv.on("org.set_title", okAnswer(t, "org.set_title"))
	r := m.run("", "org", "title", "Acme", "huy@acme.test", "Engineer")
	wantCode(t, r, 0)
	c := onlyCall(t, m.srv, "org.set_title")
	if p := params(t, c); p["org"] != "Acme" || p["member"] != M2 || p["title"] != "Engineer" || c.Auth != "Bearer "+auth {
		t.Fatalf("org title %v", p)
	}
	m.srv.on("org.set_title", refusedAnswer(t, "org.set_title", "NOT_ALLOWED"))
	r = m.run("", "org", "title", "Acme", "huy", "x")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "NOT_ALLOWED")
}

func TestProfile(t *testing.T) {
	m := newMachine(t, true)
	m.srv.on("account.me", lanMe)
	r := m.run("", "profile")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "name: lan")
	m.srv.on("account.update_profile", okAnswer(t, "account.update_profile"))
	r = m.run("", "profile", "--name", "Huy Nguyen", "--alias", "")
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "account.update_profile")); p["name"] != "Huy Nguyen" || p["alias"] != "" {
		t.Fatalf("profile %v", p)
	}
	r = m.run("", "profile", "--name", " ")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "a name cannot be empty")

	png := filepath.Join(t.TempDir(), "me.png")
	pngBytes, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAQAAAAECAIAAAAmkwkpAAAAEElEQVR4nGMQUDCAIwbiOACaNAYBjjyKngAAAABJRU5ErkJggg==")
	if err := os.WriteFile(png, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	m.srv.on("account.set_avatar", okAnswer(t, "account.set_avatar"))
	r = m.run("", "profile", "avatar", png)
	wantCode(t, r, 0)
	if p := params(t, onlyCall(t, m.srv, "account.set_avatar")); p["media_type"] != "image/png" ||
		p["content_b64"] != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("avatar %v", p)
	}
	m.srv.on("account.remove_avatar", okAnswer(t, "account.remove_avatar"))
	r = m.run("", "profile", "avatar", "--remove")
	wantCode(t, r, 0)
	onlyCall(t, m.srv, "account.remove_avatar")

	for _, tc := range []struct {
		name, file string
		data       []byte
		want       string
	}{
		{"not an image", "notes.txt", []byte("hello"), "PNG, JPEG or WebP"},
		{"too large", "big.png", append(pngBytes, make([]byte, 205*1024)...), "over the 200 KB limit"},
	} {
		f := filepath.Join(t.TempDir(), tc.file)
		if err := os.WriteFile(f, tc.data, 0o644); err != nil {
			t.Fatal(err)
		}
		r := m.run("", "profile", "avatar", f)
		wantCode(t, r, 1)
		contains(t, "stderr", r.err, tc.want)
	}
	r = m.run("", "profile", "avatar", filepath.Join(m.dir, "config.json"))
	wantCode(t, r, 1)
	r = m.run("", "profile", "avatar", "/nonexistent.png")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "no file")
	r = m.run("", "profile", "avatar")
	wantCode(t, r, 2)
	if got := m.srv.sent("account.set_avatar"); len(got) != 1 {
		t.Fatalf("a refused avatar was sent: %d", len(got))
	}
	m2 := newMachine(t, false)
	r = m2.run("", "profile", "--name", "x")
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "not signed in")
}

func TestTasks(t *testing.T) {
	m := newMachine(t, true)
	m.key()
	m.srv.on("task.list", okAnswer(t, "task.list"))
	r := m.run("", "tasks", "--project", P, "--state", "done")
	wantCode(t, r, 0)
	contains(t, "stdout", r.out, "tk_00000000000000000000000001", "ROLLING_UPDATE_PROJECT")
	if p := params(t, onlyCall(t, m.srv, "task.list")); p["state"] != "DONE" {
		t.Fatalf("tasks %v", p)
	}
	r = m.run("", "tasks", "--project", P, "--state", "maybe")
	wantCode(t, r, 2)
	m.srv.on("task.list", refusedAnswer(t, "task.list", "NOT_A_MEMBER"))
	r = m.run("", "tasks", "--project", P)
	wantCode(t, r, 1)
	contains(t, "stderr", r.err, "NOT_A_MEMBER")
}

// ------------------------------------------------------------------ teamflow mcp

func TestMCPServesOverStdio(t *testing.T) {
	newMachine(t, true)
	inR, inW := io.Pipe()
	out := &lockedBuffer{}
	var errb bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- Main([]string{"mcp"}, inR, out, &errb) }()
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	} {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(out.String(), "\n") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	inW.Close() // the agent closes the pipe: the server ends cleanly
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("exit %d, stderr %q", c, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("teamflow mcp did not end when its stdin closed")
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var lines []map[string]any
	for sc.Scan() {
		var l map[string]any
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("not a JSON-RPC line on stdout: %q", sc.Text())
		}
		lines = append(lines, l)
	}
	if len(lines) != 2 {
		t.Fatalf("answers %v (stderr %q)", lines, errb.String())
	}
	init := lines[0]["result"].(map[string]any)
	if init["serverInfo"].(map[string]any)["name"] != "teamflow" || !strings.HasPrefix(init["instructions"].(string), "# Teamflow") {
		t.Fatalf("initialize %v", init)
	}
	if tools := lines[1]["result"].(map[string]any)["tools"].([]any); len(tools) != 44 {
		t.Fatalf("%d tools", len(tools))
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
