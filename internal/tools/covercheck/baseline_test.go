package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCommittedBaselineParses(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "coverage-baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := readBaseline(f, f.Name()); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	packages := map[string]total{
		"b":   {stmts: 3, covered: 1},
		"a":   {stmts: 8, covered: 3},
		"c/d": {stmts: 0},
	}
	var b strings.Builder
	if err := writeBaseline(&b, packages); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), baselineHeader) {
		t.Fatalf("baseline lacks its header:\n%s", b.String())
	}
	if body, want := strings.TrimPrefix(b.String(), baselineHeader), "a 37.5\nb 33.3\nc/d 0.0\n"; body != want {
		t.Fatalf("body %q, want %q", body, want)
	}
	got, err := readBaseline(strings.NewReader(b.String()), "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]float64{"a": 37.5, "b": 33.3, "c/d": 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %v, want %v", got, want)
	}
}

func TestReadHeaderOnlyBaseline(t *testing.T) {
	got, err := readBaseline(strings.NewReader(baselineHeader+"\n   \n"), "baseline")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an empty baseline", got, err)
	}
}

func TestReadBaselineTrailingComment(t *testing.T) {
	got, err := readBaseline(strings.NewReader("a 50.0 # was 40 before the rewrite\n"), "baseline")
	if err != nil || got["a"] != 50 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestReadBaselineRejects(t *testing.T) {
	for name, text := range map[string]string{
		"one field":     "a\n",
		"three fields":  "a 1 2\n",
		"not a number":  "a lots\n",
		"over 100":      "a 100.1\n",
		"negative":      "a -1\n",
		"listed twice":  "a 1\nb 2\na 3\n",
		"percent sign?": "a 50%\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readBaseline(strings.NewReader(text), "base.txt")
			if err == nil || !strings.HasPrefix(err.Error(), "base.txt:") {
				t.Fatalf("err = %v, want one naming base.txt and the line", err)
			}
		})
	}
}

func TestReadBaselineReadError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := readBaseline(iotest.ErrReader(boom), "base.txt"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestWriteBaselineError(t *testing.T) {
	if err := writeBaseline(failWriter{}, map[string]total{"a": {}}); err == nil {
		t.Fatal("write to a failing writer succeeded")
	}
}

func TestCompare(t *testing.T) {
	base := map[string]float64{
		"same":    50,
		"edge":    50,
		"dropped": 50,
		"gone":    10,
		"better":  10,
	}
	packages := map[string]total{
		"same":    {stmts: 2, covered: 1},      // 50.0
		"edge":    {stmts: 1000, covered: 495}, // 49.5: exactly at the tolerance
		"dropped": {stmts: 1000, covered: 494}, // 49.4
		"better":  {stmts: 1, covered: 1},
		"new":     {stmts: 4, covered: 1},
	}
	drops, added := compare(base, packages, 0.5)
	wantDrops := []string{
		"dropped: 49.4%, 0.6 points below the baseline's 50.0%",
		"gone: in the baseline at 10.0% but in no profile; if the package is gone, make cover-baseline",
	}
	if !reflect.DeepEqual(drops, wantDrops) {
		t.Errorf("drops:\n%q\nwant:\n%q", drops, wantDrops)
	}
	if want := []string{"new: 25.0%, not in the baseline yet"}; !reflect.DeepEqual(added, want) {
		t.Errorf("added %q, want %q", added, want)
	}
}

func TestCompareEmptyBaseline(t *testing.T) {
	drops, added := compare(map[string]float64{}, map[string]total{"a": {stmts: 1}, "b": {stmts: 1}}, 0.5)
	if len(drops) != 0 || len(added) != 2 {
		t.Fatalf("drops %q, added %q; want no drops and two added", drops, added)
	}
}
