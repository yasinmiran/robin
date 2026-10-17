package probe

import (
	"slices"
	"strings"
)

// waylandNeed is something attaching to the Wayland desktop needs. packages
// is nil for what no package provides, such as a running session.
type waylandNeed struct {
	name     string
	have     func(*Report) bool
	packages map[string]string
}

// waylandNeeds are checked in order: without a session nothing else matters.
// The agent reads the screen with gst-launch-1.0, pipewiresrc, videoconvert,
// videoscale and y4menc.
var waylandNeeds = []waylandNeed{
	{"a running Wayland session", func(r *Report) bool { return len(r.Wayland) > 0 }, nil},
	{"the session's D-Bus socket", func(r *Report) bool { return r.SessionBus != "" }, nil},
	{"gst-launch-1.0", func(r *Report) bool { return r.Commands["gst-launch-1.0"] != "" }, map[string]string{
		"apt": "gstreamer1.0-tools", "dnf": "gstreamer1", "zypper": "gstreamer-utils",
		"pacman": "gstreamer", "apk": "gstreamer-tools",
	}},
	{"GStreamer's PipeWire plugin", gstPlugin("pipewire"), map[string]string{
		"apt": "gstreamer1.0-pipewire", "dnf": "pipewire-gstreamer", "zypper": "gstreamer-plugin-pipewire",
		"pacman": "gst-plugin-pipewire", "apk": "gst-plugin-pipewire",
	}},
	{"GStreamer's base plugins", gstPlugin("videoconvertscale", "videoconvert"), map[string]string{
		"apt": "gstreamer1.0-plugins-base", "dnf": "gstreamer1-plugins-base", "zypper": "gstreamer-plugins-base",
		"pacman": "gst-plugins-base", "apk": "gst-plugins-base",
	}},
	{"GStreamer's good plugins", gstPlugin("y4m", "y4menc"), map[string]string{
		"apt": "gstreamer1.0-plugins-good", "dnf": "gstreamer1-plugins-good", "zypper": "gstreamer-plugins-good",
		"pacman": "gst-plugins-good", "apk": "gst-plugins-good",
	}},
}

// gstPlugin is met by any of the plugin files names. Where the probe found
// no plugin directory it cannot tell, and the agent reports what it lacks.
func gstPlugin(names ...string) func(*Report) bool {
	return func(r *Report) bool {
		return len(r.GstDirs) == 0 || slices.ContainsFunc(names, func(n string) bool { return slices.Contains(r.GstPlugins, n) })
	}
}

// WaylandMissing lists what attaching to the user's Wayland desktop needs
// and the host lacks, in words fit for a message.
func (r *Report) WaylandMissing() []string {
	var missing []string
	for _, n := range waylandNeeds {
		if !n.have(r) {
			missing = append(missing, n.name)
		}
	}
	return missing
}

// WaylandInstallHint returns one command line that installs the packages
// behind WaylandMissing, such as "sudo apt install gstreamer1.0-tools". It
// returns "" when no package would help or the distribution's package
// manager is unknown.
func (r *Report) WaylandInstallHint() string {
	pm := r.packageManager()
	if pm == "" {
		return ""
	}
	var pkgs []string
	for _, n := range waylandNeeds {
		if n.packages != nil && !n.have(r) {
			pkgs = append(pkgs, n.packages[pm])
		}
	}
	if len(pkgs) == 0 {
		return ""
	}
	sudo := "sudo "
	if r.UID == 0 {
		sudo = ""
	}
	return sudo + installCommands[pm] + " " + strings.Join(pkgs, " ")
}
