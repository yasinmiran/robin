//go:build linux

package gstsink

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// helperEnv makes the test binary act as an agent that starts a sink and
// waits to be killed; see TestChildDiesWithTheAgent.
const helperEnv = "ROBIN_GSTSINK_TEST_HELPER"

func runHelper() {
	s, err := Start(nil, Options{Name: "helper"})
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("ready " + strconv.Itoa(s.cur.cmd.Process.Pid) + "\n")
	select {}
}

// TestChildDiesWithTheAgent kills an agent with SIGKILL, which runs no
// cleanup, and checks the kernel took its pipeline down with it.
func TestChildDiesWithTheAgent(t *testing.T) {
	fake(t, "deaf")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	f := strings.Fields(line)
	if err != nil || len(f) != 2 || f[0] != "ready" {
		cmd.Process.Kill()
		t.Fatalf("helper said %q: %v", line, err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	eventually(t, "the pipeline to die with the agent", func() bool {
		b, err := os.ReadFile("/proc/" + f[1] + "/stat")
		return err != nil || strings.Contains(string(b), ") Z ")
	})
}
