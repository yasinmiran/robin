package gui

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/sshmux"
)

func TestStartMessageDefaults(t *testing.T) {
	for _, tc := range []struct {
		name         string
		o            Options
		cpus         int
		w, h, fps, k int
	}{
		{"defaults", Options{}, 8, 2560, 1440, 30, 10_000},
		{"four cores", Options{}, 4, 1920, 1080, 30, 10_000},
		{"five cores", Options{}, 5, 2560, 1440, 30, 10_000},
		{"small host", Options{}, 2, 1280, 720, 20, 10_000},
		{"unknown cores", Options{}, 0, 1920, 1080, 30, 10_000},
		{"asked for", Options{MaxWidth: 2560, MaxHeight: 1440, FPS: 60, MaxKbps: 4000}, 2, 2560, 1440, 60, 4000},
	} {
		s, _ := startMessage(tc.o, &probe.Report{CPUs: tc.cpus})
		if s.MaxWidth != tc.w || s.MaxHeight != tc.h || s.FPS != tc.fps || s.MaxKbps != tc.k {
			t.Errorf("%s: %dx%d at %d fps, %d kbps; want %dx%d at %d fps, %d kbps",
				tc.name, s.MaxWidth, s.MaxHeight, s.FPS, s.MaxKbps, tc.w, tc.h, tc.fps, tc.k)
		}
	}
}

// TestTunnelTransportAsksNoSTUNServer: with --transport tunnel neither the
// agent nor the page is given a STUN server, and the agent is told to
// gather no UDP. No UDP pair can form then, so a public address would
// serve nothing, and asking for it would tell the server both ends'
// addresses. The other transports keep the servers the user gave, and
// UDP. A resume and a redial ask for the same transport.
func TestTunnelTransportAsksNoSTUNServer(t *testing.T) {
	stun := []string{"stun:stun.example:3478"}
	for transport, want := range map[relay.Transport][]string{"": stun, relay.Auto: stun, relay.Direct: stun, relay.Tunnel: nil} {
		o := Options{Transport: transport, STUN: stun}
		s, _ := startMessage(o, &probe.Report{})
		if !slices.Equal(s.STUN, want) {
			t.Errorf("%q: the agent gets STUN %q, want %q", transport, s.STUN, want)
		}
		tunnel := transport == relay.Tunnel
		if s.Tunnel != tunnel || resumeMessage(o, "ab12").Tunnel != tunnel || redialMessage(o, "ab12").Tunnel != tunnel {
			t.Errorf("%q: the agent is told Tunnel %v, on a resume %v, on a redial %v",
				transport, s.Tunnel, resumeMessage(o, "ab12").Tunnel, redialMessage(o, "ab12").Tunnel)
		}
		page, err := json.Marshal(protocol.Config{ICEServers: iceServers(s.STUN)})
		if err != nil {
			t.Fatal(err)
		}
		if (want == nil) != strings.Contains(string(page), `"iceServers":[]`) {
			t.Errorf("%q: the page gets %s", transport, page)
		}
	}
}

// TestStartMessageEncoder: auto becomes the probe's pick, judged at the
// session's maximum size, and only a hardware pick may fall back; an
// encoder the user named goes out as named, without fallback.
func TestStartMessageEncoder(t *testing.T) {
	vaapi := probe.Report{
		CPUs: 20, FFmpegEncoders: []string{"h264_vaapi"},
		GPU: probe.GPU{DRI: []probe.DRINode{{Path: "/dev/dri/renderD128", RW: true}}},
	}
	for _, tc := range []struct {
		name     string
		o        Options
		r        probe.Report
		encoder  string
		fallback bool
		why      string
	}{
		{"auto, no GPU", Options{Encoder: "auto"}, probe.Report{CPUs: 8}, "software", false, "no GPU encoder"},
		{"auto, 1080p on 20 CPUs", Options{Encoder: "auto"}, vaapi, "software", false, "cheap enough"},
		{"auto, 4K on 20 CPUs", Options{Encoder: "auto", MaxWidth: 3840, MaxHeight: 2160}, vaapi, "vaapi", true, "3840x2160"},
		{"named vaapi", Options{Encoder: "vaapi"}, vaapi, "vaapi", false, ""},
		{"named software", Options{Encoder: "software", MaxWidth: 3840, MaxHeight: 2160}, vaapi, "software", false, ""},
	} {
		s, why := startMessage(tc.o, &tc.r)
		if s.Encoder != tc.encoder || s.EncoderFallback != tc.fallback || !strings.Contains(why, tc.why) || (tc.why == "") != (why == "") {
			t.Errorf("%s: encoder %s, fallback %v, why %q; want %s, %v, %q",
				tc.name, s.Encoder, s.EncoderFallback, why, tc.encoder, tc.fallback, tc.why)
		}
		if s.Encoder == protocol.EncoderAuto {
			t.Errorf("%s: auto went out to the agent", tc.name)
		}
	}
}

// testAgent is an agent binary of a few bytes, as the CLI would carry one.
func testAgent(t *testing.T) agentbin.Agent {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write([]byte("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
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
