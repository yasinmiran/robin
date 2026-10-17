package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
			if len(entries) != 1 {
				t.Errorf("agent dir holds %v, want only agent-a", entries)
			}
		})
	}
	if ran == 0 {
		t.Skip("no POSIX shell found")
	}
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
