//go:build linux

package wayland

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestParseY4MHeader(t *testing.T) {
	for _, tc := range []struct {
		header string
		w, h   int
		ok     bool
	}{
		{"YUV4MPEG2 W1280 H720 F31:1 Ip A1:1 C420mpeg2 XYSCSS=420MPEG2\n", 1280, 720, true},
		{"YUV4MPEG2 W1919 H1079 F30:1 Ip A1:1\n", 1919, 1079, true}, // C absent means 4:2:0
		{"YUV4MPEG2 W640 H480 C444\n", 0, 0, false},
		{"YUV4MPEG2 H480\n", 0, 0, false},
		{"P6 640 480\n", 0, 0, false},
	} {
		w, h, err := parseY4MHeader(tc.header)
		if (err == nil) != tc.ok || w != tc.w || h != tc.h {
			t.Errorf("%q: got %dx%d err=%v, want %dx%d ok=%v", tc.header, w, h, err, tc.w, tc.h, tc.ok)
		}
	}
}

func TestPipelineScalesOnlyWhenSizeKnown(t *testing.T) {
	scaled := strings.Join(pipeline(47, 1280, 720, 30), " ")
	for _, want := range []string{
		"pipewiresrc fd=3 path=47 keepalive-time=100",
		"video/x-raw,max-framerate=31/1",
		"video/x-raw,format=I420,colorimetry=bt601,width=1280,height=720,pixel-aspect-ratio=1/1",
		"y4menc ! fdsink fd=1",
	} {
		if !strings.Contains(scaled, want) {
			t.Errorf("pipeline %q lacks %q", scaled, want)
		}
	}
	if native := strings.Join(pipeline(47, 0, 0, 30), " "); strings.Contains(native, "width=") {
		t.Errorf("pipeline %q forces a size it does not know", native)
	}
}

func TestFit(t *testing.T) {
	for _, tc := range []struct{ w, h, maxW, maxH, wantW, wantH int }{
		{1280, 720, 1920, 1080, 1280, 720},
		{3840, 2160, 1920, 1080, 1920, 1080},
		{2560, 1600, 1920, 1080, 1728, 1080},
		{1366, 768, 1920, 1080, 1366, 768},
		{1367, 769, 1920, 1080, 1366, 768},
		{0, 0, 1920, 1080, 0, 0},
	} {
		if w, h := fit(tc.w, tc.h, tc.maxW, tc.maxH); w != tc.wantW || h != tc.wantH {
			t.Errorf("fit(%d, %d, %d, %d) = %dx%d, want %dx%d", tc.w, tc.h, tc.maxW, tc.maxH, w, h, tc.wantW, tc.wantH)
		}
	}
}

func TestCaptureSkipsRepeatedFrames(t *testing.T) {
	f := &frames{width: 2, height: 2}
	size := 6
	f.front, f.latest, f.back = make([]byte, size), make([]byte, size), make([]byte, size)
	deliver := func(b byte) {
		for i := range f.back {
			f.back[i] = b
		}
		f.back, f.latest = f.latest, f.back
		f.fresh = true
	}

	deliver(1)
	if fr, changed, err := f.capture(); err != nil || !changed || fr.Pix[0] != 1 {
		t.Fatalf("first frame: changed=%v err=%v", changed, err)
	}
	if _, changed, _ := f.capture(); changed {
		t.Fatal("no new frame, but capture reports a change")
	}
	deliver(1) // a keepalive resend
	if fr, changed, _ := f.capture(); changed || fr.Pix[0] != 1 {
		t.Fatal("an identical frame counts as a change")
	}
	deliver(2)
	deliver(3) // two frames between captures: only the newest is handed out
	if fr, changed, _ := f.capture(); !changed || fr.Pix[0] != 3 {
		t.Fatalf("got frame %d changed=%v, want frame 3 changed", fr.Pix[0], changed)
	}
}

func TestMimeListAcceptsGnomeTuple(t *testing.T) {
	want := []string{"text/plain", "image/png"}
	if got := mimeList(want); !slices.Equal(got, want) {
		t.Errorf("as: got %v", got)
	}
	if got := mimeList([]any{want}); !slices.Equal(got, want) {
		t.Errorf("(as): got %v", got)
	}
	if got := pickText([]string{"image/png", "UTF8_STRING", "text/plain"}); got != "text/plain" {
		t.Errorf("pickText chose %q", got)
	}
	if got := pickText([]string{"image/png"}); got != "" {
		t.Errorf("pickText chose %q from no text type", got)
	}
}

func TestKeysyms(t *testing.T) {
	for _, tc := range []struct {
		key, code string
		want      uint32
	}{
		{"Enter", "Enter", 0xff0d},
		{"Shift", "ShiftRight", xkShiftR},
		{"Control", "ControlLeft", xkControlL},
		{"F12", "F12", 0xffc9},
		{"AltGraph", "AltRight", xkISOLevel3Shift},
	} {
		if got, ok := namedKeysym(tc.key, tc.code); !ok || got != tc.want {
			t.Errorf("namedKeysym(%q, %q) = %#x, %v; want %#x", tc.key, tc.code, got, ok, tc.want)
		}
	}
	if _, ok := namedKeysym("CapsLock", "CapsLock"); ok {
		t.Error("CapsLock maps, but the browser already applied it")
	}
	if got := runeKeysym('é'); got != 0xe9 {
		t.Errorf("é = %#x", got)
	}
	if got := runeKeysym('€'); got != 0x10020ac {
		t.Errorf("€ = %#x", got)
	}
}

func TestWheelSteps(t *testing.T) {
	for _, tc := range []struct {
		acc   float64
		steps int32
		rest  float64
	}{
		{99, 0, 99},
		{100, 1, 0},
		{-250, -2, -50},
		{1e9, maxWheelSteps, 0}, // the excess is dropped, not queued
	} {
		steps, rest := wheelSteps(tc.acc)
		if steps != tc.steps || rest != tc.rest {
			t.Errorf("wheelSteps(%v) = %d, %v; want %d, %v", tc.acc, steps, rest, tc.steps, tc.rest)
		}
	}
}

func TestInputScalesFramePixelsToStream(t *testing.T) {
	in := newInput(&portalSession{width: 2560, height: 1440, devices: deviceKeyboard | devicePointer}, 1280, 720)
	if in.scaleX != 2 || in.scaleY != 2 || in.maxX != 2559 || in.maxY != 1439 {
		t.Errorf("scale %v,%v max %v,%v", in.scaleX, in.scaleY, in.maxX, in.maxY)
	}
	if in.viewOnly {
		t.Error("keyboard and pointer granted, but input is view only")
	}
	if !newInput(&portalSession{devices: devicePointer}, 640, 480).viewOnly {
		t.Error("pointer alone granted, but input is not view only")
	}
}

func TestTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if got := readToken(path); got != "" {
		t.Fatalf("no file, but token %q", got)
	}
	if err := saveToken(path, "abc-123"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	if got := readToken(path); got != "abc-123" {
		t.Errorf("read %q", got)
	}
	removeToken(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("token still there after remove: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Errorf("left behind %v", entries)
	}
}

func TestExplainNamesThePortalPackages(t *testing.T) {
	err := explain(dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"})
	if !strings.Contains(err.Error(), "xdg-desktop-portal-gnome") {
		t.Errorf("got %q", err)
	}
}
