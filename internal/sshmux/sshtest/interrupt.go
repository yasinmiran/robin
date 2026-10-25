package sshtest

import "os"

// CtrlC sends p the SIGINT a terminal's Ctrl+C sends every process in its
// foreground group, and fails unless p ignores it, as an ssh shielded from
// the keystroke does. Where the ignore cannot be read off the process, it
// gives p the moment ssh would take to die instead, and the caller's next
// round trip over p tells.
func CtrlC(p *os.Process) error {
	if err := p.Signal(os.Interrupt); err != nil {
		return err
	}
	return interruptIgnored(p.Pid)
}
