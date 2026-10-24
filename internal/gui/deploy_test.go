package gui

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/sshmux"
)

// testAgent is an agent binary of a few bytes, as the CLI would carry one.
func testAgent(t *testing.T) agentbin.Agent {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte("#!/bin/sh\nexit 0\n"))
	w.Close()
	a, err := agentbin.FromFS(fstest.MapFS{"robin-agent-linux-amd64.gz": {Data: gz.Bytes()}}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func dirNames(t *testing.T, dir string) []string {
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

// TestPrepareDecidesBeforeItDeploys: a kept session that runs another agent
// is refused before anything is uploaded, since an upload removes the other
// agents in the directory, that session's binary with them. Started beside
// it with --new, the upload leaves that binary in place, so the robin-send
// of the kept desktop, a link to it, still works.
func TestPrepareDecidesBeforeItDeploys(t *testing.T) {
	o, _, _ := localhost(t)
	o, log := o.fill()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := sshmux.Dial(ctx, o.Dest, sshmux.Options{Args: o.SSHArgs, Stderr: o.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	h := &host{o: o, log: log, m: m}
	defer h.close()

	dir := t.TempDir()
	const running = "robin-agent-0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, running), []byte("the kept daemon's binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "robin-agent-fedcba9876543210"), []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.report = &probe.Report{
		Arch: "amd64", AgentDir: dir, SHA256Tool: "sha256sum",
		Kept: []protocol.Session{{ID: "ab12", Mode: protocol.ModeSpawn, Agent: running, Build: "old"}},
	}
	a := testAgent(t)

	if _, _, err := h.prepare(ctx, o, a); err == nil || !strings.Contains(err.Error(), "runs agent "+running) {
		t.Fatalf("prepare beside a kept session of another agent: %v, want it refused", err)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{running, "robin-agent-fedcba9876543210"}) {
		t.Fatalf("after the refusal the agent dir holds %q, want it untouched", names)
	}

	o.New = true
	resume, path, err := h.prepare(ctx, o, a)
	if err != nil || resume != "" || path != filepath.Join(dir, a.Name()) {
		t.Fatalf("prepare with --new: %q, %q, %v", resume, path, err)
	}
	want := []string{running, a.Name()}
	slices.Sort(want)
	if names := dirNames(t, dir); !slices.Equal(names, want) {
		t.Fatalf("after --new the agent dir holds %q, want %q", names, want)
	}
}
