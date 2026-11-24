package adapters

// Writing an agent's config files: in place of the file a link points to (dotfiles kept elsewhere), atomically (a
// temp file renamed over it, never more open than the file), keeping its mode, and never over a file the person
// made read-only.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/vutran1710/teamflow-cli/internal/home"
)

// target is where writing path lands: the file a link points to, else path itself.
func target(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s is a link to nothing: fix or remove it first", home.Display(path))
	}
	return real, err
}

// ReadFile is a config file's bytes (nil when it does not exist). A folder at the path is refused.
func ReadFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file", home.Display(path))
	}
	return os.ReadFile(path)
}

// Writable says why path can't be written ("" when it can): a read-only file, or not a file.
func Writable(path string) error {
	t, err := target(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(t)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", home.Display(path))
	}
	if fi.Mode().Perm()&0o200 == 0 || unix.Access(t, unix.W_OK) != nil {
		return fmt.Errorf("%s is read-only: make it writable, then run the install again", home.Display(path))
	}
	return nil
}

// WriteFile replaces path's content (the file a link points to), atomically, keeping its mode (perm for a new
// file, its folders made 0755). An unchanged content is not written at all.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := Writable(path); err != nil {
		return err
	}
	t, err := target(path)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(t); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if fi, err := os.Stat(t); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(t), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(t), "."+filepath.Base(t)+".teamflow-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm & 0o600); err != nil { // never more open than the file while being written
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), t)
}
