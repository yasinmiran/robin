package display

import (
	"os"
	"strings"
)

// localeFiles hold the system locale a display manager would give a
// session: systemd's, then Debian's.
var localeFiles = []string{"/etc/locale.conf", "/etc/default/locale"}

// withLocale returns env with LANG added when it names no locale. An agent
// started over SSH has one only when PAM or the client passed it on, and a
// session left in the C locale shows UTF-8 text as mojibake. C.UTF-8 is the
// last resort: current glibc and musl both have it.
func withLocale(env, files []string) []string {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if (k == "LANG" || k == "LC_ALL" || k == "LC_CTYPE") && v != "" {
			return env
		}
	}
	for _, f := range files {
		if lang := readLang(f); lang != "" {
			return append(env, "LANG="+lang)
		}
	}
	return append(env, "LANG=C.UTF-8")
}

// readLang returns the LANG assignment in a file of shell-style
// assignments, without its quotes, or "" when there is none.
func readLang(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == "LANG" {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
