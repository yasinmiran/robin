package probe

import (
	"slices"
	"strings"

	"github.com/yasinmiran/remotejump/internal/sessions"
)

// requirement is something spawn mode needs, met by any one of commands.
type requirement struct {
	name     string
	commands []string
	packages map[string]string // package manager to package
}

var (
	xvfb = requirement{"Xvfb", []string{"Xvfb"}, map[string]string{
		"apt": "xvfb", "dnf": "xorg-x11-server-Xvfb", "zypper": "xorg-x11-server-Xvfb",
		"pacman": "xorg-server-xvfb", "apk": "xvfb",
	}}
	xkbcomp = requirement{"xkbcomp", []string{"xkbcomp"}, map[string]string{
		"apt": "x11-xkb-utils", "dnf": "xkbcomp", "zypper": "xkbcomp",
		"pacman": "xorg-xkbcomp", "apk": "xkbcomp",
	}}
	windowManager = requirement{"a window manager or desktop",
		slices.Concat(sessions.Desktops, sessions.WindowManagers), everywhere("openbox")}
	terminal = requirement{"a terminal", sessions.Terminals, everywhere("xterm")}

	spawnRequirements = []*requirement{&xvfb, &xkbcomp, &windowManager, &terminal}
)

func everywhere(pkg string) map[string]string {
	return map[string]string{"apt": pkg, "dnf": pkg, "zypper": pkg, "pacman": pkg, "apk": pkg}
}

var installCommands = map[string]string{
	"apt":    "apt install",
	"dnf":    "dnf install",
	"zypper": "zypper install",
	"pacman": "pacman -S",
	"apk":    "apk add",
}

// packageManagers maps os-release IDs, matched against ID then ID_LIKE.
var packageManagers = map[string]string{
	"debian": "apt", "ubuntu": "apt",
	"fedora": "dnf", "rhel": "dnf", "centos": "dnf",
	"opensuse": "zypper", "suse": "zypper",
	"arch":   "pacman",
	"alpine": "apk",
}

// SpawnMissing lists what spawn mode needs and the host lacks, in words fit
// for a message: "Xvfb", "xkbcomp", "a window manager or desktop", "a terminal".
func (r *Report) SpawnMissing() []string {
	var missing []string
	for _, req := range spawnRequirements {
		if r.lacks(req) {
			missing = append(missing, req.name)
		}
	}
	return missing
}

// InstallHint returns one command line that installs what SpawnMissing
// reports, such as "sudo apt install xvfb openbox xterm". It returns "" when
// nothing is missing or the distribution's package manager is unknown.
func (r *Report) InstallHint() string {
	pm := r.packageManager()
	if pm == "" {
		return ""
	}
	var pkgs []string
	for _, req := range spawnRequirements {
		if !r.lacks(req) {
			continue
		}
		// Every distribution's Xvfb package depends on xkbcomp.
		if req == &xkbcomp && r.lacks(&xvfb) {
			continue
		}
		pkgs = append(pkgs, req.packages[pm])
	}
	if len(pkgs) == 0 {
		return ""
	}

	sudo := "sudo "
	if r.UID == 0 {
		sudo = ""
	}
	cmd := sudo + installCommands[pm] + " " + strings.Join(pkgs, " ")
	// Enterprise Linux ships no window manager outside EPEL.
	if pm == "dnf" && r.Distro.ID != "fedora" && r.distroIs("rhel") && r.lacks(&windowManager) {
		cmd = sudo + "dnf install epel-release && " + cmd
	}
	return cmd
}

func (r *Report) lacks(req *requirement) bool {
	// A full desktop brings its own way to start programs.
	if req == &terminal && !r.lacks(&requirement{commands: sessions.Desktops}) {
		return false
	}
	for _, c := range req.commands {
		if r.Commands[c] != "" {
			return false
		}
	}
	return true
}

func (r *Report) packageManager() string {
	for _, id := range append([]string{r.Distro.ID}, r.Distro.IDLike...) {
		if pm, ok := packageManagers[id]; ok {
			return pm
		}
	}
	return ""
}

func (r *Report) distroIs(id string) bool {
	return r.Distro.ID == id || slices.Contains(r.Distro.IDLike, id)
}
