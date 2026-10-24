package sshtest

import (
	"os/exec"
	"syscall"
)

// noServer skips Start: the server's config and its process handling are
// written for a unix sshd.
const noServer = "sshtest: no sshd to run on Windows"

func ownGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, _ syscall.Signal) {
	_ = cmd.Process.Kill()
}
