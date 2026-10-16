package agentbin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFromFS(t *testing.T) {
	bin := bytes.Repeat([]byte("\x7fELF agent "), 10000)
	sum := sha256.Sum256(bin)
	fsys := fstest.MapFS{"remotejump-agent-linux-arm64.gz": {Data: gz(t, bin)}}

	a, err := FromFS(fsys, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if a.Arch != "arm64" || a.Hash != hex.EncodeToString(sum[:]) || a.Size != int64(len(bin)) {
		t.Fatalf("got %+v", a)
	}
	if want := "remotejump-agent-" + a.Hash[:16]; a.Name() != want {
		t.Errorf("Name = %s, want %s", a.Name(), want)
	}
	for range 2 {
		rc, err := a.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, bin) {
			t.Fatalf("Open: %d bytes, %v", len(got), err)
		}
	}
}

func TestFromFSErrors(t *testing.T) {
	good := gz(t, []byte("agent"))
	fsys := fstest.MapFS{
		"remotejump-agent-linux-amd64.gz":   {Data: good},
		"remotejump-agent-linux-riscv64.gz": {Data: good[:len(good)-4]},
		"remotejump-agent-linux-mips.gz":    {Data: []byte("not gzip")},
	}
	_, err := FromFS(fsys, "arm64")
	if err == nil || !strings.Contains(err.Error(), "make") {
		t.Errorf("missing arch: %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing arch should wrap fs.ErrNotExist: %v", err)
	}
	for _, arch := range []string{"riscv64", "mips", "../amd64", ""} {
		if _, err := FromFS(fsys, arch); err == nil {
			t.Errorf("FromFS(%q) succeeded", arch)
		}
	}
}

// TestEmbedded checks that the embedded set and Names agree, whether or not
// this build bundled any agents.
func TestEmbedded(t *testing.T) {
	names := Names()
	entries, err := fs.ReadDir(bundled(), ".")
	if err != nil {
		t.Fatal(err)
	}
	var archs []string
	for _, e := range entries {
		if arch, ok := archOf(e.Name()); ok {
			archs = append(archs, arch)
		}
	}
	if len(names) != len(archs) {
		t.Fatalf("Names = %q for bundled archs %q", names, archs)
	}
	for _, arch := range archs {
		a, err := For(arch)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := For(arch); b != a {
			t.Errorf("For(%s) not stable", arch)
		}
	}
	if _, err := For("no-such-arch"); err == nil || !strings.Contains(err.Error(), "rebuild with make") {
		t.Errorf("For unbundled arch: %v", err)
	}
}
