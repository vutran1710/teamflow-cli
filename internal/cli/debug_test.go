package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const (
	secret = "0123456789abcdefghijklmnopqrstuv"
	pkey   = "tfp_" + secret
)

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// filled makes a home with something in every part of the store.
func filled(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	t.Setenv("TEAMFLOW_HOME", dir)
	must(t, os.MkdirAll(dir, 0o700))
	st, err := store.Open(dir)
	must(t, err)
	defer st.Close()
	must(t, st.PutProjectKey(store.Binding("https://tf.example", "lan@acme.test"), "pj_1", pkey, "mb_1", "og_1", "Pricing", t0))
	_, err = st.UpsertSession("s1", "agent", 42, "/work", t0)
	must(t, err)
	must(t, st.Share("s1", "pj_1", "mb_1", api.KeyRef(pkey), []string{"tp_1"}))
	must(t, st.Notice("s1", protocol.NoticeTypeDriftFound, "dr_1", "Drift found", t0))
	must(t, st.SpoolAdd("sg_2", "s1", api.KeyRef(pkey), `{"signal":{"id":"sg_2","kind":"NOTE"}}`, t0))
	must(t, st.Cite("s1", []string{"sg_1"}, t0))
	s := protocol.NewStoredSignal()
	s.Signal = protocol.NewSignal()
	s.Signal.ID, s.Signal.Project, s.Signal.Topic, s.Signal.Kind, s.Signal.Title = "sg_1", "pj_1", "tp_1", protocol.SignalKindNote, "Hello"
	s.Signal.Body = protocol.CommentBody{Type: "comment.v1", Text: "hi"}
	s.Seq, s.Chain, s.ChainVersion, s.State = 1, "sg_1", 1, protocol.SignalStateInForce
	must(t, st.PutSignal(s))
	must(t, st.SetCursor("pj_1", 7))
	must(t, st.PutPeople(map[string]store.Person{"mb_1": {Name: "Lan", Alias: "lan", Title: "Lead"}}))
	must(t, st.SetLiveAt("pj_1", t0))
	return dir
}

func run(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Debug(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDebugStateHasNoKeys(t *testing.T) {
	filled(t)
	code, out, errOut := run("state", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, leak := range []string{pkey, secret, secret[:12]} {
		if strings.Contains(out, leak) || strings.Contains(errOut, leak) {
			t.Fatalf("the key is in the debug state:\n%s", out)
		}
	}
	var got map[string]json.RawMessage
	must(t, json.Unmarshal([]byte(out), &got))
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, want := range []string{"citations", "cursors", "live", "notices", "people", "project_keys", "sessions", "signals", "spool"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("no %q in %v", want, keys)
		}
	}
	var pks []map[string]any
	must(t, json.Unmarshal(got["project_keys"], &pks))
	if len(pks) != 1 || pks[0]["project"] != "pj_1" || pks[0]["member"] != "mb_1" {
		t.Fatalf("project_keys %v", pks)
	}
	if _, has := pks[0]["key"]; has {
		t.Fatal("project_keys carries a key field")
	}
	var state State
	must(t, json.Unmarshal([]byte(out), &state))
	if len(state.Sessions) != 1 || state.Sessions[0].ID != "s1" || state.Sessions[0].Project != "pj_1" ||
		len(state.Sessions[0].Topics) != 1 {
		t.Fatalf("sessions %+v", state.Sessions)
	}
	if len(state.Notices) != 1 || state.Notices[0].Session != "s1" || state.Notices[0].Type != "DRIFT_FOUND" || state.Notices[0].Shown {
		t.Fatalf("notices %+v", state.Notices)
	}
	if len(state.Spool) != 1 || state.Spool[0].Signal != "sg_2" || !strings.Contains(string(state.Spool[0].Params), `"NOTE"`) {
		t.Fatalf("spool %+v", state.Spool)
	}
	if len(state.Citations) != 1 || state.Citations[0].Signal != "sg_1" {
		t.Fatalf("citations %+v", state.Citations)
	}
	if len(state.Signals) != 1 || state.Signals[0].Signal.ID != "sg_1" || state.Signals[0].State != protocol.SignalStateInForce {
		t.Fatalf("signals %+v", state.Signals)
	}
	if state.Cursors["pj_1"] != 7 || state.People["mb_1"].Name != "Lan" || state.Live["pj_1"] == "" {
		t.Fatalf("cursors %v people %v live %v", state.Cursors, state.People, state.Live)
	}
}

func TestDebugStateOfAnEmptyHome(t *testing.T) {
	t.Setenv("TEAMFLOW_HOME", filepath.Join(t.TempDir(), "home"))
	code, out, errOut := run("state", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{`"sessions": []`, `"notices": []`, `"spool": []`, `"cursors": {}`, `"project_keys": []`} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %s in\n%s", want, out)
		}
	}
}

func TestDebugRefusesOtherForms(t *testing.T) {
	t.Setenv("TEAMFLOW_HOME", filepath.Join(t.TempDir(), "home"))
	for _, args := range [][]string{{}, {"state"}, {"state", "--yaml"}, {"frob", "--json"}, {"state", "--json", "extra"}} {
		code, out, errOut := run(args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "usage: teamflow debug state --json") {
			t.Fatalf("%v: exit %d, out %q, err %q", args, code, out, errOut)
		}
	}
}

func TestDebugRefusesAnOpenHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.Chmod(dir, 0o755))
	t.Setenv("TEAMFLOW_HOME", dir)
	code, out, errOut := run("state", "--json")
	if code != 1 || out != "" || !strings.Contains(errOut, "open to other users") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, store.FileName)); err == nil {
		t.Fatal("a store was made in an open home")
	}
}

// TestDebugStateShowsCwdFromHome: a session's cwd under the home reads "~/…", so no real home path is printed.
func TestDebugStateShowsCwdFromHome(t *testing.T) {
	user := filepath.Join(t.TempDir(), "someone")
	t.Setenv("HOME", user)
	dir := filepath.Join(t.TempDir(), "home")
	t.Setenv("TEAMFLOW_HOME", dir)
	must(t, os.MkdirAll(dir, 0o700))
	st, err := store.Open(dir)
	must(t, err)
	cwds := map[string]string{
		"in":      filepath.Join(user, "work", "pricing"),
		"home":    user,
		"slash":   user + "/",
		"sibling": user + "else/work", // shares a prefix only: not under the home
		"outside": "/srv/work",
		"empty":   "",
	}
	for id, cwd := range cwds {
		_, err := st.UpsertSession(id, "agent", 1, cwd, t0)
		must(t, err)
	}
	must(t, st.Close())
	code, out, errOut := run("state", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, filepath.Join(user, "work")) || strings.Contains(out, `"`+user+`"`) {
		t.Fatalf("a real home path is printed:\n%s", out)
	}
	var state State
	must(t, json.Unmarshal([]byte(out), &state))
	want := map[string]string{"in": "~/work/pricing", "home": "~", "slash": "~", "sibling": user + "else/work",
		"outside": "/srv/work", "empty": ""}
	if len(state.Sessions) != len(want) {
		t.Fatalf("sessions %+v", state.Sessions)
	}
	for _, s := range state.Sessions {
		if s.Cwd != want[s.ID] {
			t.Errorf("%s: cwd %q, want %q", s.ID, s.Cwd, want[s.ID])
		}
	}

	t.Setenv("HOME", "") // no home known: paths are left as they are, never an error
	if code, _, errOut := run("state", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}
