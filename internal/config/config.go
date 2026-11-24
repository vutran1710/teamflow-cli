// Package config is the port of client/tf_client/config.py: which server this machine uses, and the auth key and
// email from `teamflow login`, in config.json (mode 0600) in the Teamflow home. The password is never kept.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/vutran1710/teamflow-cli/internal/api"
)

// FileName is the config file in the Teamflow home.
const FileName = "config.json"

// DefaultServer is the server used when none is given and none is configured.
const DefaultServer = "https://teamflow.vutr.io"

// Config is what config.json holds.
type Config struct {
	Server  string `json:"server"`
	AuthKey string `json:"auth_key"` // tfa_…, from signing in; empty when signed out
	Email   string `json:"email"`
}

// SignedIn says whether this machine holds an auth key.
func (c *Config) SignedIn() bool { return c != nil && c.AuthKey != "" }

// String shows the config with the auth key masked.
func (c Config) String() string {
	key := "none"
	if c.AuthKey != "" {
		key = "tfa_***"
	}
	return fmt.Sprintf("config{server: %s, email: %s, auth_key: %s}", c.Server, c.Email, key)
}

// KeyRef names a key without revealing it (Python's config.key_ref).
func KeyRef(key string) string { return api.KeyRef(key) }

func read(dir string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", FileName, errors.Unwrap(err))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: run `teamflow login` again", FileName) // never its text: it holds the key
	}
	return m, nil
}

// Load reads config.json from dir; nil (and no error) when there is none.
func Load(dir string) (*Config, error) {
	m, err := read(dir)
	if err != nil || m == nil {
		return nil, err
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	return &Config{Server: str("server"), AuthKey: str("auth_key"), Email: str("email")}, nil
}

// Save writes cfg to dir/config.json with mode 0600 (tightened if it was open), keeping keys it does not know
// but dropping the old member "key".
func Save(dir string, cfg Config) error {
	m, err := read(dir)
	if err != nil {
		m = nil // a broken file is replaced
	}
	if m == nil {
		m = map[string]any{}
	}
	delete(m, "key")
	m["server"], m["auth_key"], m["email"] = cfg.Server, cfg.AuthKey, cfg.Email
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", FileName, errors.Unwrap(err))
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
