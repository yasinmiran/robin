package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pkg/sftp"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

func testAgent(t *testing.T) (agentbin.Agent, []byte) {
	t.Helper()
	bin := make([]byte, 2<<20)
	rand.Read(bin)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(bin)
	w.Close()
	a, err := agentbin.FromFS(fstest.MapFS{"robin-agent-linux-amd64.gz": {Data: gz.Bytes()}}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return a, bin
}

func dial(t *testing.T) *sshmux.Master {
	t.Helper()
	s := sshtest.Start(t)
	m, err := sshmux.Dial(t.Context(), s.Dest, sshmux.Options{Args: s.Args, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func ls(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestEnsureUploads(t *testing.T) {
	m := dial(t)
	a, bin := testAgent(t)
	for _, tool := range []string{"sha256sum", "shasum", "openssl", "busybox", "none"} {
		t.Run(tool, func(t *testing.T) {
			if _, err := exec.LookPath(tool); err != nil && tool != "none" {
				t.Skipf("%s not installed", tool)
			}
			dir := filepath.Join(t.TempDir(), "agents")
			rec := &recorder{Host: m}
			got, err := Ensure(t.Context(), rec, Target{Dir: dir, SHA256Tool: tool}, a)
			if err != nil {
				t.Fatal(err)
			}
			if tool == "none" && len(rec.outputs) != 0 {
				t.Errorf("ran %q with no tool", rec.cmds)
			}
			if tool != "none" {
				if len(rec.outputs) != 1 {
					t.Fatalf("ran %q, want one hash command", rec.cmds)
				}
				if sum, ok := parseHash(rec.outputs[0]); !ok || sum != a.Hash {
					t.Errorf("%s printed %q", tool, rec.outputs[0])
				}
			}
			if want := filepath.Join(dir, a.Name()); got != want {
				t.Fatalf("path %s, want %s", got, want)
			}
			assertInstalled(t, got, bin)
			if names := ls(t, dir); !slices.Equal(names, []string{a.Name()}) {
				t.Errorf("dir holds %q", names)
			}
			if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
				t.Errorf("new dir mode %v, want 0700", fi.Mode().Perm())
			}
		})
	}
}

func assertInstalled(t *testing.T, file string, bin []byte) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, bin) {
		t.Fatalf("installed %d bytes differ from the agent's %d", len(data), len(bin))
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestEnsureReplacesDamagedInstall(t *testing.T) {
	m := dial(t)
	a, bin := testAgent(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, a.Name()), []byte("truncated"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Ensure(t.Context(), m, Target{Dir: dir, PresentHash: "0badc0de", SHA256Tool: "sha256sum"}, a)
	if err != nil {
		t.Fatal(err)
	}
	assertInstalled(t, got, bin)
}

// recorder keeps every command Ensure runs and what it printed.
type recorder struct {
	Host
	cmds    []string
	outputs [][]byte
}

func (r *recorder) Output(ctx context.Context, cmd string, stdin io.Reader) ([]byte, error) {
	out, err := r.Host.Output(ctx, cmd, stdin)
	r.cmds = append(r.cmds, cmd)
	r.outputs = append(r.outputs, out)
	return out, err
}

func TestParseHash(t *testing.T) {
	sum := strings.Repeat("0f", 32)
	for _, tc := range []struct {
		out string
		ok  bool
	}{
		{sum + "  /a/file\n", true},
		{sum + " */a/file\n", true},
		{strings.ToUpper(sum) + "  f\n", true},
		{"", false},
		{"sha256sum: /a/file: No such file or directory\n", false},
		{sum[:62] + "  f\n", false},
		{"SHA2-256(/a/file)= " + sum + "\n", false},
	} {
		got, ok := parseHash([]byte(tc.out))
		if ok != tc.ok || ok && got != sum {
			t.Errorf("parseHash(%q) = %q, %v", tc.out, got, ok)
		}
	}
}

// offline fails every call, proving Ensure did not touch the host.
type offline struct{}

func (offline) Output(context.Context, string, io.Reader) ([]byte, error) {
	return nil, errors.New("offline")
}

func (offline) SFTP(context.Context) (*sftp.Client, error) { return nil, errors.New("offline") }

func TestEnsureSkipsWhenPresent(t *testing.T) {
	a, _ := testAgent(t)
	got, err := Ensure(t.Context(), offline{}, Target{Dir: "/srv/agents", PresentHash: strings.ToUpper(a.Hash)}, a)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/srv/agents/" + a.Name(); got != want {
		t.Errorf("path %s, want %s", got, want)
	}
}

func TestEnsureRejectsRelativeDir(t *testing.T) {
	a, _ := testAgent(t)
	if _, err := Ensure(t.Context(), offline{}, Target{Dir: ".cache/robin"}, a); err == nil {
		t.Fatal("relative dir accepted")
	}
}

// lyingTool reports a fixed digest for every hash command.
type lyingTool struct{ Host }

func (l lyingTool) Output(ctx context.Context, cmd string, stdin io.Reader) ([]byte, error) {
	return []byte(strings.Repeat("ab", 32) + "  /some/file\n"), nil
}

func TestEnsureDetectsCorruptUpload(t *testing.T) {
	m := dial(t)
	a, _ := testAgent(t)
	wrongHash := a
	wrongHash.Hash = strings.Repeat("cd", 32)

	for _, tc := range []struct {
		name  string
		host  Host
		tool  string
		agent agentbin.Agent
	}{
		{"tool disagrees", lyingTool{m}, "sha256sum", a},
		{"read back disagrees", m, "none", wrongHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := Ensure(t.Context(), tc.host, Target{Dir: dir, SHA256Tool: tc.tool}, tc.agent)
			t.Logf("Ensure: %v", err)
			if err == nil || !strings.Contains(err.Error(), "corrupt") {
				t.Fatalf("Ensure = %v, want a corruption error", err)
			}
			if names := ls(t, dir); len(names) != 0 {
				t.Errorf("left behind %q", names)
			}
		})
	}
}

func TestEnsureRemovesStaleFiles(t *testing.T) {
	m := dial(t)
	a, bin := testAgent(t)
	dir := t.TempDir()
	touch := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(-age)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	touch("robin-agent-0123456789abcdef", 0)
	touch(".robin-agent-0123456789abcdef.tmp-ABANDONED", 2*time.Hour)
	touch(".robin-agent-0123456789abcdef.tmp-INFLIGHT", time.Minute)
	touch("unrelated", 0)

	got, err := Ensure(t.Context(), m, Target{Dir: dir, SHA256Tool: "none"}, a)
	if err != nil {
		t.Fatal(err)
	}
	assertInstalled(t, got, bin)
	want := []string{".robin-agent-0123456789abcdef.tmp-INFLIGHT", a.Name(), "unrelated"}
	slices.Sort(want)
	if names := ls(t, dir); !slices.Equal(names, want) {
		t.Errorf("dir holds %q, want %q", names, want)
	}
}

func TestEnsureKeepsRunningAgentAlive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on Linux keeping unlinked executables alive")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	m := dial(t)
	a, _ := testAgent(t)
	dir := t.TempDir()
	old := filepath.Join(dir, "robin-agent-0123456789abcdef")
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, data, 0o700); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(old, "30")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		running.Process.Kill()
		running.Wait()
	}()

	if _, err := Ensure(t.Context(), m, Target{Dir: dir, SHA256Tool: "sha256sum"}, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old agent not removed: %v", err)
	}
	if err := running.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("running old agent died: %v", err)
	}
}

func TestEnsureRefusesSharedDir(t *testing.T) {
	m := dial(t)
	a, _ := testAgent(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(t.Context(), m, Target{Dir: dir, SHA256Tool: "sha256sum"}, a)
	if err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("Ensure = %v", err)
	}
	if names := ls(t, dir); len(names) != 0 {
		t.Errorf("wrote into a shared dir: %q", names)
	}
}
