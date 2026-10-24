package gui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

// localhost is this machine over sshtest's throwaway sshd: the probe runs
// here for real, reads only, and finds no kept session of this test's.
func localhost(t *testing.T) (Options, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	s := sshtest.Start(t)
	var stdout, stderr bytes.Buffer
	return Options{
		Dest: s.Dest, SSHArgs: s.Args, Stdout: &stdout, Stderr: &stderr,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &stdout, &stderr
}

func TestListOverSSH(t *testing.T) {
	o, stdout, stderr := localhost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := List(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Whatever this machine keeps is listed one per line, else said so.
	if lines := strings.TrimSpace(stdout.String()); lines == "" {
		if !strings.Contains(stderr.String(), "no kept sessions on "+o.Dest) {
			t.Fatalf("stdout empty and stderr %q", stderr.String())
		}
	} else {
		for _, l := range strings.Split(lines, "\n") {
			if !strings.Contains(l, "spawn") && !strings.Contains(l, "attach") {
				t.Errorf("line %q does not describe a session", l)
			}
		}
	}
}

func TestEndOverSSHRefusesAnUnknownSession(t *testing.T) {
	o, _, _ := localhost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := End(ctx, o, "zz99-not-a-session")
	if err == nil || !strings.Contains(err.Error(), "no kept session zz99-not-a-session") {
		t.Fatalf("End of nothing: %v", err)
	}
}

func TestConnectReportsABadDestination(t *testing.T) {
	o, _, _ := localhost(t)
	o.Dest = "-bad"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := List(ctx, o); err == nil || !strings.Contains(err.Error(), "invalid destination") {
		t.Fatalf("List to a bad destination: %v", err)
	}
	if err := End(ctx, o, ""); err == nil {
		t.Fatal("End to a bad destination succeeded")
	}
	if err := Run(ctx, o); err == nil {
		t.Fatal("Run to a bad destination succeeded")
	}
}
