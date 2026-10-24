//go:build unix

package macros

import (
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
	if got := Open(path, "box").Load(); !strings.Contains(got.Error, "is not a regular file") {
		t.Errorf("a FIFO: %+v", got)
	}
	if got := Open(filepath.Join(path, "x"), "box").Load(); got.Error == "" {
		t.Errorf("a path under a FIFO read as no file")
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
	s := Open(filepath.Join(parent, "robin", "macros.json"), "box")
	if got := s.Change(put(macro("a", "A"))); !strings.Contains(got.Error, "permission denied") || len(got.List) != 0 {
		t.Errorf("a config dir that cannot be made: %+v", got)
	}

	// A rename that fails takes its temp file with it.
	dir := t.TempDir()
	target := filepath.Join(dir, "macros.json")
	if err := os.MkdirAll(filepath.Join(target, "full"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Open(target, "box").write(nil); err == nil {
		t.Errorf("a rename over a directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %v", entries)
	}
}
