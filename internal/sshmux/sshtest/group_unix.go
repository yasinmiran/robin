//go:build !windows

package sshtest

import (
	"os/exec"
	"syscall"
)

const noServer = ""

func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
