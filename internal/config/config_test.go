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
	if DefaultServer != "https://teamflow.vutr.io" {
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
