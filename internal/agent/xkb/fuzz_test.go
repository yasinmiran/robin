package xkb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// symbolSeeds are the test layouts, which hold every form the parser
// knows, and the files TestParseErrors refuses.
func symbolSeeds(f *testing.F) []string {
	f.Helper()
	var seeds []string
	for _, name := range []string{"us", "latin", "pc", "ru", "broken", "vndr/us"} {
		b, err := os.ReadFile(filepath.Join("testdata", "symbols", name))
		if err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, string(b))
	}
	return append(seeds,
		`/* open`, `xkb_symbols "a`, `xkb_symbols "a" { key <AC01 { [ a ] }`, `xkb_symbols { };`,
		`xkb_symbols "a" { key <AC01> { [ { a, b }, { c ] }; };`,
		`xkb_symbols "a" { include "fz(b)+fz(b)|fz(b):2" }; xkb_symbols "b" { key <AC01> { [ a, A ] }; };`,
		`default xkb_symbols "a" { include "fz(a)" };`,
		`xkb_symbols "a" { name = "x"; name[Group1] = "y"; key <AC01> { symbols[Group1] = [ 0x61 ] }; };`,
	)
}

// FuzzParse feeds the parser symbols and keycodes files: whatever they
// hold, it ends without a panic, and every key it reads is named <...>.
func FuzzParse(f *testing.F) {
	for _, s := range symbolSeeds(f) {
		f.Add(s)
	}
	b, err := os.ReadFile(filepath.Join("testdata", "keycodes", "evdev"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(string(b))
	f.Fuzz(func(t *testing.T, src string) {
		secs, err := parse(src)
		if err == nil {
			for _, sec := range secs {
				for _, s := range sec.stmts {
					if s.key != "" && (!strings.HasPrefix(s.key, "<") || !strings.HasSuffix(s.key, ">")) {
						t.Fatalf("read the key %q", s.key)
					}
				}
			}
		}
		parseKeycodes(src)
	})
}

// FuzzLoad loads a layout from a symbols file of any contents beside the
// test layouts, which it may include, itself too. Whatever the file
// holds, Load ends within a bound, and a layout it gives has only the
// levels mutter can type within maxLevel or past it.
func FuzzLoad(f *testing.F) {
	for _, s := range symbolSeeds(f) {
		f.Add(s, "")
	}
	f.Add(`xkb_symbols "a" { include "fz(b)+fz(b)" }; xkb_symbols "b" { include "us" };`, "a")
	f.Add(fanOut(4, 4), "s0")
	f.Fuzz(func(t *testing.T, src, variant string) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "symbols"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "symbols", "fz"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		l, err := Load([]string{dir, "testdata"}, "fz", variant)
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("Load took %s", took)
		}
		if err != nil {
			return
		}
		for sym, level := range l.levels {
			if level < 0 || sym == "" {
				t.Fatalf("the layout types %q on level %d", sym, level)
			}
		}
	})
}

// fanOut is a symbols file fz whose section s0 includes s1 ways times,
// each of those s2 as often, down to depth.
func fanOut(ways, depth int) string {
	var b strings.Builder
	for i := range depth {
		refs := make([]string, ways)
		for k := range refs {
			refs[k] = fmt.Sprintf("fz(s%d)", i+1)
		}
		fmt.Fprintf(&b, "xkb_symbols \"s%d\" { include \"%s\" };\n", i, strings.Join(refs, "+"))
	}
	fmt.Fprintf(&b, "xkb_symbols \"s%d\" { key <AC01> { [ a, A ] }; };\n", depth)
	return b.String()
}

// TestIncludesAreBounded: includes may each nest maxDepth deep, but a
// file whose sections include the next several times over would have
// Load apply them ways^depth times. Load counts every include it applies
// and gives up past maxIncludes.
func TestIncludesAreBounded(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "symbols"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "symbols", "fz"), []byte(fanOut(8, maxDepth)), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := Load([]string{dir, "testdata"}, "fz", "s0")
	if err == nil || !strings.Contains(err.Error(), "includes") {
		t.Errorf("Load of an 8-way fan-out %d deep: %v, want too many includes", maxDepth, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Load took %s", took)
	}
	// A fan-out within the bound still loads.
	if err := os.WriteFile(filepath.Join(dir, "symbols", "fz"), []byte(fanOut(2, 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := Load([]string{dir, "testdata"}, "fz", "s0"); err != nil || !l.Has('a') {
		t.Errorf("Load of a 2-way fan-out 4 deep: %v", err)
	}
}
