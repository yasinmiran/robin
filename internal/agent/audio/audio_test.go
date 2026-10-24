//go:build linux

package audio

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFramerCutsWholeFrames(t *testing.T) {
	var frames [][]int16
	f := framer{onFrame: func(pcm []int16) { frames = append(frames, append([]int16(nil), pcm...)) }}

	// Two and a half frames of a ramp, in chunks that split samples.
	samples := 5 * frameBytes / 4
	var stream bytes.Buffer
	for i := range samples {
		binary.Write(&stream, binary.LittleEndian, int16(i-samples/2))
	}
	b := stream.Bytes()
	for len(b) > 0 {
		n := min(len(b), 997)
		f.write(b[:n])
		b = b[n:]
	}

	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	for i, fr := range frames {
		if len(fr) != frameBytes/2 {
			t.Fatalf("frame %d has %d samples", i, len(fr))
		}
		for j, v := range fr {
			if want := int16(i*frameBytes/2 + j - samples/2); v != want {
				t.Fatalf("frame %d sample %d = %d, want %d", i, j, v, want)
			}
		}
	}
	if f.n != frameBytes/2 {
		t.Errorf("held back %d bytes of the partial frame, want %d", f.n, frameBytes/2)
	}
}

func TestServerScriptQuotesPaths(t *testing.T) {
	s := serverScript(`/run/a "b"/native`, `/run/c\d/cookie`, false)
	for _, want := range []string{
		"load-module module-null-sink sink_name=robin rate=48000 channels=2 norewinds=1 ",
		"set-default-sink robin\n",
		`socket="/run/a \"b\"/native"`,
		`auth-cookie="/run/c\\d/cookie"`,
		"auth-anonymous=0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

// TestServerEnvKeepsTheUserOut checks the daemon inherits nothing that would
// lead it to the user's own session, home or bus.
func TestServerEnvKeepsTheUserOut(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("DISPLAY", ":0")
	dir := "/rd/pulse"
	for _, kv := range serverEnv(dir) {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PATH" {
			continue
		}
		if !strings.Contains(v, dir) {
			t.Errorf("%s=%s points outside %s", k, v, dir)
		}
	}
}

func TestUserSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	if got := userSocket(); got != "/run/user/4242/pulse/native" {
		t.Errorf("with XDG_RUNTIME_DIR: %s", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got, want := userSocket(), filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "pulse/native"); got != want {
		t.Errorf("without XDG_RUNTIME_DIR: %s, want %s", got, want)
	}
}

func TestUserCookie(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PULSE_COOKIE", "")
	if c := userCookie(); len(c) != cookieSize || !bytes.Equal(c, make([]byte, cookieSize)) {
		t.Error("no cookie anywhere: want 256 zero bytes")
	}

	cookie := bytes.Repeat([]byte{7}, cookieSize)
	os.MkdirAll(filepath.Join(home, ".config", "pulse"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "pulse", "cookie"), cookie, 0o600)
	if !bytes.Equal(userCookie(), cookie) {
		t.Error("~/.config/pulse/cookie not used")
	}

	explicit := filepath.Join(t.TempDir(), "c")
	os.WriteFile(explicit, bytes.Repeat([]byte{9}, cookieSize), 0o600)
	t.Setenv("PULSE_COOKIE", explicit)
	if c := userCookie(); c[0] != 9 {
		t.Error("PULSE_COOKIE not preferred")
	}
	os.WriteFile(explicit, []byte("short"), 0o600)
	if !bytes.Equal(userCookie(), cookie) {
		t.Error("a cookie of the wrong size was used")
	}
}

func TestNoServerInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	if _, err := startServer(t.Context(), dir, false); err != errNoServer {
		t.Fatalf("startServer = %v, want errNoServer", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("run dir not left empty: %v", left)
	}
}
