package gui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasinmiran/remotejump/internal/probe"
)

func fixture(t *testing.T, name string) *probe.Report {
	t.Helper()
	out, err := os.ReadFile(filepath.Join("..", "probe", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := probe.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPickAttach(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    string
	}{
		{"fedora-x11.txt", ":0"},                // the session names its display; root owns the socket
		{"real-host-ubuntu-22.04.txt", ":1"},    // GDM: an active X11 session with no display, one socket of ours
		{"real-alpine-live-displays.txt", ":8"}, // no logind; root's :7 is not ours, :8 is
	} {
		got, err := pickAttach(fixture(t, tc.fixture))
		if err != nil || got != tc.want {
			t.Errorf("%s: pickAttach = %q, %v; want %q", tc.fixture, got, err, tc.want)
		}
	}
}

func TestPickAttachRefusesToGuess(t *testing.T) {
	twoServers := fixture(t, "real-alpine-live-displays.txt")
	twoServers.X11 = append(twoServers.X11, probe.X11Display{Display: ":9", UID: twoServers.UID, Owner: "dave"})
	remoteOnly := fixture(t, "fedora-x11.txt")
	remoteOnly.Sessions[0].Remote = true

	for _, tc := range []struct {
		name string
		r    *probe.Report
		want []string
	}{
		{"wayland", fixture(t, "ubuntu-desktop-wayland.txt"), []string{
			"Wayland", "--attach wayland", "2 (wayland, active)", "7 (tty, active, remote)", ":0 (bob)", ":1024 (gdm)"}},
		{"server", fixture(t, "ubuntu-server.txt"), []string{"no X11 desktop", "--attach :N", "X displays: none"}},
		{"two servers", twoServers, []string{"more than one X server", ":8 (dave)", ":9 (dave)", ":7 (root)"}},
		{"remote session", remoteOnly, []string{"no X11 desktop", "3 (x11, display :0, active, remote)", ":0 (root)"}},
	} {
		got, err := pickAttach(tc.r)
		if err == nil {
			t.Errorf("%s: picked %q", tc.name, got)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q lacks %q", tc.name, err, w)
			}
		}
	}
}
