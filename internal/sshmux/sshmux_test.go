package sshmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var hostile = []string{
	"",
	"plain",
	"two words",
	"it's",
	`'`,
	`''`,
	`"double"`,
	`back\slash`,
	"$(echo injected)",
	"`id`",
	"$HOME ${HOME}",
	"line\nbreak",
	"tab\there",
	"semi; colon && amp | pipe",
	"glob * ? [a]",
	"~tilde",
	"-dash",
	"A=b",
	"#hash",
	"ünïcode",
}

func TestQuoteForms(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "''"},
		{"abc", "abc"},
		{"/a/b-c_d.e:f@g,h+i%j", "/a/b-c_d.e:f@g,h+i%j"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"A=b", "'A=b'"},
		{"~", "'~'"},
	} {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := Quote("a", "b c"); got != "a 'b c'" {
		t.Errorf("Quote join = %s", got)
	}
}

// TestQuoteShell feeds quoted arguments to the local sh and checks each one
// comes back as a single, unchanged argument.
func TestQuoteShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command(sh, "-c", printArgs(hostile...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := splitArgs(out); !slices.Equal(got, hostile) {
		t.Fatalf("round trip mismatch:\n got %q\nwant %q", got, hostile)
	}
}

// printArgs is a remote command that prints each argument NUL-terminated.
func printArgs(args ...string) string {
	return `printf '%s\000' ` + Quote(args...)
}

func splitArgs(out []byte) []string {
	parts := strings.Split(string(out), "\x00")
	return parts[:len(parts)-1]
}

func TestArgs(t *testing.T) {
	m := &Master{ssh: "ssh", args: []string{"-F", "cfg", "-p", "2222"}, dest: "host", ctl: "/tmp/robin-1/ctl", mux: true}

	want := []string{
		"-o", "ControlMaster=yes", "-o", "ControlPersist=no",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
		"-o", "RemoteCommand=none",
		"-F", "cfg", "-p", "2222",
		"-S", "/tmp/robin-1/ctl", "-N", "-n", "--", "host",
	}
	if got := m.masterArgs(); !slices.Equal(got, want) {
		t.Errorf("masterArgs:\n got %q\nwant %q", got, want)
	}

	session := []string{
		"-F", os.DevNull,
		"-o", "ControlMaster=no", "-o", "ProxyCommand=false", "-o", "BatchMode=yes",
		"-S", "/tmp/robin-1/ctl", "-T", "-a", "-x", "-e", "none",
	}
	want = append(slices.Clone(session), "--", "host", "-v; echo hi")
	if got := m.childArgs("-v; echo hi"); !slices.Equal(got, want) {
		t.Errorf("childArgs:\n got %q\nwant %q", got, want)
	}
	want = append(slices.Clone(session), "-s", "--", "host", "sftp")
	if got := m.subsystemArgs("sftp"); !slices.Equal(got, want) {
		t.Errorf("subsystemArgs:\n got %q\nwant %q", got, want)
	}
}

func TestDialRejectsOptionLikeDestination(t *testing.T) {
	for _, dest := range []string{"", "-oProxyCommand=sh", "-x"} {
		if _, err := Dial(t.Context(), dest, Options{SSHPath: "/nonexistent"}); err == nil {
			t.Errorf("Dial(%q) succeeded", dest)
		}
	}
}

func TestControlDirFallback(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	dir, err := controlDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if !strings.HasPrefix(dir, "/tmp/robin-") {
		t.Errorf("controlDir = %s, want under /tmp", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", fi.Mode().Perm())
	}
}
