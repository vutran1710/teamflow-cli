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
	"strings"

	"github.com/vutran1710/teamflow-cli/internal/api"
)

// FileName is the config file in the Teamflow home.
const FileName = "config.json"

// DefaultServer is the server used when none is given and none is configured.
const DefaultServer = "https://teamflow.10l.ink"

// Config is what config.json holds.
type Config struct {
	Server  string `json:"server"`
	AuthKey string `json:"auth_key"` // tfa_…, from signing in; empty when signed out
	Email   string `json:"email"`
	// AutoUpdate: false turns off the background sync's scheduled self-update (nil: on). Set with
	// `teamflow config auto-update off|on` (SetAutoUpdate); Save keeps it as it is in the file.
	AutoUpdate *bool `json:"auto_update,omitempty"`
}

// AutoUpdateEnv set to 0 (or false, off, no) turns the scheduled self-update off, whatever config.json says.
const AutoUpdateEnv = "TEAMFLOW_AUTO_UPDATE"

// AutoUpdate says whether the background sync updates teamflow on its own; when it does not, why: the env, or the
// config (c nil: no config yet, on).
func AutoUpdate(c *Config) (on bool, why string) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AutoUpdateEnv))) {
	case "0", "false", "off", "no":
		return false, AutoUpdateEnv + "=" + os.Getenv(AutoUpdateEnv)
	}
	if c != nil && c.AutoUpdate != nil && !*c.AutoUpdate {
		return false, "turned off with `teamflow config auto-update off`"
	}
	return true, ""
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
	cfg := &Config{Server: str("server"), AuthKey: str("auth_key"), Email: str("email")}
	if on, ok := m["auto_update"].(bool); ok {
		cfg.AutoUpdate = &on
	}
	return cfg, nil
}

// SetAutoUpdate turns the scheduled self-update on or off in dir/config.json, keeping everything else in it (a
// config.json that can't be read is refused, never replaced: it holds the sign-in).
func SetAutoUpdate(dir string, on bool) error {
	m, err := read(dir)
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]any{}
	}
	m["auto_update"] = on
	return write(dir, m)
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
	if cfg.AutoUpdate != nil {
		m["auto_update"] = *cfg.AutoUpdate
	}
	return write(dir, m)
}

// write writes m to dir/config.json (0600), atomically.
func write(dir string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A temp file at 0600, renamed over config.json: hooks and the background sync never read it half written.
	f, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("write %s: %w", FileName, errors.Unwrap(err))
	}
	tmp := f.Name()
	defer os.Remove(tmp) // gone once renamed; removes it after a failure
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, FileName)); err != nil {
		return err
	}
	// The rename is durable once the folder is synced.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
