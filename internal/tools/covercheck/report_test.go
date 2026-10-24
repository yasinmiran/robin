package main

import (
	"strings"
	"testing"
)

const sample = `mode: set
example.com/m/a/x.go:1.1,2.2 3 1
example.com/m/a/x.go:3.1,4.2 1 0
example.com/m/a/y.go:1.1,2.2 4 0
example.com/m/b/z.go:1.1,2.2 2 1
other.org/q/q.go:1.1,2.2 2 0
`

func TestSummarize(t *testing.T) {
	p := profile{}
	mustAdd(t, p, sample)
	s := summarize(p, "example.com/m")
	for name, want := range map[string]total{
		"a/x.go":           {stmts: 4, covered: 3},
		"a/y.go":           {stmts: 4},
		"b/z.go":           {stmts: 2, covered: 2},
		"other.org/q/q.go": {stmts: 2},
	} {
		if got := s.files[name]; got != want {
			t.Errorf("file %s: %+v, want %+v", name, got, want)
		}
	}
	for name, want := range map[string]total{
		"a":           {stmts: 8, covered: 3},
		"b":           {stmts: 2, covered: 2},
		"other.org/q": {stmts: 2},
	} {
		if got := s.packages[name]; got != want {
			t.Errorf("package %s: %+v, want %+v", name, got, want)
		}
	}
	if len(s.files) != 4 || len(s.packages) != 3 {
		t.Errorf("%d files, %d packages; want 4 and 3", len(s.files), len(s.packages))
	}
	if want := (total{stmts: 12, covered: 5}); s.all != want {
		t.Errorf("total %+v, want %+v", s.all, want)
	}
}

func TestSummarizeWithoutModule(t *testing.T) {
	p := profile{}
	mustAdd(t, p, sample)
	s := summarize(p, "")
	if _, ok := s.packages["example.com/m/a"]; !ok {
		t.Fatalf("packages %v lack the full path example.com/m/a", s.packages)
	}
}

func TestPercent(t *testing.T) {
	for _, c := range []struct {
		t    total
		want float64
	}{
		{total{}, 0},
		{total{stmts: 4}, 0},
		{total{stmts: 4, covered: 1}, 25},
		{total{stmts: 3, covered: 3}, 100},
	} {
		if got := c.t.percent(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.t, got, c.want)
		}
	}
}

func TestPrint(t *testing.T) {
	p := profile{}
	mustAdd(t, p, sample)
	var b strings.Builder
	if err := summarize(p, "example.com/m").print(&b); err != nil {
		t.Fatal(err)
	}
	want := `a/x.go                         3/4   75.0%
a/y.go                         0/4    0.0%
b/z.go                         2/2  100.0%
other.org/q/q.go               0/2    0.0%

a                              3/8   37.5%
b                              2/2  100.0%
other.org/q                    0/2    0.0%

total (statements)            5/12   41.7%
`
	if got := b.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrintWidensForLongNames(t *testing.T) {
	p := profile{}
	long := "example.com/m/" + strings.Repeat("d", 30) + "/f.go"
	mustAdd(t, p, "mode: set\n"+long+":1.1,1.2 1 1\n")
	var b strings.Builder
	if err := summarize(p, "example.com/m").print(&b); err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(b.String(), "\n")
	if want := strings.Repeat("d", 30) + "/f.go             1/1  100.0%"; first != want {
		t.Fatalf("first row %q, want %q", first, want)
	}
}
