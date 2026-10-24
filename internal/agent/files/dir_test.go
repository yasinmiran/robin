//go:build linux

package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadDir(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config")
	os.MkdirAll(config, 0o700)
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "user-dirs.dirs"), []byte(`XDG_DOWNLOAD_DIR="/srv/dl"`+"\n"), 0o600)

	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(config, "user-dirs.dirs"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := DownloadDir(home, env(nil)), filepath.Join(home, "Downloads"); got != want {
		t.Errorf("with nothing set: %q, want %q", got, want)
	}
	write("# written by xdg-user-dirs-update\nXDG_DESKTOP_DIR=\"$HOME/Desktop\"\nXDG_DOWNLOAD_DIR=\"$HOME/Telechargements\"\n")
	if got, want := DownloadDir(home, env(nil)), filepath.Join(home, "Telechargements"); got != want {
		t.Errorf("from user-dirs.dirs: %q, want %q", got, want)
	}
	write("XDG_DOWNLOAD_DIR=\"/data/downloads/\"\n")
	if got := DownloadDir(home, env(nil)); got != "/data/downloads" {
		t.Errorf("absolute in user-dirs.dirs: %q", got)
	}
	// xdg-user-dirs-update points a folder that is gone at $HOME itself,
	// where an upload could plant a startup file.
	for _, dir := range []string{"$HOME", "$HOME/", home, home + "/."} {
		write("XDG_DOWNLOAD_DIR=\"" + dir + "\"\n")
		if got, want := DownloadDir(home, env(nil)), filepath.Join(home, "Downloads"); got != want {
			t.Errorf("%s in user-dirs.dirs: %q, want %q", dir, got, want)
		}
		if got, want := DownloadDir(home, env(map[string]string{"XDG_DOWNLOAD_DIR": dir})), filepath.Join(home, "Downloads"); got != want {
			t.Errorf("XDG_DOWNLOAD_DIR=%s: %q, want %q", dir, got, want)
		}
	}
	write("XDG_DOWNLOAD_DIR=\"$HOMEWARD/x\"\nXDG_DOWNLOAD_DIR=relative\n")
	if got, want := DownloadDir(home, env(nil)), filepath.Join(home, "Downloads"); got != want {
		t.Errorf("unusable user-dirs.dirs: %q, want %q", got, want)
	}
	if got := DownloadDir(home, env(map[string]string{"XDG_CONFIG_HOME": other})); got != "/srv/dl" {
		t.Errorf("XDG_CONFIG_HOME's user-dirs.dirs: %q", got)
	}
	if got := DownloadDir(home, env(map[string]string{"XDG_DOWNLOAD_DIR": "$HOME/dl", "XDG_CONFIG_HOME": other})); got != filepath.Join(home, "dl") {
		t.Errorf("XDG_DOWNLOAD_DIR in the environment: %q", got)
	}
	if got, want := DownloadDir(home, env(map[string]string{"XDG_DOWNLOAD_DIR": "rel"})), filepath.Join(home, "Downloads"); got != want {
		t.Errorf("relative XDG_DOWNLOAD_DIR: %q, want it ignored for %q", got, want)
	}
}
