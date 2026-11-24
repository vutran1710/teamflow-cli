package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/config"
	"github.com/vutran1710/teamflow-cli/internal/protocol"
	"github.com/vutran1710/teamflow-cli/internal/store"
)

const P = "pj_00000000000000000000000001"

func open(t *testing.T, cfg *config.Config) *Core {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMFLOW_HOME", dir)
	if cfg != nil {
		if err := config.Save(dir, *cfg); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSignedOutRefusesBeforeSending(t *testing.T) {
	ctx := context.Background()
	c := open(t, nil)
	if _, err := c.AuthAPI(time.Second); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("AuthAPI: %v", err)
	}
	if _, err := c.API(ctx, P, time.Second); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("API: %v", err)
	}
	if _, err := c.Projects(ctx); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Projects: %v", err)
	}
	if _, err := c.Verdict(ctx, "", "sg_x", protocol.SignalKindApprove, "", protocol.ActorMember); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Verdict: %v", err)
	}
	if !c.Sync(ctx, time.Second, "") {
		t.Fatal("nothing to sync is not offline")
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatalf("Logout signed out: %v", err)
	}
}

func TestVerdictTakesOnlyVerdictsAndRejectsNeedAReason(t *testing.T) {
	ctx := context.Background()
	c := open(t, &config.Config{Server: "http://127.0.0.1:1", AuthKey: "tfa_x"})
	if _, err := c.Verdict(ctx, "", "sg_x", protocol.SignalKind("DECISION"), "", protocol.ActorMember); err == nil ||
		!strings.Contains(err.Error(), "not a verdict") {
		t.Fatalf("a DECISION is not a verdict: %v", err)
	}
	if _, err := c.Verdict(ctx, "", "sg_x", protocol.SignalKindReject, " ", protocol.ActorMember); err == nil ||
		!strings.Contains(err.Error(), "says why") {
		t.Fatalf("a reject without a reason: %v", err)
	}
}

// Offline, Sync says so and keeps the project key; a key refused as NOT_A_MEMBER is dropped.
func TestSyncOfflineKeepsTheKeyRefusedDropsIt(t *testing.T) {
	ctx := context.Background()
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !refuse {
			w.WriteHeader(http.StatusServiceUnavailable) // 5xx: offline
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "gone", "data": {"reason": "NOT_A_MEMBER"}}}`))
	}))
	defer srv.Close()
	c := open(t, &config.Config{Server: srv.URL, AuthKey: "tfa_x", Email: "lan@acme.test"})
	b := mustBinding(t, c)
	if err := c.St.PutProjectKey(b, P, "tfk_x", "mb", "og", "n", time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.Sync(ctx, time.Second, "") {
		t.Fatal("offline must say so")
	}
	if _, _, _, _, ok := c.St.ProjectKey(b, P); !ok {
		t.Fatal("offline drops no key")
	}
	refuse = true
	if !c.Sync(ctx, time.Second, "") {
		t.Fatal("a refusal is not offline")
	}
	if _, _, _, _, ok := c.St.ProjectKey(b, P); ok {
		t.Fatal("a refused key is dropped")
	}
}

// A process opened before a sign-in elsewhere still holds the old config: the key it fetches from the old server
// is written under the old binding, and the process with the new config never sends it to the new server.
func TestStaleProcessKeyNeverReachesTheNewSignIn(t *testing.T) {
	ctx := context.Background()
	answer := func(key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "result": {"key": "` + key + `", "project": "` + P + `", "org": "og", "member": "mb", "name": "n"}}`))
		}))
	}
	a, b := answer("tfk_from_a"), answer("tfk_from_b")
	defer a.Close()
	defer b.Close()
	old := open(t, &config.Config{Server: a.URL, AuthKey: "tfa_a", Email: "lan@acme.test"})
	// Meanwhile another process signs in to server B.
	if err := config.Save(old.Dir, config.Config{Server: b.URL, AuthKey: "tfa_b", Email: "lan@acme.test"}); err != nil {
		t.Fatal(err)
	}
	// The stale process fetches a key from A after the switch.
	if k, err := old.ProjectKey(ctx, P); err != nil || k != "tfk_from_a" {
		t.Fatalf("stale fetch: %q %v", k, err)
	}
	fresh, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if ps, _ := fresh.KeyProjects(); len(ps) != 0 {
		t.Fatalf("the new sign-in sees A's key: %v", ps)
	}
	if k, err := fresh.ProjectKey(ctx, P); err != nil || k != "tfk_from_b" {
		t.Fatalf("the new sign-in fetches its own key: %q %v", k, err)
	}
}

