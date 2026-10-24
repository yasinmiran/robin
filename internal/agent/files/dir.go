//go:build linux

package files

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DownloadDir is where uploads land: the XDG download directory, from the
// environment or from user-dirs.dirs as xdg-user-dirs writes it, else
// ~/Downloads. A download directory that is home itself counts as unset:
// xdg-user-dirs-update points one it cannot find there, and uploads must
// not land beside the shell's startup files. getenv reads the environment.
func DownloadDir(home string, getenv func(string) string) string {
	usable := func(dir string) bool { return dir != "" && dir != filepath.Clean(home) }
	if dir := expand(getenv("XDG_DOWNLOAD_DIR"), home); usable(dir) {
		return dir
	}
	config := getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	if dir := userDir(filepath.Join(config, "user-dirs.dirs"), "XDG_DOWNLOAD_DIR", home); usable(dir) {
		return dir
	}
	return filepath.Join(home, "Downloads")
}

// userDir reads one variable from a user-dirs.dirs file, whose lines are
// shell assignments of a quoted "$HOME/path" or "/path".
func userDir(file, name, home string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(s.Text()), "=")
		if !ok || key != name {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		return expand(value, home)
	}
	return ""
}

// expand resolves a leading $HOME and keeps only absolute results.
func expand(dir, home string) string {
	if rest, ok := strings.CutPrefix(dir, "$HOME"); ok && (rest == "" || rest[0] == '/') {
		dir = home + rest
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Clean(dir)
}
