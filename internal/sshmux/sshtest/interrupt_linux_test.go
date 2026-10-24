package sshtest

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// cat is a running cat started through sh -c script, which execs into it:
// once a line comes back, script has run and the pid is cat's.
func cat(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	if _, err := io.WriteString(stdin, "up\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "up\n" {
		t.Fatalf("cat said %q, %v", line, err)
	}
	return cmd
}

func TestCtrlC(t *testing.T) {
	shielded := cat(t, `trap '' INT; exec cat`)
	if err := CtrlC(shielded.Process); err != nil {
		t.Fatalf("a cat that ignores SIGINT: %v", err)
	}

	if err := interruptIgnored(os.Getpid()); err == nil {
		t.Skip("this test runs with SIGINT ignored, which every child inherits")
	}
	plain := cat(t, `exec cat`)
	if err := CtrlC(plain.Process); err == nil || !strings.Contains(err.Error(), "does not ignore SIGINT") {
		t.Fatalf("a cat that dies of SIGINT: %v", err)
	}
	plain.Wait()
	if err := CtrlC(plain.Process); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("a cat that is gone: %v", err)
	}
	if err := interruptIgnored(1 << 30); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a pid above any pid_max: %v", err)
	}
}

func TestIgnoresInterrupt(t *testing.T) {
	for _, tc := range []struct{ status, err string }{
		{"Name:\tssh\nSigIgn:\t0000000000000002\nSigCgt:\t0000000000000000\n", ""},
		{"SigIgn:\t0000000000001000\n", "does not ignore SIGINT"},
		{"SigIgn:\tzz\n", "SigIgn: "},
		{"Name:\tssh\n", "no SigIgn"},
	} {
		err := ignoresInterrupt(tc.status)
		if (err == nil) != (tc.err == "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("ignoresInterrupt(%q) = %v, want %q", tc.status, err, tc.err)
		}
	}
}
