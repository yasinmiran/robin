//go:build linux

package wayland

import (
	"os"
	"path/filepath"
	"strings"
)

// readToken returns the stored restore token, or "" when there is none.
func readToken(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// saveToken replaces the stored token, or removes it when the portal gave
// none. The token alone lets a later session see and drive the desktop
// without asking anyone, so only the user may read it, and it is renamed
// into place so no reader sees half of one.
func saveToken(path, token string) error {
	if token == "" {
		removeToken(path)
		return nil
	}
	if path == "" {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// removeToken forgets the stored token. A token is spent once a Start has
// used it, whatever the outcome.
func removeToken(path string) {
	if path != "" {
		os.Remove(path)
	}
}