// The same server written another way (case of the host, a trailing slash) is the same sign-in: its keys stay.
func TestLoginToTheSameServerWrittenDifferentlyKeepsKeys(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "result": {"auth_key": "tfa_y", "account": {"id": "ac", "email": "lan@acme.test", "name": "lan", "alias": "", "avatar": null, "created_at": "2026-01-15T09:00:00Z", "disabled_at": null}, "expires_at": "2026-01-15T09:00:01Z"}}`))
	}))
	defer srv.Close()
	c := open(t, &config.Config{Server: srv.URL, AuthKey: "tfa_x", Email: "lan@acme.test"})
	b, err := c.binding()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.St.PutProjectKey(b, P, "tfk_x", "mb", "og", "n", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Login(ctx, strings.Replace(srv.URL, "http://", "HTTP://", 1)+"/", "LAN@acme.test", "pw"); err != nil {
		t.Fatal(err)
	}
	if c.Cfg.Server != srv.URL {
		t.Fatalf("the server is saved in one form: %q", c.Cfg.Server)
	}
	if k, err := c.ProjectKey(ctx, P); err != nil || k != "tfk_x" {
		t.Fatalf("the key stays: %q %v", k, err)
	}
}

// No config, or one without a server or email: no binding, so no project key at all, through every accessor, and
// nothing is sent.
func TestNoBindingIsNotSignedIn(t *testing.T) {
	ctx := context.Background()
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent++ }))
	defer srv.Close()
	for _, cfg := range []*config.Config{nil, {}, {Server: "https://a.example"}, {AuthKey: "tfa_x", Email: "a@b"},
		{Server: "  ", AuthKey: "tfa_x", Email: "a@b"}, {Server: srv.URL, AuthKey: "tfa_x", Email: "   "}} {
		c := open(t, cfg)
		if _, err := c.API(ctx, P, time.Second); err == nil {
			t.Fatalf("API with %v", cfg)
		}
		if !c.Sync(ctx, time.Second, "") || !c.Sync(ctx, time.Second, P) {
			t.Fatalf("Sync with %v: nothing to sync is not offline", cfg)
		}
		if _, err := c.binding(); !errors.Is(err, ErrNotSignedIn) {
			t.Fatalf("binding with %v: %v", cfg, err)
		}
		if _, err := c.KeyProjects(); !errors.Is(err, ErrNotSignedIn) {
			t.Fatalf("KeyProjects with %v: %v", cfg, err)
		}
		if _, err := c.ProjectKey(ctx, P); !errors.Is(err, ErrNotSignedIn) {
			t.Fatalf("ProjectKey with %v: %v", cfg, err)
		}
	}
}

// A config.json that can't be read or parsed stops the client opening, says so without its text (it holds the
// key), and no binding is ever made from it.
func TestUnreadableConfigNeverMakesABinding(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"corrupt", `{"server": "https://a.example", "auth_key": "tfa_secret", "email": `},
		{"not an object", `["tfa_secret"]`},
		{"unreadable", `{"server": "https://a.example", "auth_key": "tfa_secret", "email": "a@b"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEAMFLOW_HOME", dir)
			path := filepath.Join(dir, config.FileName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.name == "unreadable" {
				if os.Getuid() == 0 {
					t.Skip("root reads anything")
				}
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatal(err)
				}
			}
			c, err := Open()
			if err == nil {
				c.Close()
				t.Fatal("opened with a config it could not read")
			}
			if strings.Contains(err.Error(), "tfa_secret") {
				t.Fatalf("the error shows the key: %v", err)
			}
		})
	}
	// A config whose values are wrong types reads as empty fields: no binding.
	c := open(t, nil)
	if err := os.WriteFile(filepath.Join(c.Dir, config.FileName), []byte(`{"server": 7, "email": ["a@b"], "auth_key": "tfa_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Cfg = cfg
	if _, err := c.binding(); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("binding from wrong-typed values: %v", err)
	}
}

// Logging in with a malformed server is refused before anything is saved or forgotten.
func TestLoginToAMalformedServerChangesNothing(t *testing.T) {
	ctx := context.Background()
	c := open(t, &config.Config{Server: "https://a.example", AuthKey: "tfa_x", Email: "lan@acme.test"})
	b := mustBinding(t, c)
	if err := c.St.PutProjectKey(b, P, "tfk_x", "mb", "og", "n", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, server := range []string{"", "teamflow.example", "::garbage::", "http://a.example", "ftp://a.example"} {
		if _, err := c.Login(ctx, server, "lan@acme.test", "pw"); err == nil {
			t.Fatalf("Login(%q) succeeded", server)
		}
	}
	if cfg, _ := config.Load(c.Dir); cfg == nil || cfg.Server != "https://a.example" || cfg.AuthKey != "tfa_x" {
		t.Fatalf("config changed: %v", cfg)
	}
	if _, _, _, _, ok := c.St.ProjectKey(b, P); !ok {
		t.Fatal("a refused login forgot the keys")
	}
}

// After a reboot or restart nothing is in memory: a fresh Core opened from the same home finds the sign-in (server,
// auth key, email) and every bound project key, and uses them without fetching anything again. No credential lives
// only in memory: the auth key is in config.json, project keys in store.db.
func TestRestartReloadsEveryCredential(t *testing.T) {
	ctx := context.Background()
	fetches := 0
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Project string `json:"project"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "auth.login":
			_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "result": {"auth_key": "tfa_saved", "account": {"id": "ac", "email": "Lan@Acme.test", "name": "lan", "alias": "", "avatar": null, "created_at": "2026-01-15T09:00:00Z", "disabled_at": null}, "expires_at": "2026-01-15T09:00:01Z"}}`))
		case "key.fetch":
			fetches++
			if r.Header.Get("Authorization") != "Bearer tfa_saved" {
				t.Errorf("key.fetch with %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "result": {"key": "tfk_` + req.Params.Project + `", "project": "` + req.Params.Project + `", "org": "og", "member": "mb", "name": "n"}}`))
		case "project.get":
			if r.Header.Get("Authorization") != "Bearer tfk_"+P {
				t.Errorf("project.get with %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "result": {}}`))
		default:
			t.Errorf("unexpected %s", req.Method)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	first := open(t, nil)
	if _, err := first.Login(ctx, strings.ToUpper(srvURL[:4])+srvURL[4:]+"/", "lan@acme.test", "pw"); err != nil {
		t.Fatal(err)
	}
	const P2 = "pj_00000000000000000000000002"
	for _, p := range []string{P, P2} {
		if _, err := first.ProjectKey(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	first.Close()
	if fetches != 2 {
		t.Fatalf("fetches %d", fetches)
	}

	// The restart: a new process, the same home.
	again, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !again.SignedIn() || again.Cfg.Server != srvURL || again.Cfg.AuthKey != "tfa_saved" || again.Cfg.Email != "Lan@Acme.test" {
		t.Fatalf("the sign-in after a restart: %v", again.Cfg)
	}
	ps, err := again.KeyProjects()
	if err != nil || len(ps) != 2 {
		t.Fatalf("keys after a restart: %v %v", ps, err)
	}
	for _, p := range []string{P, P2} {
		if k, err := again.ProjectKey(ctx, p); err != nil || k != "tfk_"+p {
			t.Fatalf("key of %s after a restart: %q %v", p, k, err)
		}
	}
	a, err := again.API(ctx, P, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Call(ctx, "project.get", protocol.ProjectGet{Project: P}, nil); err != nil {
		t.Fatalf("the reloaded key is usable: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("a restart fetched keys again (%d)", fetches)
	}
}

func TestLoginKeepsNoPasswordAndRefusalSavesNothing(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "wrong email or password", "data": {"reason": "NOT_ALLOWED"}}}`))
	}))
	defer srv.Close()
	c := open(t, nil)
	_, err := c.Login(ctx, srv.URL, "a@b", "pw")
	var ae *api.APIError
	if !errors.As(err, &ae) || ae.Message != "wrong email or password" {
		t.Fatalf("Login: %v", err)
	}
	if cfg, _ := config.Load(c.Dir); cfg != nil {
		t.Fatalf("a refused login saved %v", cfg)
	}
}

func TestPyRepr(t *testing.T) {
	for in, want := range map[string]string{"Nope": "'Nope'", "it's": `"it's"`, `a'"b`: `'a\'"b'`} {
		if got := pyRepr(in); got != want {
			t.Errorf("pyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}

func mustBinding(t *testing.T, c *Core) store.KeyBinding {
	t.Helper()
	b, err := c.binding()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
