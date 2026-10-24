// Command pagecover prints line coverage for every script of the page:
//
//	pagecover -static internal/web/static coverage.json|dir...
//
// Each input holds V8's precise coverage, {"result": [script...]}: what
// Chrome ran in the e2e tests (Playwright's page.coverage) and what Node ran
// in the page's unit tests (NODE_V8_COVERAGE). A directory stands for every
// *.json file beneath it. Scripts are matched to the .js files of -static by
// name, served over http or loaded from a file:// URL inside that directory;
// anything else is ignored. A line counts when it holds code, not only a
// comment or closing brackets, and is covered when any input ran any of it.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("pagecover", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() {
		fmt.Fprintln(fl.Output(), "usage: pagecover [-static dir] coverage.json|dir...")
		fl.PrintDefaults()
	}
	static := fl.String("static", "internal/web/static", "directory of the page's scripts")
	switch err := fl.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	}
	if fl.NArg() == 0 {
		fl.Usage()
		return 2
	}
	if err := report(*static, fl.Args(), stdout); err != nil {
		fmt.Fprintln(stderr, "pagecover:", err)
		return 2
	}
	return 0
}

func report(static string, inputs []string, stdout io.Writer) error {
	pages, err := loadPages(static)
	if err != nil {
		return err
	}
	files, err := jsonFiles(inputs)
	if err != nil {
		return err
	}
	absStatic, err := filepath.Abs(static)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := addFile(pages, absStatic, f); err != nil {
			return err
		}
	}
	return printPages(stdout, static, pages)
}

// v8Coverage is the result of the profiler's takePreciseCoverage, the
// shape both Node and the e2e helper write.
type v8Coverage struct {
	Result []v8Script `json:"result"`
}

type v8Script struct {
	URL string `json:"url"`
	// Source is there in Playwright's coverage, not in Node's.
	Source    *string `json:"source"`
	Functions []struct {
		Ranges []v8Range `json:"ranges"`
	} `json:"functions"`
}

// v8Range offsets count UTF-16 code units, as JavaScript strings do.
type v8Range struct {
	Start int `json:"startOffset"`
	End   int `json:"endOffset"`
	Count int `json:"count"`
}

func addFile(pages map[string]*page, absStatic, name string) error {
	b, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	var c v8Coverage
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for _, s := range c.Result {
		p := pages[scriptName(s.URL, absStatic)]
		if p == nil {
			continue
		}
		if s.Source != nil && *s.Source != p.source {
			return fmt.Errorf("%s: %s ran another version of %s; rerun the suite that wrote it", name, s.URL, p.name)
		}
		var ranges []v8Range
		for _, f := range s.Functions {
			ranges = append(ranges, f.Ranges...)
		}
		p.ran(ranges)
	}
	return nil
}

// scriptName returns the file name a script URL loads from the static
// directory, or "" for any other script. absStatic must be absolute and
// clean, as a file:// URL's directory is.
func scriptName(raw, absStatic string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	var name string
	switch u.Scheme {
	case "http", "https":
		name = strings.TrimPrefix(u.Path, "/")
	case "file":
		dir, file := filepath.Split(filepath.FromSlash(u.Path))
		if filepath.Clean(dir) != absStatic {
			return ""
		}
		name = file
	default:
		return ""
	}
	if strings.ContainsRune(name, '/') {
		return ""
	}
	return name
}

func loadPages(static string) (map[string]*page, error) {
	entries, err := os.ReadDir(static)
	if err != nil {
		return nil, err
	}
	pages := map[string]*page{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".js" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(static, e.Name()))
		if err != nil {
			return nil, err
		}
		pages[e.Name()] = newPage(e.Name(), string(b))
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no scripts in %s", static)
	}
	return pages, nil
}

// jsonFiles expands directories into the *.json files beneath them.
func jsonFiles(inputs []string) ([]string, error) {
	var files []string
	for _, in := range inputs {
		fi, err := os.Stat(in)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			files = append(files, in)
			continue
		}
		err = filepath.WalkDir(in, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".json" {
				return err
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no coverage in " + strings.Join(inputs, ", "))
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

func printPages(w io.Writer, static string, pages map[string]*page) error {
	const all = "total (lines)"
	width := len(all)
	names := slices.Sorted(maps.Keys(pages))
	for _, n := range names {
		width = max(width, len(filepath.Join(static, n)))
	}
	var b strings.Builder
	row := func(name string, covered, lines int) {
		pct := 0.0
		if lines > 0 {
			pct = 100 * float64(covered) / float64(lines)
		}
		fmt.Fprintf(&b, "%-*s %15s %6.1f%%\n", width, name, fmt.Sprintf("%d/%d", covered, lines), pct)
	}
	covered, lines := 0, 0
	for _, n := range names {
		c, l := pages[n].lines()
		row(filepath.Join(static, n), c, l)
		covered += c
		lines += l
	}
	b.WriteString("\n")
	row(all, covered, lines)
	_, err := io.WriteString(w, b.String())
	return err
}
