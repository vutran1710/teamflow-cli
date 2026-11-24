// Package home finds and guards the Teamflow home folder: ~/.teamflow, or $TEAMFLOW_HOME (which is how tests and
// two people on one machine keep apart). It holds the client store with the project keys, so it must be readable
// by its owner only.
package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Dir is the home folder: $TEAMFLOW_HOME, else ~/.teamflow.
func Dir() string {
	if d := os.Getenv("TEAMFLOW_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		h = "."
	}
	return filepath.Join(h, ".teamflow")
}

// Ensure creates the home folder (mode 0700) when it is missing. An existing one must be a folder of this user that
// no group or other user can open; Ensure refuses it otherwise, and says how to fix it, rather than fix it itself.
func Ensure() (string, error) {
	d := Dir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("teamflow home %s: %w", Display(d), err)
	}
	fi, err := os.Stat(d)
	if err != nil {
		return "", fmt.Errorf("teamflow home %s: %w", Display(d), err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("teamflow home %s is not a folder", Display(d))
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("teamflow home %s belongs to another user; set TEAMFLOW_HOME to a folder of your own",
			Display(d))
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("teamflow home %s is open to other users (mode %o): it holds your keys; run: chmod 700 %s",
			Display(d), perm, Display(d))
	}
	return d, nil
}

// Display names a path for messages: under the user's home it starts with ~, so no username or home path is shown.
func Display(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" || h == "/" {
		return p
	}
	if p == h {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, h+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}
