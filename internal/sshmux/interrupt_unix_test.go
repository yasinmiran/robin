//go:build !windows

package sshmux

import (
	"bufio"
	"io"
	"os"
	"testing"

	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

// ctrlC is Ctrl+C at a terminal for ssh p, which must shrug it off.
func ctrlC(t *testing.T, p *os.Process) {
	t.Helper()
	if err := sshtest.CtrlC(p); err != nil {
		t.Fatalf("Ctrl+C: %v", err)
	}
}

// TestOwnConnectionsOutliveCtrlC: every ssh that holds a connection of its
// own (the master, or without one each command and forward) stays
// connected through Ctrl+C, so the CLI can still end the session over it.
func TestOwnConnectionsOutliveCtrlC(t *testing.T) {
	t.Run("master", func(t *testing.T) {
		m := dial(t)
		ctrlC(t, m.cmd.Process)
		if out, err := m.Output(t.Context(), "echo still", nil); err != nil || string(out) != "still\n" {
			t.Fatalf("over the master after Ctrl+C: %q, %v", out, err)
		}
	})

	t.Run("command", func(t *testing.T) {
		m := dialNoMux(t, sshtest.Start(t))
		cmd := m.Command(t.Context(), "cat")
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
		lines := bufio.NewReader(stdout)
		echo := func(line string) {
			t.Helper()
			if _, err := io.WriteString(stdin, line+"\n"); err != nil {
				t.Fatalf("write %q: %v", line, err)
			}
			if got, err := lines.ReadString('\n'); err != nil || got != line+"\n" {
				t.Fatalf("echo of %q: %q, %v", line, got, err)
			}
		}
		echo("before") // logged in, its client loop running
		ctrlC(t, cmd.Process)
		echo("after")
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("the command, once its input ended: %v", err)
		}
	})

	t.Run("forward", func(t *testing.T) {
		m := dialNoMux(t, sshtest.Start(t))
		f, err := m.Forward(t.Context(), echoServer(t))
		if err != nil {
			t.Fatal(err)
		}
		echoThrough(t, f)
		ctrlC(t, f.proc.cmd.Process)
		echoThrough(t, f)
	})
}
