package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWriteFile(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCheck(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs strings.Builder
	code = run(args, &out, &errs)
	return code, out.String(), errs.String()
}

// suites lays out profiles as make cover does: a directory per suite and a
// stray file that is not a profile.
func suites(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "profiles", "unit.out"), "mode: set\nexample.com/m/a/x.go:1.1,2.2 3 1\nexample.com/m/a/x.go:3.1,4.2 1 0\n")
	mustWriteFile(t, filepath.Join(dir, "profiles", "x", "agent.out"), "mode: atomic\nexample.com/m/a/x.go:3.1,4.2 1 5\nexample.com/m/b/y.go:1.1,2.2 4 0\n")
	mustWriteFile(t, filepath.Join(dir, "profiles", "x", "notes.txt"), "not a profile\n")
	return dir
}

func TestRunMergesDirectoriesAndWritesProfile(t *testing.T) {
	dir := suites(t)
	merged := filepath.Join(dir, "profiles", "merged.out")
	// A stale merged profile inside a walked directory must not be read back.
	mustWriteFile(t, merged, "mode: set\nexample.com/m/stale/s.go:1.1,2.2 9 1\n")
	code, stdout, stderr := runCheck(t, "-o", merged, "-module", "example.com/m", filepath.Join(dir, "profiles"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"a/x.go", "4/4  100.0%", "b/y.go", "0/4    0.0%", "total (statements)", "4/8   50.0%"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "stale") {
		t.Errorf("the old merged profile was read back:\n%s", stdout)
	}
	b, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	want := "mode: set\nexample.com/m/a/x.go:1.1,2.2 3 1\nexample.com/m/a/x.go:3.1,4.2 1 1\nexample.com/m/b/y.go:1.1,2.2 4 0\n"
	if string(b) != want {
		t.Fatalf("merged profile:\n%s\nwant:\n%s", b, want)
	}
}

func TestRunHeaderOnlyBaselinePasses(t *testing.T) {
	dir := suites(t)
	base := mustWriteFile(t, filepath.Join(dir, "baseline.txt"), baselineHeader)
	code, stdout, stderr := runCheck(t, "-module", "example.com/m", "-baseline", base, filepath.Join(dir, "profiles"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"not in " + base + " yet:", "a: 100.0%, not in the baseline yet", "no package below " + base} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestRunDropFails(t *testing.T) {
	dir := suites(t)
	base := mustWriteFile(t, filepath.Join(dir, "baseline.txt"), "a 50.0\nb 0.6\n")
	code, stdout, stderr := runCheck(t, "-module", "example.com/m", "-baseline", base, filepath.Join(dir, "profiles"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, stderr)
	}
	if want := "b: 0.0%, 0.6 points below the baseline's 0.6%"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
	if want := "covercheck: coverage fell below the baseline in " + base + " (1 of its packages)"; !strings.Contains(stderr, want) {
		t.Errorf("stderr %q lacks %q", stderr, want)
	}
	// A wider tolerance lets the same run through.
	if code, _, stderr := runCheck(t, "-module", "example.com/m", "-baseline", base, "-tolerance", "1", filepath.Join(dir, "profiles")); code != 0 {
		t.Fatalf("with -tolerance 1: exit %d: %s", code, stderr)
	}
}

func TestRunWriteBaseline(t *testing.T) {
	dir := suites(t)
	base := filepath.Join(dir, "baseline.txt")
	code, stdout, stderr := runCheck(t, "-module", "example.com/m", "-baseline", base, "-write", filepath.Join(dir, "profiles"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if want := "wrote 2 packages to " + base; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
	b, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if want := baselineHeader + "a 100.0\nb 0.0\n"; string(b) != want {
		t.Fatalf("baseline:\n%s\nwant:\n%s", b, want)
	}
	fi, err := os.Stat(base)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("baseline mode %v, %v; want 0644", fi.Mode(), err)
	}
	// The written baseline holds this run exactly, so checking against it passes.
	if code, _, stderr := runCheck(t, "-module", "example.com/m", "-baseline", base, filepath.Join(dir, "profiles")); code != 0 {
		t.Fatalf("check against the written baseline: exit %d: %s", code, stderr)
	}
}

func TestRunNamedFiles(t *testing.T) {
	dir := suites(t)
	// A file named twice counts once.
	unit := filepath.Join(dir, "profiles", "unit.out")
	code, stdout, stderr := runCheck(t, unit, unit)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "example.com/m/a/x.go") || strings.Contains(stdout, "y.go") {
		t.Fatalf("stdout:\n%s", stdout)
	}
}

func TestRunUsageAndErrors(t *testing.T) {
	dir := suites(t)
	profiles := filepath.Join(dir, "profiles")
	empty := t.TempDir()
	bad := mustWriteFile(t, filepath.Join(t.TempDir(), "bad.out"), "no mode\n")
	badBase := mustWriteFile(t, filepath.Join(dir, "bad-baseline.txt"), "a\n")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no profiles":         {nil, "usage: covercheck"},
		"write without base":  {[]string{"-write", profiles}, "usage: covercheck"},
		"unknown flag":        {[]string{"-frobnicate", profiles}, "flag provided but not defined"},
		"missing input":       {[]string{filepath.Join(dir, "nope")}, "no such file"},
		"empty directory":     {[]string{empty}, "no profiles in " + empty},
		"malformed profile":   {[]string{bad}, "bad.out:1: no mode line"},
		"missing baseline":    {[]string{"-baseline", filepath.Join(dir, "nope.txt"), profiles}, "nope.txt"},
		"malformed baseline":  {[]string{"-baseline", badBase, profiles}, "bad-baseline.txt:1"},
		"unwritable output":   {[]string{"-o", filepath.Join(dir, "no", "such", "merged.out"), profiles}, "no such file"},
		"unwritable baseline": {[]string{"-baseline", filepath.Join(dir, "no", "base.txt"), "-write", profiles}, "no such file"},
		"path through a file": {[]string{filepath.Join(dir, "profiles", "unit.out", "x")}, "not a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runCheck(t, c.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr %q lacks %q", stderr, c.want)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	code, stdout, stderr := runCheck(t, "-h")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q; want 0 and nothing", code, stdout)
	}
	for _, want := range []string{"usage: covercheck", "-tolerance"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestRunUsageListsFlags(t *testing.T) {
	code, _, stderr := runCheck(t)
	if code != 2 || !strings.Contains(stderr, "usage: covercheck") || !strings.Contains(stderr, "-baseline") {
		t.Fatalf("exit %d, stderr %q; want 2, the synopsis and the flags", code, stderr)
	}
}

func TestProfileFilesNoWorkingDirectory(t *testing.T) {
	profiles := filepath.Join(suites(t), "profiles")
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := profileFiles([]string{profiles}, "merged.out"); err == nil {
		t.Fatal("a relative -o resolved without a working directory")
	}
}

func TestProfileFilesWalkError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := suites(t)
	locked := filepath.Join(dir, "profiles", "locked")
	mustWriteFile(t, filepath.Join(locked, "a.out"), "mode: set\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := profileFiles([]string{filepath.Join(dir, "profiles")}, ""); err == nil {
		t.Fatal("walking an unreadable directory succeeded")
	}
}
