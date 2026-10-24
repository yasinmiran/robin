package main

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

func mustAdd(t *testing.T, p profile, text string) {
	t.Helper()
	if err := p.add(strings.NewReader(text), "test.out"); err != nil {
		t.Fatal(err)
	}
}

func written(t *testing.T, p profile) string {
	t.Helper()
	var b strings.Builder
	if err := p.write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestMergeORsAcrossModes(t *testing.T) {
	p := profile{}
	mustAdd(t, p, `mode: set
m/a/a.go:1.1,2.2 2 0
m/a/a.go:3.1,4.2 1 1
m/b/b.go:5.1,6.2 3 0
`)
	// A -race test binary writes atomic counts; textfmt writes set.
	mustAdd(t, p, `mode: atomic
m/a/a.go:1.1,2.2 2 7
m/a/a.go:3.1,4.2 1 0
`)
	want := `mode: set
m/a/a.go:1.1,2.2 2 1
m/a/a.go:3.1,4.2 1 1
m/b/b.go:5.1,6.2 3 0
`
	if got := written(t, p); got != want {
		t.Fatalf("merged:\n%s\nwant:\n%s", got, want)
	}
}

func TestMergeRepeatedModeLines(t *testing.T) {
	// go test over several packages, or cat, repeats the mode line.
	p := profile{}
	mustAdd(t, p, "mode: count\nm/a.go:1.1,1.9 1 0\n\nmode: count\nm/a.go:1.1,1.9 1 3\n")
	if got, want := written(t, p), "mode: set\nm/a.go:1.1,1.9 1 1\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWriteSortsByPosition(t *testing.T) {
	p := profile{}
	mustAdd(t, p, `mode: set
m/b.go:1.1,1.2 1 0
m/a.go:10.1,10.2 1 0
m/a.go:2.5,3.1 1 0
m/a.go:2.1,2.4 1 0
m/a.go:2.1,2.3 1 0
`)
	want := `mode: set
m/a.go:2.1,2.3 1 0
m/a.go:2.1,2.4 1 0
m/a.go:2.5,3.1 1 0
m/a.go:10.1,10.2 1 0
m/b.go:1.1,1.2 1 0
`
	if got := written(t, p); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFileNameWithColon(t *testing.T) {
	p := profile{}
	mustAdd(t, p, "mode: set\nC:/m/a.go:1.1,1.2 4 1\n")
	if got, want := written(t, p), "mode: set\nC:/m/a.go:1.1,1.2 4 1\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAddRejects(t *testing.T) {
	for name, text := range map[string]string{
		"no mode":           "m/a.go:1.1,1.2 1 0\n",
		"unknown mode":      "mode: sometimes\n",
		"no colon":          "mode: set\nm/a.go 1 0\n",
		"empty file name":   "mode: set\n:1.1,1.2 1 0\n",
		"short":             "mode: set\nm/a.go:1.1,1.2 1\n",
		"not a number":      "mode: set\nm/a.go:1.1,1.x 1 0\n",
		"trailing field":    "mode: set\nm/a.go:1.1,1.2 1 0 9\n",
		"negative count":    "mode: set\nm/a.go:1.1,1.2 1 -1\n",
		"negative stmts":    "mode: set\nm/a.go:1.1,1.2 -1 0\n",
		"inconsistent size": "mode: set\nm/a.go:1.1,1.2 1 0\nm/a.go:1.1,1.2 2 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := profile{}.add(strings.NewReader(text), "bad.out")
			if err == nil || !strings.HasPrefix(err.Error(), "bad.out") {
				t.Fatalf("err = %v, want one naming bad.out", err)
			}
		})
	}
}

func TestAddReadError(t *testing.T) {
	boom := errors.New("boom")
	err := profile{}.add(iotest.ErrReader(boom), "x.out")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteError(t *testing.T) {
	p := profile{}
	mustAdd(t, p, "mode: set\nm/a.go:1.1,1.2 1 0\n")
	if err := p.write(failWriter{}); err == nil {
		t.Fatal("write to a failing writer succeeded")
	}
}
