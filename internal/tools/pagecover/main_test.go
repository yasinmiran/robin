package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	appJS   = "start();\nif (later) {\n  never();\n}\n"
	toolsJS = "export const a = 1;\nexport const b = 2;\n"
)

// staticDir is a page with two scripts, a test directory and a non-script.
func staticDir(t *testing.T) string {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"app.js":             appJS,
		"tools.js":           toolsJS,
		"style.css":          "body {}\n",
		"test/tools.test.js": "import './tools.js';\n",
	} {
		writeFile(t, filepath.Join(dir, name), text)
	}
	return dir
}

func writeFile(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCoverage(t *testing.T, path string, scripts ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"result": scripts})
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, path, string(b))
}

func script(url string, source *string, ranges ...v8Range) map[string]any {
	s := map[string]any{"url": url, "functions": []map[string]any{{"ranges": ranges}}}
	if source != nil {
		s["source"] = *source
	}
	return s
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func ptr(s string) *string { return &s }

func runPagecover(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errs strings.Builder
	code := run(args, &out, &errs)
	return code, out.String(), errs.String()
}

func TestRunMergesChromeAndNode(t *testing.T) {
	static := staticDir(t)
	cov := t.TempDir()
	// Chrome served app.js and ran all but the if block; it also ran a
	// script of another page and one from a subdirectory, both ignored.
	writeCoverage(t, filepath.Join(cov, "e2e", "direct", "page", "gui.json"),
		script("http://127.0.0.1:4321/app.js", ptr(appJS), v8Range{0, len(appJS), 1}, v8Range{22, 37, 0}),
		script("http://127.0.0.1:4321/other.js", ptr("x();\n"), v8Range{0, 5, 1}),
		script("http://127.0.0.1:4321/test/tools.test.js", nil, v8Range{0, 21, 1}),
	)
	// Node ran tools.js from disk, its test file, and its own internals.
	writeCoverage(t, filepath.Join(cov, "unit", "coverage-1.json"),
		script(fileURL(filepath.Join(static, "tools.js")), nil, v8Range{0, 20, 1}),
		script(fileURL(filepath.Join(static, "test", "tools.test.js")), nil, v8Range{0, 21, 1}),
		script("node:internal/main", nil, v8Range{0, 1, 1}),
		script("%%bad url", nil),
	)
	writeFile(t, filepath.Join(cov, "unit", "notes.txt"), "not coverage\n")

	code, stdout, stderr := runPagecover(t, "-static", static, cov)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	app, tools := filepath.Join(static, "app.js"), filepath.Join(static, "tools.js")
	for _, want := range []string{
		app + " ", "2/3   66.7%",
		tools + " ", "1/2   50.0%",
		"total (lines)", "3/5   60.0%",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	for _, unwanted := range []string{"other.js", "test", "style.css"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("stdout names %s:\n%s", unwanted, stdout)
		}
	}
}

func TestRunNamedFileAndNeverLoaded(t *testing.T) {
	static := staticDir(t)
	cov := writeCoverage(t, filepath.Join(t.TempDir(), "only.cov"),
		script("https://example.test/tools.js", ptr(toolsJS), v8Range{0, len(toolsJS), 1}))
	code, stdout, stderr := runPagecover(t, "-static", static, cov)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "0/3    0.0%") || !strings.Contains(stdout, "2/2  100.0%") {
		t.Fatalf("want app.js at 0/3 and tools.js at 2/2:\n%s", stdout)
	}
}

func TestRunStaleSource(t *testing.T) {
	static := staticDir(t)
	cov := writeCoverage(t, filepath.Join(t.TempDir(), "gui.json"),
		script("http://127.0.0.1:1/app.js", ptr("start(); // an older app.js\n"), v8Range{0, 10, 1}))
	code, _, stderr := runPagecover(t, "-static", static, cov)
	if code != 2 || !strings.Contains(stderr, "ran another version of app.js") {
		t.Fatalf("exit %d, stderr %q; want a stale source error", code, stderr)
	}
}

func TestRunErrors(t *testing.T) {
	static := staticDir(t)
	good := writeCoverage(t, filepath.Join(t.TempDir(), "good.json"))
	bad := writeFile(t, filepath.Join(t.TempDir(), "bad.json"), "{not json")
	empty := t.TempDir()
	noScripts := t.TempDir()
	writeFile(t, filepath.Join(noScripts, "index.html"), "<p>\n")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no inputs":         {[]string{"-static", static}, "usage: pagecover"},
		"unknown flag":      {[]string{"-frobnicate", good}, "flag provided but not defined"},
		"missing static":    {[]string{"-static", filepath.Join(empty, "nope"), good}, "no such file"},
		"static no scripts": {[]string{"-static", noScripts, good}, "no scripts in " + noScripts},
		"missing input":     {[]string{"-static", static, filepath.Join(empty, "nope.json")}, "no such file"},
		"empty directory":   {[]string{"-static", static, empty}, "no coverage in " + empty},
		"malformed":         {[]string{"-static", static, bad}, "bad.json"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runPagecover(t, c.args...)
			if code != 2 || !strings.Contains(stderr, c.want) {
				t.Errorf("exit %d, stderr %q; want 2 and %q", code, stderr, c.want)
			}
		})
	}
}

func TestRunUnreadableScript(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any file")
	}
	static := staticDir(t)
	if err := os.Chmod(filepath.Join(static, "app.js"), 0); err != nil {
		t.Fatal(err)
	}
	good := writeCoverage(t, filepath.Join(t.TempDir(), "good.json"))
	if code, _, stderr := runPagecover(t, "-static", static, good); code != 2 || !strings.Contains(stderr, "permission denied") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestScriptName(t *testing.T) {
	static := "/srv/page"
	for raw, want := range map[string]string{
		"http://127.0.0.1:8080/app.js":    "app.js",
		"https://host/overlay.js":         "overlay.js",
		"http://host/sub/app.js":          "",
		"file:///srv/page/app.js":         "app.js",
		"file:///srv/page/test/x.test.js": "",
		"file:///elsewhere/app.js":        "",
		"node:internal/modules/run_main":  "",
		"":                                "",
		"%zz":                             "",
	} {
		if got := scriptName(raw, static); got != want {
			t.Errorf("scriptName(%q) = %q, want %q", raw, got, want)
		}
	}
}
