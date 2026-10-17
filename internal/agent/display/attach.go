//go:build linux

package display

import (
	"os"
	"path/filepath"
	"strconv"
)

// XAuthorityFiles lists the files that may hold the cookie for a display the
// user is running, most likely first: $XAUTHORITY, ~/.Xauthority (startx,
// LightDM), then what display managers leave in the user's runtime directory
// (GDM, mutter's Xwayland, SDDM). Only files that exist are listed.
func XAuthorityFiles() []string {
	home, _ := os.UserHomeDir()
	return xauthorityFiles(os.Getenv("XAUTHORITY"), home, "/run/user/"+strconv.Itoa(os.Getuid()))
}

func xauthorityFiles(env, home, runtimeDir string) []string {
	var candidates []string
	if env != "" {
		candidates = append(candidates, env)
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".Xauthority"))
	}
	candidates = append(candidates, filepath.Join(runtimeDir, "gdm", "Xauthority"))
	for _, pattern := range []string{".mutter-Xwaylandauth.*", "xauth_*"} {
		m, _ := filepath.Glob(filepath.Join(runtimeDir, pattern))
		candidates = append(candidates, m...)
	}
	var files []string
	seen := map[string]bool{}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	return files
}
