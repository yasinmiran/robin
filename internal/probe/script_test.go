package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestScriptShells runs the embedded script the way the CLI does, on stdin
// with agent names as arguments, under every POSIX shell this machine has.
func TestScriptShells(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe targets Linux hosts")
	}
	shells := [][]string{{"sh"}, {"dash"}, {"bash"}, {"busybox", "sh"}}
	ran := 0
	for _, shell := range shells {
		if _, err := exec.LookPath(shell[0]); err != nil {
			continue
		}
		ran++
		t.Run(strings.Join(shell, "-"), func(t *testing.T) {
			cache := t.TempDir()
			agentDir := filepath.Join(cache, "robin")
			if err := os.Mkdir(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			agent := []byte("agent bytes")
			if err := os.WriteFile(filepath.Join(agentDir, "agent-a"), agent, 0o700); err != nil {
				t.Fatal(err)
			}
			// Two kept sessions: one whose daemon (this process) lives, one
			// whose pid is gone.
			for id, pid := range map[string]int{"ab12": os.Getpid(), "dead": deadPID(t)} {
				dir := filepath.Join(agentDir, "run", id)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				line := fmt.Sprintf(`{"id":%q,"pid":%d,"agent":"agent-a","mode":"spawn","display":":7","keep":28800,"started":"2026-10-24T10:00:00Z","connected":false}`, id, pid)
				if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(line+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stubs := stubCommands(t)

			args := append(shell[1:], "-s", "--", "agent-a", "agent-b")
			cmd := exec.Command(shell[0], args...)
			cmd.Stdin = strings.NewReader(Script)
			cmd.Env = append(os.Environ(),
				"XDG_CACHE_HOME="+cache,
				"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			start := time.Now()
			out, err := cmd.Output()
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%v: %v\n%s", shell, err, stderr.Bytes())
			}
			if stderr.Len() > 0 {
				t.Errorf("stderr: %s", stderr.Bytes())
			}
			if elapsed > 2*time.Second {
				t.Errorf("probe took %v", elapsed)
			}
			r, err := Parse(out)
			if err != nil {
				t.Fatal(err)
			}

			if r.OS != "Linux" || r.Arch != runtime.GOARCH || r.UID != os.Getuid() {
				t.Errorf("os %q arch %q uid %d, want Linux %s %d", r.OS, r.Arch, r.UID, runtime.GOARCH, os.Getuid())
			}
			if r.CPUs < 1 || r.MemoryKB < 1 {
				t.Errorf("cpus %d memory %d kB", r.CPUs, r.MemoryKB)
			}
			if r.AgentDir != agentDir || len(r.AgentDirRejected) != 0 {
				t.Errorf("agent dir %q rejected %v, want %q", r.AgentDir, r.AgentDirRejected, agentDir)
			}
			sum := sha256.Sum256(agent)
			if got := r.AgentHash("agent-a"); got != hex.EncodeToString(sum[:]) {
				t.Errorf("AgentHash(agent-a) = %q (tool %q)", got, r.SHA256Tool)
			}
			if got := r.AgentHash("agent-b"); got != "" {
				t.Errorf("AgentHash(agent-b) = %q for a missing file", got)
			}
			if missing := r.SpawnMissing(); len(missing) != 0 {
				t.Errorf("SpawnMissing() = %v with every command stubbed", missing)
			}
			for _, name := range spawnCommands() {
				if want := filepath.Join(stubs, name); r.Commands[name] != want {
					t.Errorf("Commands[%q] = %q, want %q", name, r.Commands[name], want)
				}
			}
			entries, _ := os.ReadDir(agentDir)
			if len(entries) != 2 {
				t.Errorf("agent dir holds %v, want agent-a and run", entries)
			}
			if len(r.Kept) != 1 || r.Kept[0].ID != "ab12" || r.Kept[0].PID != os.Getpid() || r.Kept[0].Keep != 28800 {
				t.Errorf("Kept = %+v, want the live session ab12 alone", r.Kept)
			}
			if r.KillUserProcesses != "" && r.KillUserProcesses != "yes" && r.KillUserProcesses != "no" {
				t.Errorf("KillUserProcesses = %q", r.KillUserProcesses)
			}
		})
	}
	if ran == 0 {
		t.Skip("no POSIX shell found")
	}
}

// TestScriptKVKeepsToOneLine checks the one way the script prints: a pair
// that would span lines is left out, so no value it reads from the host
// can add keys of its own.
func TestScriptKVKeepsToOneLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe targets Linux hosts")
	}
	body, ok := strings.CutSuffix(strings.TrimSpace(Script), `main "$@"`)
	if !ok {
		t.Fatal(`the script no longer ends with main "$@"`)
	}
	for v, want := range map[string]string{
		"plain":             "key=plain\n",
		"a\nagentdir=/evil": "",
		"a\ragentdir=/evil": "",
	} {
		out, err := exec.Command("sh", "-c", body+`kv key "$1"`, "sh", v).Output()
		if err != nil || string(out) != want {
			t.Errorf("kv key %q printed %q (%v), want %q", v, out, err, want)
		}
	}
}

