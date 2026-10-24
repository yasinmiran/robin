package agentbin

import (
	"bytes"
	"debug/buildinfo"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The agents make keeps here must be reproducible: nothing of the machine
// that built them may be inside. make repro proves it by building twice;
// this is the cheap check that make test-agents runs on every build. It
// runs only there, with ROBIN_REQUIRE_AGENTS=1: agents an older Makefile
// left on disk are not the ones this tree builds.
func TestEmbeddedAgentsCarryNothingOfTheBuilder(t *testing.T) {
	if os.Getenv("ROBIN_REQUIRE_AGENTS") != "1" {
		t.Skip("checks the agents make test-agents builds")
	}
	entries, err := fs.ReadDir(bundled(), ".")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := builderPaths(t, root)
	goVersion := pinnedGoVersion(t, root)

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
		if info.GoVersion != goVersion {
			t.Errorf("%s: built by %s, go.mod wants %s", e.Name(), info.GoVersion, goVersion)
		}
		checkSettings(t, e.Name(), arch, info)
	}
	if tested == 0 {
		t.Fatal("no agents embedded")
	}
}

// builderPaths lists the directories of this machine that an agent must not
// name: the checkout and its toolchain cache, the home directory, the temp
// directory, and where Go and its modules live.
func builderPaths(t *testing.T, root string) []string {
	paths := []string{root}
	add := func(p string) {
		if p == "" || p == "/" || p == "/tmp" {
			return
		}
		for _, q := range paths {
			if q == p {
				return
			}
		}
		paths = append(paths, p)
	}
	resolved := func(p string) {
		add(p)
		if physical, err := filepath.EvalSymlinks(p); err == nil {
			add(physical)
		}
	}
	resolved(filepath.Join(root, ".cache"))
	if home, err := os.UserHomeDir(); err == nil {
		resolved(home)
	}
	resolved(os.TempDir())
	out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE", "GOPATH").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		resolved(p)
	}
	return paths
}

// pinnedGoVersion is the toolchain line of go.mod, the one Go the Makefile
// builds with.
func pinnedGoVersion(t *testing.T, root string) string {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		if v, ok := strings.CutPrefix(line, "toolchain "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("go.mod has no toolchain line")
	return ""
}

// checkSettings holds the build info to what the Makefile sets: anything
// else came from the builder's environment (GOFLAGS, GOEXPERIMENT, a
// -race or -asan, vcs stamping) and would make this agent differ from
// everyone else's.
func checkSettings(t *testing.T, name, arch string, info *buildinfo.BuildInfo) {
	want := map[string]string{
		"-buildmode":  "exe",
		"-compiler":   "gc",
		"-tags":       "openh264,opus,netgo,osusergo",
		"-trimpath":   "true",
		"CGO_ENABLED": "1",
		"GOOS":        "linux",
		"GOARCH":      arch,
	}
	// Present whenever go.mod's go line trails the toolchain; its value
	// follows the toolchain.
	optional := map[string]bool{"DefaultGODEBUG": true}
	switch arch {
	case "amd64":
		want["GOAMD64"] = "v1"
	case "arm64":
		want["GOARM64"] = "v8.0"
	}
	seen := map[string]bool{}
	for _, s := range info.Settings {
		v, ok := want[s.Key]
		if !ok && !optional[s.Key] {
			t.Errorf("%s: build info records %s=%s, which the Makefile does not set", name, s.Key, s.Value)
			continue
		}
		seen[s.Key] = true
		if ok && s.Value != v {
			t.Errorf("%s: build info records %s=%s, want %s", name, s.Key, s.Value, v)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("%s: build info lacks %s", name, k)
		}
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
