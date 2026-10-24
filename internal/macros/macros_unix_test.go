//go:build unix

package macros

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOnlyARegularFileIsRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "macros.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
	if _, err := New(path, "box").Load(); !errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), "(not a regular file)") {
		t.Errorf("a FIFO: %v", err)
	}
	if _, err := New(filepath.Join(path, "x"), "box").Load(); err == nil {
		t.Errorf("a path under a FIFO read as no file")
	}
}

func TestAFileThatCannotBeOpenedIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory it may not search")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "macros.json")
	body, _ := encode(nil)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	_, err := New(path, "box").Load()
	if !errors.Is(err, ErrUnreadable) || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "(permission denied). It leaves the file alone") {
		t.Errorf("Load: %v", err)
	}
}

func TestAFailedWriteLeavesNoTemp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	s := New(filepath.Join(parent, "robin", "macros.json"), "box")
	if got, err := s.Change(put(macro("a", "A"))); err == nil || !strings.Contains(err.Error(), "permission denied") || len(got) != 0 {
		t.Errorf("a config dir that cannot be made: %+v, %v", got, err)
	}

	// A rename that fails takes its temp file with it.
	dir := t.TempDir()
	target := filepath.Join(dir, "macros.json")
	if err := os.MkdirAll(filepath.Join(target, "full"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(target, "box").write(nil); err == nil {
		t.Errorf("a rename over a directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %v", entries)
	}
}
