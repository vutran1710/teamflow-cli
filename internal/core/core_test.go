package core

import (
	"context"
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
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := open(t, &config.Config{Server: dead.URL, AuthKey: "tfa_x"})
	if err := c.St.PutProjectKey(P, "tfk_x", "mb", "og", "n", time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.Sync(ctx, time.Second, "") {
		t.Fatal("offline must say so")
	}
	if _, _, _, _, ok := c.St.ProjectKey(P); !ok {
		t.Fatal("offline drops no key")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "gone", "data": {"reason": "NOT_A_MEMBER"}}}`))
	}))
	defer srv.Close()
	c.Cfg.Server = srv.URL
	if !c.Sync(ctx, time.Second, "") {
		t.Fatal("a refusal is not offline")
	}
	if _, _, _, _, ok := c.St.ProjectKey(P); ok {
		t.Fatal("a refused key is dropped")
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
