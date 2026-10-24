//go:build linux

package files

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testInbox(t *testing.T) *Inbox {
	t.Helper()
	in := NewInbox(filepath.Join(t.TempDir(), "Downloads"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(in.Close)
	return in
}

// listing is what the directory holds, sorted.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// upload sends data through a fresh upload in 64 KiB writes, as the bulk
// receiver would, and commits it.
func upload(t *testing.T, in *Inbox, name string, data []byte) (*Upload, error) {
	t.Helper()
	u, err := in.Accept(name, int64(len(data)))
	if err != nil {
		return nil, err
	}
	for rest := data; len(rest) > 0; {
		n := min(len(rest), 64<<10)
		if _, err := u.Write(rest[:n]); err != nil {
			u.Abort(err)
			return u, err
		}
		rest = rest[n:]
	}
	return u, u.Commit()
}

// waitEmpty waits for the inbox to have no uploads in progress, which is
// when an aborted upload's writer has removed its file.
func waitEmpty(t *testing.T, in *Inbox) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		in.mu.Lock()
		n := len(in.uploads)
		in.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d uploads still in progress", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUploadKeepsTheFile(t *testing.T) {
	in := testInbox(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 300_000) // 4.8 MB, many chunks
	u, err := upload(t, in, "report.pdf", data)
	if err != nil {
		t.Fatal(err)
	}
	if u.Saved() != "report.pdf" {
		t.Errorf("Saved() = %q", u.Saved())
	}
	got, err := os.ReadFile(filepath.Join(in.Dir(), "report.pdf"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("kept %d bytes (%v), want %d", len(got), err, len(data))
	}
	if names := listing(t, in.Dir()); len(names) != 1 {
		t.Errorf("directory holds %v, want only the upload", names)
	}
	st, _ := os.Stat(filepath.Join(in.Dir(), "report.pdf"))
	umask := syscall.Umask(0)
	syscall.Umask(umask)
	if want := os.FileMode(0o666 &^ umask); st.Mode().Perm() != want {
		t.Errorf("mode %v, want %v as for any new file", st.Mode().Perm(), want)
	}
	dir, _ := os.Stat(in.Dir())
	if dir.Mode().Perm() != 0o700 {
		t.Errorf("created the directory with mode %v, want 0700", dir.Mode().Perm())
	}
}

func TestUploadEmptyFile(t *testing.T) {
	in := testInbox(t)
	if _, err := upload(t, in, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(in.Dir(), "empty")); err != nil || st.Size() != 0 {
		t.Fatalf("empty upload: %v, %v", st, err)
	}
}

func TestUploadNeverReplaces(t *testing.T) {
	in := testInbox(t)
	os.MkdirAll(in.Dir(), 0o700)
	os.WriteFile(filepath.Join(in.Dir(), "a.txt"), []byte("mine"), 0o600)
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("precious"), 0o600)
	// A symlink in the way is a name taken, not a place to write.
	os.Symlink(outside, filepath.Join(in.Dir(), "a (1).txt"))

	for _, want := range []string{"a (2).txt", "a (3).txt"} {
		u, err := upload(t, in, "a.txt", []byte(want))
		if err != nil {
			t.Fatal(err)
		}
		if u.Saved() != want {
			t.Errorf("Saved() = %q, want %q", u.Saved(), want)
		}
	}
	for name, want := range map[string]string{"a.txt": "mine", "a (2).txt": "a (2).txt", "a (3).txt": "a (3).txt"} {
		if got, _ := os.ReadFile(filepath.Join(in.Dir(), name)); string(got) != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
	if got, _ := os.ReadFile(outside); string(got) != "precious" {
		t.Errorf("the symlink's target now holds %q", got)
	}
}

func TestUploadGivesUpOnNumbering(t *testing.T) {
	in := testInbox(t)
	os.MkdirAll(in.Dir(), 0o700)
	for n := 0; n <= maxNumbered; n++ {
		os.WriteFile(filepath.Join(in.Dir(), Numbered("x", n)), nil, 0o600)
	}
	if _, err := upload(t, in, "x", []byte("y")); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("upload with every name taken: %v", err)
	}
	waitEmpty(t, in)
	if n := len(listing(t, in.Dir())); n != maxNumbered+1 {
		t.Errorf("directory holds %d files, want the %d that were there", n, maxNumbered+1)
	}
}

func TestUploadWithoutRenameNoReplace(t *testing.T) {
	renameat2 = func(int, string, int, string, uint) error { return unix.EINVAL }
	t.Cleanup(func() { renameat2 = unix.Renameat2 })
	in := testInbox(t)
	os.MkdirAll(in.Dir(), 0o700)
	os.WriteFile(filepath.Join(in.Dir(), "b.bin"), []byte("old"), 0o600)
	u, err := upload(t, in, "b.bin", []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Saved() != "b (1).bin" {
		t.Errorf("Saved() = %q", u.Saved())
	}
	if names := listing(t, in.Dir()); strings.Join(names, ",") != "b (1).bin,b.bin" {
		t.Errorf("directory holds %v", names)
	}
}

func TestUploadRefusesBadNames(t *testing.T) {
	in := testInbox(t)
	for _, name := range []string{"../escape", "a/b", "..", "", "x\ny"} {
		if _, err := in.Accept(name, 1); !errors.Is(err, ErrName) {
			t.Errorf("Accept(%q) = %v, want ErrName", name, err)
		}
	}
	if names := listing(t, filepath.Dir(in.Dir())); len(names) != 0 {
		t.Errorf("bad names left %v", names)
	}
}

func TestUploadAbortRemovesThePartialFile(t *testing.T) {
	in := testInbox(t)
	u, err := in.Accept("big.iso", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	u.Write(make([]byte, 64<<10))
	if names := listing(t, in.Dir()); len(names) != 1 || !strings.HasPrefix(names[0], tempPrefix) {
		t.Errorf("during the upload the directory holds %v, want one temporary file", names)
	}
	u.Abort(errors.New("bulk transfer cancelled"))
	waitEmpty(t, in)
	if names := listing(t, in.Dir()); len(names) != 0 {
		t.Errorf("after the abort the directory holds %v", names)
	}
	if u.Saved() != "" {
		t.Errorf("an aborted upload reports the name %q", u.Saved())
	}
}

func TestInboxCloseRemovesUploadsInProgress(t *testing.T) {
	in := NewInbox(filepath.Join(t.TempDir(), "dl"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, name := range []string{"one", "two"} {
		u, err := in.Accept(name, 10)
		if err != nil {
			t.Fatal(err)
		}
		u.Write([]byte("half"))
	}
	in.Close()
	if names := listing(t, in.Dir()); len(names) != 0 {
		t.Errorf("after Close the directory holds %v", names)
	}
	if _, err := in.Accept("late", 1); !errors.Is(err, ErrClosed) {
		t.Errorf("Accept after Close = %v, want ErrClosed", err)
	}
}

func fakeFree(free int64) func(int, *unix.Statfs_t) error {
	return func(_ int, st *unix.Statfs_t) error {
		st.Bsize = 4096
		st.Bavail = uint64(free / 4096)
		return nil
	}
}

func TestUploadNeedsFreeSpace(t *testing.T) {
	in := testInbox(t)
	in.statfs = fakeFree(SpaceMargin + 8<<20)
	if _, err := in.Accept("too-big", 8<<20+4096); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("upload over the free space less the margin: %v, want ErrNoSpace", err)
	} else if !strings.Contains(err.Error(), "264.0 MiB free") {
		t.Errorf("the refusal %q does not say how much is free", err)
	}
	// Uploads in progress hold their space: two that each fit alone do not
	// fit together.
	first, err := in.Accept("first", 6<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Accept("second", 6<<20); !errors.Is(err, ErrNoSpace) {
		t.Errorf("second upload into reserved space: %v, want ErrNoSpace", err)
	}
	first.Abort(nil)
	waitEmpty(t, in)
	if u, err := in.Accept("second", 6<<20); err != nil {
		t.Errorf("once the first is gone: %v", err)
	} else {
		u.Abort(nil)
	}
	in.statfs = func(int, *unix.Statfs_t) error { return unix.EIO }
	if _, err := in.Accept("x", 1); !errors.Is(err, unix.EIO) {
		t.Errorf("statfs failing: %v", err)
	}
}

func TestUploadDiskFullMidway(t *testing.T) {
	in := testInbox(t)
	// The temporary file is real, so its removal shows; the writes go to
	// /dev/full, which fails each with ENOSPC.
	in.create = func(dir int, name string) (*os.File, error) {
		f, err := createTemp(dir, name)
		if err != nil {
			return nil, err
		}
		f.Close()
		return os.OpenFile("/dev/full", os.O_WRONLY, 0)
	}
	u, err := in.Accept("full.bin", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	var werr error
	for i := 0; i < 16 && werr == nil; i++ {
		_, werr = u.Write(chunk)
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(werr, unix.ENOSPC) {
		t.Fatalf("Write after the disk filled: %v, want ENOSPC", werr)
	}
	u.Abort(werr)
	waitEmpty(t, in)
	if names := listing(t, in.Dir()); len(names) != 0 {
		t.Errorf("after a full disk the directory holds %v", names)
	}

	u, err = in.Accept("full2.bin", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	u.Write(chunk)
	if err := u.Commit(); !errors.Is(err, unix.ENOSPC) {
		t.Errorf("Commit after the disk filled: %v, want ENOSPC", err)
	}
	waitEmpty(t, in)
	if names := listing(t, in.Dir()); len(names) != 0 {
		t.Errorf("after a failed commit the directory holds %v", names)
	}
}

// stuckFile stands in for a disk that stops answering: its writes block
// until released.
func stuckCreate(release <-chan struct{}) func(int, string) (*os.File, error) {
	return func(dir int, name string) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		go func() { <-release; io.Copy(io.Discard, r); r.Close() }()
		f, err := createTemp(dir, name)
		if err != nil {
			return nil, err
		}
		f.Close()
		return w, nil
	}
}

func TestUploadFailsRatherThanStall(t *testing.T) {
	in := testInbox(t)
	release := make(chan struct{})
	defer close(release)
	in.create = stuckCreate(release)
	u, err := in.Accept("slow.bin", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	start := time.Now()
	var werr error
	n := 0
	for ; n < 1000 && werr == nil; n++ {
		_, werr = u.Write(chunk)
	}
	if !errors.Is(werr, ErrSlowDisk) {
		t.Fatalf("Write to a stuck disk: %v after %d chunks, want ErrSlowDisk", werr, n)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("filling the queue took %v; Write must not wait", took)
	}
	// The pipe holds 64 KiB and the writer one chunk; the queue the rest.
	if n > queueChunks+4 {
		t.Errorf("queue took %d chunks, want about %d", n, queueChunks)
	}
	u.Abort(werr)
}

func TestUploadCommitGivesUp(t *testing.T) {
	commitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { commitTimeout = 5 * time.Second })
	in := testInbox(t)
	release := make(chan struct{})
	in.create = stuckCreate(release)
	u, err := in.Accept("slow.bin", 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		if _, err := u.Write(make([]byte, 64<<10)); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	err = u.Commit()
	if !errors.Is(err, ErrSlowDisk) {
		t.Errorf("Commit on a stuck disk: %v, want ErrSlowDisk", err)
	}
	if took := time.Since(start); took < commitTimeout || took > commitTimeout+time.Second {
		t.Errorf("Commit gave up after %v, want %v", took, commitTimeout)
	}
	close(release)
	waitEmpty(t, in)
	if names := listing(t, in.Dir()); len(names) != 0 {
		t.Errorf("after Commit gave up the directory holds %v", names)
	}
}

func TestUploadsAtOnceAreBounded(t *testing.T) {
	in := testInbox(t)
	var ups []*Upload
	for i := 0; i < maxUploads; i++ {
		u, err := in.Accept(Numbered("n", i), 1)
		if err != nil {
			t.Fatal(err)
		}
		ups = append(ups, u)
	}
	if _, err := in.Accept("one too many", 1); !errors.Is(err, ErrBusy) {
		t.Errorf("upload %d: %v, want ErrBusy", maxUploads+1, err)
	}
	for _, u := range ups {
		u.Abort(nil)
	}
}

func TestUploadIntoUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	in := NewInbox(file, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := in.Accept("x", 1); err == nil {
		t.Error("upload into a file instead of a directory succeeded")
	}
	in = NewInbox(filepath.Join(file, "sub"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := in.Accept("x", 1); err == nil {
		t.Error("upload under a file succeeded")
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 256 << 20: "256.0 MiB", 5 << 40: "5.0 TiB", 3 << 50: "3072.0 TiB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
