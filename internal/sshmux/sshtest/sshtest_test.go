package sshtest_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

func TestStart(t *testing.T) {
	s := sshtest.Start(t)
	args := append(append([]string{}, s.Args...), "--", s.Dest, "echo ok")
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
}

func TestStartSetsTheSessionsEnvironment(t *testing.T) {
	s := sshtest.Start(t, "ROBIN_SSHTEST_A=a b", "ROBIN_SSHTEST_B=/c")
	args := append(append([]string{}, s.Args...), "--", s.Dest, `echo "$ROBIN_SSHTEST_A,$ROBIN_SSHTEST_B"`)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "a b,/c" {
		t.Fatalf("got %q, want the environment Start was given", got)
	}
}

func TestStop(t *testing.T) {
	s := sshtest.Start(t)
	s.Stop()
	s.Stop()
	args := append(append([]string{}, s.Args...), "-o", "ConnectTimeout=2", "--", s.Dest, "true")
	if out, err := exec.Command("ssh", args...).CombinedOutput(); err == nil {
		t.Fatalf("ssh to a stopped server succeeded: %s", out)
	}
}
