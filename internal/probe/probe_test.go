package probe

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// summary is the part of a Report the fixture table checks.
type summary struct {
	Arch         string
	Distro       string
	UID          int
	SessionTypes []string
	X11          []string
	AgentDir     string
	Rejected     []RejectedDir
	Missing      []string
	Hint         string
}

func summarize(r *Report) summary {
	s := summary{
		Arch:     r.Arch,
		Distro:   r.Distro.ID,
		UID:      r.UID,
		AgentDir: r.AgentDir,
		Rejected: r.AgentDirRejected,
		Missing:  r.SpawnMissing(),
		Hint:     r.InstallHint(),
	}
	for _, sess := range r.Sessions {
		s.SessionTypes = append(s.SessionTypes, sess.Type)
	}
	for _, x := range r.X11 {
		s.X11 = append(s.X11, x.Display+" "+x.Owner)
	}
	return s
}

var allMissing = []string{"Xvfb", "xkbcomp", "a window manager or desktop", "a terminal"}

func TestParseFixtures(t *testing.T) {
	tests := []struct {
		file string
		want summary
	}{
		{"ubuntu-server.txt", summary{
			Arch: "amd64", Distro: "ubuntu", UID: 1000,
			SessionTypes: []string{"tty"},
			AgentDir:     "/home/alice/.cache/remotejump",
			Missing:      allMissing,
			Hint:         "sudo apt install xvfb openbox xterm",
		}},
		{"ubuntu-desktop-wayland.txt", summary{
			Arch: "amd64", Distro: "ubuntu", UID: 1000,
			SessionTypes: []string{"wayland", "tty"},
			X11:          []string{":0 bob", ":1024 gdm"},
			AgentDir:     "/home/bob/.cache/remotejump",
			Missing:      []string{"Xvfb", "a window manager or desktop"},
			Hint:         "sudo apt install xvfb openbox",
		}},
		{"fedora-x11.txt", summary{
			Arch: "amd64", Distro: "fedora", UID: 1000,
			SessionTypes: []string{"x11"},
			X11:          []string{":0 root"},
			AgentDir:     "/home/carol/.cache/remotejump",
		}},
		{"alpine-arm64.txt", summary{
			Arch: "arm64", Distro: "alpine", UID: 1000,
			AgentDir: "/home/dave/.cache/remotejump",
			Missing:  []string{"a terminal"},
			Hint:     "sudo apk add xterm",
		}},
		{"noexec-home.txt", summary{
			Arch: "amd64", Distro: "rocky", UID: 51234,
			SessionTypes: []string{"tty"},
			AgentDir:     "/run/user/51234/remotejump",
			Rejected:     []RejectedDir{{"/home/erik/.cache/remotejump", "noexec"}},
			Missing:      allMissing,
			Hint:         "sudo dnf install epel-release && sudo dnf install xorg-x11-server-Xvfb openbox xterm",
		}},
		{"missing-xvfb.txt", summary{
			Arch: "amd64", Distro: "debian", UID: 1001,
			SessionTypes: []string{"tty"},
			AgentDir:     "/home/frank/.cache/remotejump",
			Missing:      []string{"Xvfb"},
			Hint:         "sudo apt install xvfb",
		}},
		{"hostile-tmp.txt", summary{
			Arch: "amd64", Distro: "almalinux", UID: 40017,
			Rejected: []RejectedDir{
				{"/cluster/home/gina/.cache/remotejump", "mkdir"},
				{"/tmp/remotejump-40017", "owner"},
			},
			Missing: []string{"a window manager or desktop", "a terminal"},
			Hint:    "sudo dnf install epel-release && sudo dnf install openbox xterm",
		}},

		// Captured by hand from the real script, on this machine (user
		// anonymized) and in podman containers.
		{"real-host-ubuntu-22.04.txt", summary{
			Arch: "amd64", Distro: "ubuntu", UID: 1000,
			SessionTypes: []string{"tty", "x11"},
			X11:          []string{":1 alice"},
			AgentDir:     "/home/alice/.cache/remotejump",
			Missing:      []string{"Xvfb", "a window manager or desktop"},
			Hint:         "sudo apt install xvfb openbox",
		}},
		{"real-ubuntu-24.04.txt", rootSummary("ubuntu", "apt install xvfb openbox xterm")},
		{"real-debian-12.txt", rootSummary("debian", "apt install xvfb openbox xterm")},
		{"real-alpine-3.20.txt", rootSummary("alpine", "apk add xvfb openbox xterm")},
		{"real-fedora-40.txt", rootSummary("fedora", "dnf install xorg-x11-server-Xvfb openbox xterm")},
		{"real-rocky-9.txt", rootSummary("rocky", "dnf install epel-release && dnf install xorg-x11-server-Xvfb openbox xterm")},
		{"real-archlinux.txt", rootSummary("arch", "pacman -S xorg-server-xvfb openbox xterm")},
		{"real-opensuse-tumbleweed.txt", rootSummary("opensuse-tumbleweed", "zypper install xorg-x11-server-Xvfb openbox xterm")},
		{"real-alpine-spawn-ready.txt", summary{
			Arch: "amd64", Distro: "alpine", UID: 1000,
			AgentDir: "/home/dave/.cache/remotejump",
		}},
		{"real-alpine-live-displays.txt", summary{
			Arch: "amd64", Distro: "alpine", UID: 1000,
			X11:      []string{":7 root", ":8 dave"},
			AgentDir: "/home/dave/.cache/remotejump",
			Missing:  []string{"a window manager or desktop", "a terminal"},
			Hint:     "sudo apk add openbox xterm",
		}},
		{"real-noexec-cache.txt", rejectedSummary("/run/user/0/remotejump",
			RejectedDir{"/root/.cache/remotejump", "noexec"})},
		{"real-tmp-symlink.txt", rejectedSummary("",
			RejectedDir{"/root/.cache/remotejump", "noexec"},
			RejectedDir{"/tmp/remotejump-0", "symlink"})},
		{"real-tmp-perms.txt", rejectedSummary("",
			RejectedDir{"/root/.cache/remotejump", "noexec"},
			RejectedDir{"/tmp/remotejump-0", "perms"})},
		{"real-tmp-owner.txt", func() summary {
			s := rejectedSummary("",
				RejectedDir{"/root/.cache/remotejump", "noexec"},
				RejectedDir{"/tmp/remotejump-0", "owner"})
			s.Distro, s.Hint = "alpine", "apk add xvfb openbox xterm"
			return s
		}()},
		{"real-nonroot-tmp.txt", summary{
			Arch: "amd64", Distro: "ubuntu", UID: 1000,
			AgentDir: "/tmp/remotejump-1000",
			Rejected: []RejectedDir{{"/nonexistent/.cache/remotejump", "mkdir"}},
			Missing:  allMissing,
			Hint:     "sudo apt install xvfb openbox xterm",
		}},
		{"real-agent-hashes.txt", rootSummary("alpine", "apk add xvfb openbox xterm")},
	}
	for _, tt := range tests {
		t.Run(strings.TrimSuffix(tt.file, ".txt"), func(t *testing.T) {
			r := parseFile(t, tt.file)
			if got := summarize(r); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// rootSummary describes a bare container image probed as root.
func rootSummary(distro, hint string) summary {
	return summary{
		Arch: "amd64", Distro: distro,
		AgentDir: "/root/.cache/remotejump",
		Missing:  allMissing,
		Hint:     hint,
	}
}

// rejectedSummary describes the Ubuntu container runs that exercise the
// agent directory checks.
func rejectedSummary(agentDir string, rejected ...RejectedDir) summary {
	s := rootSummary("ubuntu", "apt install xvfb openbox xterm")
	s.AgentDir, s.Rejected = agentDir, rejected
	return s
}

func TestParseDesktopWayland(t *testing.T) {
	want := &Report{
		OS: "Linux", Kernel: "6.8.0-45-generic", Machine: "x86_64", Arch: "amd64",
		Distro:   Distro{ID: "ubuntu", VersionID: "24.04", IDLike: []string{"debian"}, Name: "Ubuntu 24.04.1 LTS"},
		User:     "bob",
		UID:      1000,
		Home:     "/home/bob",
		CPUs:     8,
		MemoryKB: 16165432,
		Env:      Env{SessionType: "tty", RuntimeDir: "/run/user/1000"},
		Sessions: []Session{
			{ID: "2", Name: "bob", Type: "wayland", State: "active", Class: "user", Desktop: "ubuntu", Active: true},
			{ID: "7", Name: "bob", Type: "tty", State: "active", Class: "user", Active: true, Remote: true},
		},
		X11: []X11Display{
			{Display: ":0", UID: 1000, Owner: "bob"},
			{Display: ":1024", UID: 120, Owner: "gdm"},
		},
		Wayland: []string{"/run/user/1000/wayland-0"},
		GPU: GPU{
			DRI:          []DRINode{{"/dev/dri/card1", true}, {"/dev/dri/renderD128", true}},
			VAInfo:       "/usr/bin/vainfo",
			VADriver:     "Intel iHD driver for Intel(R) Gen Graphics - 24.1.0 ()",
			VAH264Encode: true,
		},
		Commands: map[string]string{
			"xkbcomp":             "/usr/bin/xkbcomp",
			"x-terminal-emulator": "/usr/bin/x-terminal-emulator",
			"dbus-run-session":    "/usr/bin/dbus-run-session",
			"dbus-launch":         "/usr/bin/dbus-launch",
		},
		AgentDir:   "/home/bob/.cache/remotejump",
		SHA256Tool: "sha256sum",
		agentHashes: map[string]string{
			"remotejump-agent-linux-amd64": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
	}
	if got := parseFile(t, "ubuntu-desktop-wayland.txt"); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseNvidia(t *testing.T) {
	r := parseFile(t, "fedora-x11.txt")
	want := GPU{
		DRI: []DRINode{
			{"/dev/dri/card0", true}, {"/dev/dri/card1", true},
			{"/dev/dri/renderD128", true}, {"/dev/dri/renderD129", true},
		},
		NvidiaSMI:  "/usr/bin/nvidia-smi",
		NvidiaGPUs: []string{"NVIDIA GeForce RTX 3060"},
	}
	if !reflect.DeepEqual(r.GPU, want) {
		t.Errorf("GPU = %+v, want %+v", r.GPU, want)
	}
	if s := r.Sessions[0]; s.Display != ":0" || s.Desktop != "xfce" {
		t.Errorf("session = %+v, want display :0 on xfce", s)
	}
}

func TestParseVAAPI(t *testing.T) {
	r := parseFile(t, "real-ubuntu-24.04-intel-vaapi.txt")
	want := GPU{
		DRI:          []DRINode{{"/dev/dri/card1", true}, {"/dev/dri/renderD128", true}},
		VAInfo:       "/usr/bin/vainfo",
		VADriver:     "Intel iHD driver for Intel(R) Gen Graphics - 24.1.0 ()",
		VAH264Encode: true,
	}
	if !reflect.DeepEqual(r.GPU, want) {
		t.Errorf("GPU = %+v, want %+v", r.GPU, want)
	}
}

func TestAgentHash(t *testing.T) {
	r := parseFile(t, "real-agent-hashes.txt")
	tests := map[string]string{
		"agent-amd64":   "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		"agent-arm64":   "486ea46224d1bb4fb680f34f7c9ad96a8f24ec88be73ea8e5a6c65260e9cb8a7",
		"missing":       "",
		"../etc/passwd": "",
	}
	for name, want := range tests {
		if got := r.AgentHash(name); got != want {
			t.Errorf("AgentHash(%q) = %q, want %q", name, got, want)
		}
	}
	if r.SHA256Tool != "sha256sum" {
		t.Errorf("SHA256Tool = %q, want sha256sum", r.SHA256Tool)
	}
}

func TestParseHeader(t *testing.T) {
	tests := []struct {
		name, out string
		ok        bool
	}{
		{"empty", "", false},
		{"shell error", "sh: 1: Syntax error: end of file unexpected\n", false},
		{"other version", "probe=2\nos=Linux\n", false},
		{"header only", "probe=1\n", true},
		{"rc noise first", "Welcome!\nconda: base\nprobe=1\nos=Linux\n", true},
		{"crlf", "probe=1\r\nos=Linux\r\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Parse([]byte(tt.out))
			if tt.ok != (err == nil) {
				t.Fatalf("Parse error = %v, want ok %v", err, tt.ok)
			}
			if err == nil && strings.Contains(tt.out, "os=") && r.OS != "Linux" {
				t.Errorf("OS = %q, want Linux", r.OS)
			}
		})
	}
}

func TestParseTolerance(t *testing.T) {
	r, err := Parse([]byte("probe=1\nfuture_key=1\nnot a pair\nsession_type=x11\nx11_uid=5\ncpus=\nos=Linux\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.OS != "Linux" || r.CPUs != 0 || len(r.Sessions) != 0 || len(r.X11) != 0 {
		t.Errorf("got %+v", r)
	}

	for _, bad := range []string{"uid=abc", "memory_kb=lots", "agentdir_rejected=/tmp/x", "agent_sha256=nohash"} {
		if _, err := Parse([]byte("probe=1\n" + bad + "\n")); err == nil {
			t.Errorf("Parse accepted %q", bad)
		}
	}
}

func TestRejectedPathWithColon(t *testing.T) {
	r, err := Parse([]byte("probe=1\nagentdir_rejected=/home/a:b/.cache/remotejump:noexec\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []RejectedDir{{"/home/a:b/.cache/remotejump", "noexec"}}
	if !reflect.DeepEqual(r.AgentDirRejected, want) {
		t.Errorf("got %+v, want %+v", r.AgentDirRejected, want)
	}
}

func TestGoarch(t *testing.T) {
	tests := map[string]string{
		"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64",
		"armv7l": "arm", "armv6l": "arm", "armv8l": "arm",
		"i686": "386", "i386": "386",
		"ppc64le": "ppc64le", "s390x": "s390x", "riscv64": "riscv64",
		"mips": "", "": "",
	}
	for machine, want := range tests {
		if got := goarch(machine); got != want {
			t.Errorf("goarch(%q) = %q, want %q", machine, got, want)
		}
	}
}

func TestInstallHint(t *testing.T) {
	tests := []struct {
		name     string
		distro   Distro
		uid      int
		commands []string
		want     string
	}{
		{"debian user", Distro{ID: "debian"}, 1000, nil, "sudo apt install xvfb openbox xterm"},
		{"mint via id_like", Distro{ID: "linuxmint", IDLike: []string{"ubuntu", "debian"}}, 1000, nil, "sudo apt install xvfb openbox xterm"},
		{"root needs no sudo", Distro{ID: "alpine"}, 0, nil, "apk add xvfb openbox xterm"},
		{"only xkbcomp", Distro{ID: "arch"}, 1000, []string{"Xvfb", "twm", "xterm"}, "sudo pacman -S xorg-xkbcomp"},
		{"xkbcomp on debian", Distro{ID: "ubuntu"}, 1000, []string{"Xvfb", "openbox", "xterm"}, "sudo apt install x11-xkb-utils"},
		{"suse", Distro{ID: "opensuse-leap", IDLike: []string{"suse", "opensuse"}}, 1000, []string{"Xvfb", "xkbcomp"}, "sudo zypper install openbox xterm"},
		{"fedora needs no epel", Distro{ID: "fedora"}, 1000, nil, "sudo dnf install xorg-x11-server-Xvfb openbox xterm"},
		{"el without wm missing skips epel", Distro{ID: "rhel"}, 1000, []string{"twm", "xterm"}, "sudo dnf install xorg-x11-server-Xvfb"},
		{"nothing missing", Distro{ID: "debian"}, 1000, []string{"Xvfb", "xkbcomp", "fluxbox", "xfce4-terminal"}, ""},
		{"unknown distro", Distro{ID: "nixos"}, 1000, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Report{Distro: tt.distro, UID: tt.uid, Commands: map[string]string{}}
			for _, c := range tt.commands {
				r.Commands[c] = "/usr/bin/" + c
			}
			if got := r.InstallHint(); got != tt.want {
				t.Errorf("InstallHint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func parseFile(t *testing.T, name string) *Report {
	t.Helper()
	out, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseAttachAndEncoderKeys(t *testing.T) {
	out := "probe=1\nos=Linux\n" +
		"xauth=/home/a/.Xauthority\nxauth=/run/user/1000/gdm/Xauthority\nxauth=/home/a/.Xauthority\n" +
		"session_bus=/run/user/1000/bus\npulse=/run/user/1000/pulse/native\npipewire=/run/user/1000/pipewire-0\n" +
		"ffmpeg_encoder=h264_vaapi\nffmpeg_encoder=h264_nvenc\ngst_plugin=pipewire\ngst_plugin=vaapi\ngst_plugin=pipewire\n"
	r, err := Parse([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.XAuthority, []string{"/home/a/.Xauthority", "/run/user/1000/gdm/Xauthority"}) {
		t.Errorf("XAuthority %q", r.XAuthority)
	}
	if r.SessionBus != "/run/user/1000/bus" || r.PulseSocket != "/run/user/1000/pulse/native" || r.PipeWireSocket != "/run/user/1000/pipewire-0" {
		t.Errorf("sockets %q %q %q", r.SessionBus, r.PulseSocket, r.PipeWireSocket)
	}
	if !slices.Equal(r.FFmpegEncoders, []string{"h264_vaapi", "h264_nvenc"}) || !slices.Equal(r.GstPlugins, []string{"pipewire", "vaapi"}) {
		t.Errorf("encoders %q plugins %q", r.FFmpegEncoders, r.GstPlugins)
	}
}
