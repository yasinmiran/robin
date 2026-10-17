//go:build linux

package display

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestXAuthorityFiles(t *testing.T) {
	dir := t.TempDir()
	home, rt := filepath.Join(dir, "home"), filepath.Join(dir, "run")
	touch := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := touch("elsewhere/auth")
	dot := touch("home/.Xauthority")
	gdm := touch("run/gdm/Xauthority")
	mutter := touch("run/.mutter-Xwaylandauth.ABC123")
	sddm := touch("run/xauth_xyz")
	if err := os.Mkdir(filepath.Join(rt, "xauth_dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got, want := xauthorityFiles(env, home, rt), []string{env, dot, gdm, mutter, sddm}; !slices.Equal(got, want) {
		t.Errorf("all present:\n got %q\nwant %q", got, want)
	}
	// $XAUTHORITY naming ~/.Xauthority is listed once; missing files not at all.
	os.Remove(gdm)
	if got, want := xauthorityFiles(dot, home, rt), []string{dot, mutter, sddm}; !slices.Equal(got, want) {
		t.Errorf("dedup and missing:\n got %q\nwant %q", got, want)
	}
	if got := xauthorityFiles(filepath.Join(dir, "nope"), "", filepath.Join(dir, "none")); len(got) != 0 {
		t.Errorf("nothing there: got %q", got)
	}
}