// deadPID is a pid no process has: a child already reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// spawnCommands are the commands SpawnMissing cares about.
func spawnCommands() []string {
	var names []string
	for _, req := range spawnRequirements {
		names = append(names, req.commands...)
	}
	return names
}

// stubCommands returns a directory of executables named after every spawn
// command, so the test proves the script looks for each of them.
func stubCommands(t *testing.T) string {
	dir := t.TempDir()
	for _, name := range spawnCommands() {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestScriptContainers runs the script in distribution images under rootless
// podman. Images are never pulled; a missing one skips its cases.
func TestScriptContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("containers are slow")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not found")
	}

	const (
		ubuntu = "docker.io/library/ubuntu:24.04"
		alpine = "docker.io/library/alpine:3.20"
		fedora = "docker.io/library/fedora:40"
	)
	// Mounting the first candidate noexec forces the probe on to the next.
	noexecCache := []string{"--tmpfs", "/root/.cache:rw,noexec,mode=700"}
	strangersX11 := []string{"-v", strangersSockets(t) + ":/tmp/.X11-unix"}

	tests := []struct {
		name   string
		image  string
		flags  []string
		setup  string   // shell run as root before the probe
		shell  string   // defaults to sh
		agents []string // defaults to one name
		check  func(*testing.T, *Report)
	}{
		{name: "ubuntu-dash", image: ubuntu, check: bare("ubuntu", "apt install xvfb openbox xterm")},
		{name: "ubuntu-bash", image: ubuntu, shell: "bash", check: bare("ubuntu", "apt install xvfb openbox xterm")},
		{name: "alpine-busybox", image: alpine, check: bare("alpine", "apk add xvfb openbox xterm")},
		{name: "fedora-bash", image: fedora, check: bare("fedora", "dnf install xorg-x11-server-Xvfb openbox xterm")},
		{
			name:  "noexec-cache-falls-back-to-runtime-dir",
			image: ubuntu,
			flags: append(noexecCache, "--tmpfs", "/run/user/0:rw,exec,mode=700", "-e", "XDG_RUNTIME_DIR=/run/user/0"),
			check: agentDir("/run/user/0/robin", "/root/.cache/robin:noexec"),
		},
		{
			name: "tmp-fresh", image: ubuntu, flags: noexecCache,
			check: agentDir("/tmp/robin-0", "/root/.cache/robin:noexec"),
		},
		{
			name: "tmp-symlink", image: ubuntu, flags: noexecCache,
			setup: "mkdir -m 700 /srv/t && ln -s /srv/t /tmp/robin-0",
			check: agentDir("", "/root/.cache/robin:noexec", "/tmp/robin-0:symlink"),
		},
		{
			name: "tmp-dangling-symlink", image: alpine, flags: noexecCache,
			setup: "ln -s /nowhere /tmp/robin-0",
			check: agentDir("", "/root/.cache/robin:noexec", "/tmp/robin-0:symlink"),
		},
		{
			name: "tmp-world-writable", image: ubuntu, flags: noexecCache,
			setup: "mkdir -m 777 /tmp/robin-0",
			check: agentDir("", "/root/.cache/robin:noexec", "/tmp/robin-0:perms"),
		},
		{
			name: "tmp-group-writable", image: alpine, flags: noexecCache,
			setup: "mkdir -m 770 /tmp/robin-0",
			check: agentDir("", "/root/.cache/robin:noexec", "/tmp/robin-0:perms"),
		},
		{
			name: "tmp-other-owner", image: alpine, flags: noexecCache,
			setup: "mkdir -m 700 /tmp/robin-0 && chown 1000:1000 /tmp/robin-0",
			check: agentDir("", "/root/.cache/robin:noexec", "/tmp/robin-0:owner"),
		},
		{
			name: "tmp-file", image: ubuntu, flags: noexecCache,
			setup: "touch /tmp/robin-0",
			check: agentDir("", "/root/.cache/robin:noexec", "/tmp/robin-0:notdir"),
		},
		{
			name: "non-root-without-home", image: ubuntu,
			flags: []string{"--user", "1000:1000", "-e", "HOME=/nonexistent"},
			check: agentDir("/tmp/robin-1000", "/nonexistent/.cache/robin:mkdir"),
		},
		{
			name: "agent-hashes", image: alpine,
			setup:  "mkdir -m 700 -p /root/.cache/robin && printf hello >/root/.cache/robin/agent-amd64",
			agents: []string{"agent-amd64", "agent-arm64", "../../../etc/passwd"},
			check: func(t *testing.T, r *Report) {
				const hello = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
				if got := r.AgentHash("agent-amd64"); got != hello {
					t.Errorf("AgentHash(agent-amd64) = %q, want %q", got, hello)
				}
				if got := r.AgentHash("agent-arm64") + r.AgentHash("../../../etc/passwd"); got != "" {
					t.Errorf("hashes for absent or unsafe names: %q", got)
				}
			},
		},
		{name: "x11-names-from-a-stranger-dash", image: ubuntu, flags: strangersX11, agents: []string{"agent-a"}, check: strangersNamesIgnored},
		{name: "x11-names-from-a-stranger-busybox", image: alpine, flags: strangersX11, agents: []string{"agent-a"}, check: strangersNamesIgnored},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if exec.Command("podman", "image", "exists", tt.image).Run() != nil {
				t.Skipf("image %s not present", tt.image)
			}
			shell := tt.shell
			if shell == "" {
				shell = "sh"
			}
			agents := tt.agents
			if agents == nil {
				agents = []string{"robin-agent-linux-amd64"}
			}
			probe := shell + " -s -- " + strings.Join(agents, " ")
			if tt.setup != "" {
				probe = tt.setup + " && exec " + probe
			}
			args := append([]string{"run", "--rm", "-i", "--pull=never", "--network=none"}, tt.flags...)
			args = append(args, tt.image, "sh", "-c", probe)

			cmd := exec.Command("podman", args...)
			cmd.Stdin = strings.NewReader(Script)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("podman: %v\n%s", err, stderr.Bytes())
			}
			r, err := Parse(out)
			if err != nil {
				t.Fatal(err)
			}
			if r.Arch != runtime.GOARCH {
				t.Errorf("Arch = %q, want %q", r.Arch, runtime.GOARCH)
			}
			if r.SHA256Tool == "" {
				t.Error("no sha256 tool found")
			}
			tt.check(t, r)
		})
	}
}

