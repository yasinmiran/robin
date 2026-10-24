package main

import (
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
)

// total counts statements over blocks: all of them, and those that ran.
// go tool cover -func can read a little lower, as it leaves out blocks
// outside any function declaration, such as a package-level func literal.
type total struct{ stmts, covered int }

func (t total) percent() float64 {
	if t.stmts == 0 {
		return 0
	}
	return 100 * float64(t.covered) / float64(t.stmts)
}

func (t *total) add(b block) {
	t.stmts += b.stmts
	if b.covered {
		t.covered += b.stmts
	}
}

type summary struct {
	files, packages map[string]total
	all             total
}

// summarize totals p per file and per package, naming both relative to
// module when they sit inside it.
func summarize(p profile, module string) summary {
	s := summary{files: map[string]total{}, packages: map[string]total{}}
	for sp, b := range p {
		file := trimModule(sp.file, module)
		pkg := path.Dir(file)
		f, k := s.files[file], s.packages[pkg]
		f.add(b)
		k.add(b)
		s.files[file], s.packages[pkg] = f, k
		s.all.add(b)
	}
	return s
}

func trimModule(file, module string) string {
	if module == "" {
		return file
	}
	if rest, ok := strings.CutPrefix(file, module+"/"); ok {
		return rest
	}
	return file
}

// print writes every file, then every package, then the total.
func (s summary) print(w io.Writer) error {
	const all = "total (statements)"
	width := len(all)
	for name := range s.files {
		width = max(width, len(name))
	}
	var b strings.Builder
	row := func(name string, t total) {
		fmt.Fprintf(&b, "%-*s %15s %6.1f%%\n", width, name, fmt.Sprintf("%d/%d", t.covered, t.stmts), t.percent())
	}
	for _, rows := range []map[string]total{s.files, s.packages} {
		for _, name := range slices.Sorted(maps.Keys(rows)) {
			row(name, rows[name])
		}
		b.WriteString("\n")
	}
	row(all, s.all)
	_, err := io.WriteString(w, b.String())
	return err
}
