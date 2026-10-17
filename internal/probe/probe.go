// Package probe inspects a host in one SSH round trip. The CLI runs Script
// there as `sh -s -- <agent file name>...` with the script on stdin, and Parse
// turns what it prints into a Report.
package probe

import (
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Script is the probe, a POSIX sh script that prints key=value lines starting
// with "probe=1". It needs no root and tolerates any optional tool missing.
//
//go:embed probe.sh
var Script string

const header = "probe=1"

// Report is what the probe found on the host.
type Report struct {
	OS      string // uname -s
	Kernel  string // uname -r
	Machine string // uname -m
	// Arch is Machine as a GOARCH value, or "" when Go has no name for it.
	Arch string

	Distro Distro

	User     string
	UID      int
	Home     string
	CPUs     int
	MemoryKB int64

	// Env is the desktop environment as the SSH exec channel sees it. It is
	// usually empty even when a desktop is running; Sessions, X11 and Wayland
	// find those independently.
	Env Env

	// Sessions are this user's logind sessions.
	Sessions []Session
	// X11 lists the X server sockets in /tmp/.X11-unix, whoever owns them.
	X11 []X11Display
	// Wayland lists the compositor sockets in this user's runtime directory.
	Wayland []string

	GPU GPU

	// Commands maps each command robin can use, found on PATH, to its path.
	Commands map[string]string

	// XAuthority lists readable X cookie files that may belong to a running
	// display, for attaching to it.
	XAuthority []string
	// SessionBus, PulseSocket and PipeWireSocket are this user's sockets in
	// the runtime directory, or "".
	SessionBus, PulseSocket, PipeWireSocket string
	// FFmpegEncoders are the host ffmpeg's hardware H.264 encoders, such as
	// h264_vaapi. GstPlugins are the GStreamer plugins found among pipewire,
	// vaapi, va, nvcodec, x264, videoconvertscale, videoconvert, y4m and
	// y4menc, in the GstDirs that exist.
	FFmpegEncoders, GstPlugins, GstDirs []string

	// AgentDir is where the agent can be stored and executed, or "" when no
	// candidate passed. AgentDirRejected says why each failed candidate did.
	AgentDir         string
	AgentDirRejected []RejectedDir

	// SHA256Tool is how the host hashes files: "sha256sum", "shasum",
	// "openssl", "busybox", or "" when it has none.
	SHA256Tool string

	agentHashes map[string]string
}

// Distro identifies the distribution from os-release.
type Distro struct {
	ID        string   // "ubuntu", "fedora", "alpine", ...
	VersionID string   // "24.04"
	IDLike    []string // ["rhel", "centos", "fedora"]
	Name      string   // PRETTY_NAME
}

// Env holds the desktop variables of the probe's own environment.
type Env struct {
	SessionType    string // XDG_SESSION_TYPE
	Display        string // DISPLAY
	WaylandDisplay string // WAYLAND_DISPLAY
	CurrentDesktop string // XDG_CURRENT_DESKTOP
	RuntimeDir     string // XDG_RUNTIME_DIR
}

// Session is a logind session, as loginctl show-session reports it. Type is
// "x11", "wayland", "tty" and so on; Display is set for X11 sessions only.
type Session struct {
	ID, Name, Type, State, Class, Display, Desktop string
	Active, Remote                                 bool
}

// X11Display is an X server socket. Display is in DISPLAY form, like ":0".
type X11Display struct {
	Display string
	UID     int
	Owner   string
}

// GPU describes the host's render devices and the encoder tools it has.
type GPU struct {
	DRI []DRINode
	// VAInfo is the path of vainfo, or "" when absent. VADriver and
	// VAH264Encode come from running it on the first usable render node.
	VAInfo       string
	VADriver     string
	VAH264Encode bool
	// NvidiaSMI is the path of nvidia-smi, or "" when absent. NvidiaGPUs are
	// the GPUs the loaded NVIDIA driver reports.
	NvidiaSMI  string
	NvidiaGPUs []string
}

// DRINode is a /dev/dri card or render node. RW reports whether the user
// could open it read-write.
type DRINode struct {
	Path string
	RW   bool
}

// RejectedDir is an agent directory candidate the probe refused. Reason is
// one of "mkdir", "stat", "symlink", "notdir", "owner" (not the user's),
// "perms" (group or other writable), "unwritable" or "noexec".
type RejectedDir struct {
	Path, Reason string
}

// AgentHash returns the lowercase hex SHA-256 of the agent file called name
// in AgentDir, or "" when there is no such file or the host cannot hash.
func (r *Report) AgentHash(name string) string {
	return r.agentHashes[name]
}

// Parse reads the output of Script. Lines before the "probe=1" header are
// skipped, since a user's shell startup files may print to stdout before the
// script runs. Unknown keys are ignored.
func Parse(out []byte) (*Report, error) {
	lines := strings.Split(string(out), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSuffix(line, "\r") == header {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("probe: no %q header in output %q", header, excerpt(out))
	}

	r := &Report{Commands: map[string]string{}, agentHashes: map[string]string{}}
	for n, line := range lines[start:] {
		key, val, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if !ok {
			continue
		}
		if err := r.set(key, val); err != nil {
			return nil, fmt.Errorf("probe: line %d: %s: %w", start+n+1, key, err)
		}
	}
	r.Arch = goarch(r.Machine)
	return r, nil
}

func (r *Report) set(key, val string) error {
	var err error
	switch key {
	case "os":
		r.OS = val
	case "kernel":
		r.Kernel = val
	case "machine":
		r.Machine = val
	case "distro_id":
		r.Distro.ID = val
	case "distro_version":
		r.Distro.VersionID = val
	case "distro_like":
		r.Distro.IDLike = strings.Fields(val)
	case "distro_name":
		r.Distro.Name = val
	case "user":
		r.User = val
	case "uid":
		r.UID, err = atoi(val)
	case "home":
		r.Home = val
	case "cpus":
		r.CPUs, err = atoi(val)
	case "memory_kb":
		if val != "" {
			r.MemoryKB, err = strconv.ParseInt(val, 10, 64)
		}

	case "env.XDG_SESSION_TYPE":
		r.Env.SessionType = val
	case "env.DISPLAY":
		r.Env.Display = val
	case "env.WAYLAND_DISPLAY":
		r.Env.WaylandDisplay = val
	case "env.XDG_CURRENT_DESKTOP":
		r.Env.CurrentDesktop = val
	case "env.XDG_RUNTIME_DIR":
		r.Env.RuntimeDir = val

	case "session":
		r.Sessions = append(r.Sessions, Session{ID: val})
	case "session_name", "session_type", "session_state", "session_class",
		"session_display", "session_desktop", "session_active", "session_remote":
		if len(r.Sessions) > 0 {
			setSession(&r.Sessions[len(r.Sessions)-1], key, val)
		}

	case "x11":
		r.X11 = append(r.X11, X11Display{Display: val})
	case "x11_uid":
		if len(r.X11) > 0 {
			r.X11[len(r.X11)-1].UID, err = atoi(val)
		}
	case "x11_owner":
		if len(r.X11) > 0 {
			r.X11[len(r.X11)-1].Owner = val
		}
	case "wayland":
		r.Wayland = append(r.Wayland, val)

	case "dri":
		r.GPU.DRI = append(r.GPU.DRI, DRINode{Path: val})
	case "dri_rw":
		if len(r.GPU.DRI) > 0 {
			r.GPU.DRI[len(r.GPU.DRI)-1].RW = val == "yes"
		}
	case "vainfo":
		r.GPU.VAInfo = val
	case "vainfo_driver":
		r.GPU.VADriver = val
	case "vainfo_h264_encode":
		r.GPU.VAH264Encode = val == "yes"
	case "nvidia_smi":
		r.GPU.NvidiaSMI = val
	case "nvidia_gpu":
		r.GPU.NvidiaGPUs = append(r.GPU.NvidiaGPUs, val)

	case "xauth":
		r.XAuthority = appendNew(r.XAuthority, val)
	case "session_bus":
		r.SessionBus = val
	case "pulse":
		r.PulseSocket = val
	case "pipewire":
		r.PipeWireSocket = val
	case "ffmpeg_encoder":
		r.FFmpegEncoders = appendNew(r.FFmpegEncoders, val)
	case "gst_plugin":
		r.GstPlugins = appendNew(r.GstPlugins, val)
	case "gst_dir":
		r.GstDirs = appendNew(r.GstDirs, val)

	case "agentdir":
		r.AgentDir = val
	case "agentdir_rejected":
		// The reason never contains a colon; the path might.
		i := strings.LastIndexByte(val, ':')
		if i < 0 {
			return fmt.Errorf("no reason in %q", val)
		}
		r.AgentDirRejected = append(r.AgentDirRejected, RejectedDir{Path: val[:i], Reason: val[i+1:]})
	case "sha256_tool":
		if val != "none" {
			r.SHA256Tool = val
		}
	case "agent_sha256":
		name, sum, ok := strings.Cut(val, " ")
		if !ok {
			return fmt.Errorf("no hash in %q", val)
		}
		r.agentHashes[name] = strings.ToLower(sum)

	default:
		if name, ok := strings.CutPrefix(key, "cmd."); ok {
			r.Commands[name] = val
		}
	}
	return err
}

func setSession(s *Session, key, val string) {
	switch key {
	case "session_name":
		s.Name = val
	case "session_type":
		s.Type = val
	case "session_state":
		s.State = val
	case "session_class":
		s.Class = val
	case "session_display":
		s.Display = val
	case "session_desktop":
		s.Desktop = val
	case "session_active":
		s.Active = val == "yes"
	case "session_remote":
		s.Remote = val == "yes"
	}
}

// atoi treats an empty value as zero: the probe prints a key with no value
// when the tool behind it is missing.
func atoi(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func excerpt(out []byte) string {
	const limit = 120
	s := strings.TrimSpace(string(out))
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}

func goarch(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "i486", "i586", "i686":
		return "386"
	case "ppc64le", "s390x", "riscv64":
		return machine
	}
	if strings.HasPrefix(machine, "armv") {
		return "arm"
	}
	return ""
}

func appendNew(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}