// strangersSockets makes a stand-in for /tmp/.X11-unix, which any local
// user can write: display 5's socket, and sockets whose names carry a
// newline and probe lines after it, as another user could bind them.
func strangersSockets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{
		"X5",
		"X0\nagent_sha256=agent-a " + strings.Repeat("ab", 32),
		"X1\nkept_session={\"id\":\"zz\",\"pid\":1,\"agent\":\"agent-a\",\"mode\":\"spawn\"}",
		"X2\nuid=nobody",
		"X3\rx11=:9",
	} {
		// Bound under a short name, since a socket's path has to fit in
		// sun_path, then renamed.
		short := filepath.Join(dir, "s")
		ln, err := net.Listen("unix", short)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close()
		if err := os.Rename(short, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// strangersNamesIgnored checks that the probe reported display 5 and
// nothing a socket's name said.
func strangersNamesIgnored(t *testing.T, r *Report) {
	if len(r.X11) != 1 || r.X11[0].Display != ":5" {
		t.Errorf("X11 = %+v, want display 5 alone", r.X11)
	}
	if got := r.AgentHash("agent-a"); got != "" {
		t.Errorf("AgentHash(agent-a) = %q for a file that is not there", got)
	}
	if len(r.Kept) != 0 || r.UID != 0 {
		t.Errorf("Kept %+v, uid %d: a socket's name reached the report", r.Kept, r.UID)
	}
}

// bare checks a stock image probed as root: no desktop, nothing for spawn
// mode, and the agent directory in root's cache.
func bare(distro, hint string) func(*testing.T, *Report) {
	return func(t *testing.T, r *Report) {
		if r.Distro.ID != distro || r.UID != 0 || r.Home != "/root" {
			t.Errorf("distro %q uid %d home %q", r.Distro.ID, r.UID, r.Home)
		}
		if len(r.Sessions)+len(r.X11)+len(r.Wayland) != 0 {
			t.Errorf("desktop found in a container: %+v %+v %v", r.Sessions, r.X11, r.Wayland)
		}
		if got := r.SpawnMissing(); !slices.Equal(got, allMissing) {
			t.Errorf("SpawnMissing() = %v", got)
		}
		if got := r.InstallHint(); got != hint {
			t.Errorf("InstallHint() = %q, want %q", got, hint)
		}
		agentDir("/root/.cache/robin")(t, r)
	}
}

// agentDir checks the chosen directory and the "path:reason" rejections.
func agentDir(want string, rejected ...string) func(*testing.T, *Report) {
	return func(t *testing.T, r *Report) {
		var got []string
		for _, d := range r.AgentDirRejected {
			got = append(got, d.Path+":"+d.Reason)
		}
		if r.AgentDir != want || !reflect.DeepEqual(got, rejected) {
			t.Errorf("agent dir %q rejected %q, want %q rejected %q", r.AgentDir, got, want, rejected)
		}
	}
}
