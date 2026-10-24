package agentbin

import (
	"bytes"
	"debug/buildinfo"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agents make keeps here must be reproducible: nothing of the machine
// that built them may be inside. make repro proves it by building twice;
// this is the cheap check that runs with every make test.
func TestEmbeddedAgentsCarryNothingOfTheBuilder(t *testing.T) {
	entries, err := fs.ReadDir(bundled(), ".")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{root}
	if physical, err := filepath.EvalSymlinks(root); err == nil && physical != root {
		forbidden = append(forbidden, physical)
	}
	if home, err := os.UserHomeDir(); err == nil {
		forbidden = append(forbidden, home)
	}
	if tmp := os.TempDir(); tmp != "/tmp" {
		forbidden = append(forbidden, tmp)
	}

	tested := 0
	for _, e := range entries {
		arch, ok := archOf(e.Name())
		if !ok {
			continue
		}
		tested++
		raw, err := fs.ReadFile(bundled(), e.Name())
		if err != nil {
			t.Fatal(err)
		}
		// RFC 1952 header: ID1 ID2 CM FLG MTIME(4) XFL OS.
		if len(raw) < 10 || raw[3] != 0 || !bytes.Equal(raw[4:8], []byte{0, 0, 0, 0}) {
			t.Errorf("%s: gzip header has flags or a time: % x", e.Name(), raw[:min(len(raw), 10)])
		}
		a, err := For(arch)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := a.Open()
		if err != nil {
			t.Fatal(err)
		}
		bin, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range forbidden {
			if i := bytes.Index(bin, []byte(s)); i >= 0 {
				end := min(len(bin), i+len(s)+80)
				t.Errorf("%s: contains the builder's %q: %q", e.Name(), s, cut(bin[i:end]))
			}
		}
		info, err := buildinfo.Read(bytes.NewReader(bin))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		for _, s := range info.Settings {
			if strings.HasPrefix(s.Key, "vcs") {
				t.Errorf("%s: build info records %s=%s", e.Name(), s.Key, s.Value)
			}
		}
	}
	if tested == 0 {
		t.Skip("no agents embedded; run make first")
	}
}

// cut returns the printable run that starts b.
func cut(b []byte) string {
	for i, c := range b {
		if c < ' ' || c > '~' {
			return string(b[:i])
		}
	}
	return string(b)
}
