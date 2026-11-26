package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingIsNil(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil || cfg != nil {
		t.Fatalf("Load of an empty home: %v, %v; want nil, nil", cfg, err)
	}
}

func TestSaveIsOwnerOnlyAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Config{Server: "https://tf.example", AuthKey: "tfa_secret", Email: "lan@acme.test"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %o, want 600", fi.Mode().Perm())
	}
	cfg, err := Load(dir)
	if err != nil || cfg == nil || *cfg != (Config{Server: "https://tf.example", AuthKey: "tfa_secret", Email: "lan@acme.test"}) {
		t.Fatalf("Load: %+v, %v", cfg, err)
	}
	if !cfg.SignedIn() {
		t.Fatal("a config with an auth key is signed in")
	}
}

func TestSaveTightensAnOpenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"server":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, Config{Server: "https://tf.example"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o after Save, want 600", fi.Mode().Perm())
	}
}

// Python's config.save keeps unknown keys but drops the old member "key".
func TestSaveKeepsOtherKeysDropsOldMemberKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"server":"x","key":"tfm_old","theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, Config{Server: "https://tf.example"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["key"]; ok || got["theme"] != "dark" || got["server"] != "https://tf.example" || got["auth_key"] != "" {
		t.Fatalf("saved %v", got)
	}
}

func TestSignedOutConfig(t *testing.T) {
	if (&Config{Server: "x"}).SignedIn() {
		t.Fatal("no auth key: signed out")
	}
	var nilCfg *Config
	if nilCfg.SignedIn() {
		t.Fatal("no config: signed out")
	}
}

func TestBrokenConfigIsAnErrorWithoutItsText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"auth_key":"tfa_secret",`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || strings.Contains(err.Error(), "tfa_secret") {
		t.Fatalf("broken config: %v", err)
	}
}

func TestStringMasksTheKey(t *testing.T) {
	c := Config{Server: "https://tf.example", AuthKey: "tfa_secret", Email: "a@b"}
	for _, s := range []string{c.String(), (&c).String()} {
		if strings.Contains(s, "secret") {
			t.Fatalf("String shows the key: %s", s)
		}
	}
}

func TestDefaultServer(t *testing.T) {
	if DefaultServer != "https://teamflow.10l.ink" {
		t.Fatal(DefaultServer)
	}
}

// Save replaces the file in one step (a temp file renamed over it): a reader never sees it half written, and no
// temp file is left behind.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"server":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := Save(dir, Config{Server: "https://tf.example", AuthKey: "tfa_x"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if os.SameFile(before, after) {
		t.Fatal("config.json was rewritten in place, not replaced")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

// A Save interrupted before its rename leaves a temp file beside the last good config.json: the next process loads
// the good one, and the next Save still works.
func TestInterruptedSaveLeavesTheLastGoodConfig(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Config{Server: "https://tf.example", AuthKey: "tfa_good", Email: "a@b"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".config-123456.json"), []byte(`{"server": "https://other", "auth_k`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil || cfg == nil || cfg.AuthKey != "tfa_good" || cfg.Server != "https://tf.example" {
		t.Fatalf("after an interrupted save: %v %v", cfg, err)
	}
	if err := Save(dir, Config{Server: "https://tf.example", AuthKey: "tfa_next", Email: "a@b"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(dir); cfg.AuthKey != "tfa_next" {
		t.Fatalf("the next save: %v", cfg)
	}
}

// The scheduled self-update is on by default; `auto_update: false` or the env turns it off. SetAutoUpdate keeps
// the sign-in and every other key; a login (Save) keeps the setting; a broken config.json is refused, never
// replaced. Sad paths: an env set to an unknown word (on), a config with auto_update not a boolean (on).
func TestAutoUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(AutoUpdateEnv, "")
	if on, why := AutoUpdate(nil); !on || why != "" {
		t.Fatalf("no config: %v %q", on, why)
	}
	if err := Save(dir, Config{Server: "https://tf.example", AuthKey: "tfa_secret", Email: "lan@acme.test"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(dir)
	if on, _ := AutoUpdate(cfg); !on || cfg.AutoUpdate != nil {
		t.Fatalf("default: %v %v", on, cfg.AutoUpdate)
	}
	if err := SetAutoUpdate(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(dir)
	if on, why := AutoUpdate(cfg); on || !strings.Contains(why, "teamflow config auto-update off") || cfg.AuthKey != "tfa_secret" {
		t.Fatalf("off: %v %q %+v", on, why, cfg)
	}
	// A new sign-in keeps the setting.
	if err := Save(dir, Config{Server: "https://tf.example", AuthKey: "tfa_other", Email: "lan@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = Load(dir); cfg.AutoUpdate == nil || *cfg.AutoUpdate {
		t.Fatalf("a login dropped auto_update: %+v", cfg)
	}
	if err := SetAutoUpdate(dir, true); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(dir)
	if on, _ := AutoUpdate(cfg); !on {
		t.Fatal("on again")
	}
	for _, v := range []string{"0", "false", "OFF", " no "} {
		t.Setenv(AutoUpdateEnv, v)
		if on, why := AutoUpdate(cfg); on || !strings.HasPrefix(why, AutoUpdateEnv+"=") {
			t.Fatalf("env %q: %v %q", v, on, why)
		}
	}
	t.Setenv(AutoUpdateEnv, "1")
	if on, _ := AutoUpdate(cfg); !on {
		t.Fatal("env 1 turned it off")
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"auth_key": "tfa_x", "auto_update": "no"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = Load(dir); cfg.AutoUpdate != nil {
		t.Fatalf("a non-boolean auto_update was read: %v", *cfg.AutoUpdate)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetAutoUpdate(dir, false); err == nil {
		t.Fatal("a broken config.json was replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, FileName)); string(b) != `{broken` {
		t.Fatalf("config.json changed: %s", b)
	}
	fresh := t.TempDir()
	if err := SetAutoUpdate(fresh, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(fresh, FileName)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}
