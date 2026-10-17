package gui

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// TestRunKeepsTerminalControlsFromTheHost stands in an ssh that fails with
// colour codes, a bell and a carriage return on stderr, and checks that none
// of them reaches the terminal, through Run's error or through its stderr.
func TestRunKeepsTerminalControlsFromTheHost(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nprintf 'Permission denied \\033[1;31m(publickey)\\033[0m\\a\\r\\n' >&2\nexit 255\n"
	if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	err := Run(context.Background(), Options{
		Dest: "box", SSHPath: ssh, Stdout: &stderr, Stderr: &stderr, Log: slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("Run succeeded against an ssh that fails")
	}
	for what, text := range map[string]string{"error": err.Error(), "stderr": stderr.String()} {
		if !strings.Contains(text, "Permission denied") {
			t.Errorf("%s lost the message: %q", what, text)
		}
		if i := strings.IndexFunc(text, unsafeOnTerminal); i >= 0 {
			t.Errorf("%s carries %q to the terminal: %q", what, text[i], text)
		}
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"plain text, tabs\tand\nnewlines": "plain text, tabs\tand\nnewlines",
		"Grüße, 日本":                       "Grüße, 日本",
		"\x1b[2Jcleared":                  `\x1b[2Jcleared`,
		"bell\a back\b cr\r del\x7f":      `bell\x07 back\x08 cr\x0d del\x7f`,
		"c1 \u009b2J":                     `c1 \x9b2J`,
		"raw \x9b2J byte":                 `raw \x9b2J byte`,
	} {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
}

func unsafeOnTerminal(r rune) bool {
	return r == unicode.ReplacementChar || (unicode.IsControl(r) && r != '\n' && r != '\t')
}
