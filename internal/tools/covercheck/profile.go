package main

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
)

// span is where a block sits in its file, the key it merges on.
type span struct {
	file                                 string
	startLine, startCol, endLine, endCol int
}

type block struct {
	stmts   int
	covered bool
}

// profile is a set of coverage blocks merged from any number of text
// profiles, whatever their mode: a block counts as covered when any input ran it.
type profile map[span]block

var modes = []string{"set", "count", "atomic"}

// add merges one text profile, as go test -coverprofile, a test binary's
// -test.coverprofile or go tool covdata textfmt writes it, into p.
func (p profile) add(r io.Reader, name string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	moded := false
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Concatenated profiles repeat the mode line.
		if mode, ok := strings.CutPrefix(line, "mode: "); ok {
			if !slices.Contains(modes, mode) {
				return fmt.Errorf("%s:%d: unknown mode %q", name, n, mode)
			}
			moded = true
			continue
		}
		if !moded {
			return fmt.Errorf("%s:%d: no mode line before the first block", name, n)
		}
		s, b, err := parseBlock(line)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", name, n, err)
		}
		if old, ok := p[s]; ok {
			if old.stmts != b.stmts {
				return fmt.Errorf("%s:%d: %s has %d statements here and %d in an earlier profile", name, n, s, b.stmts, old.stmts)
			}
			b.covered = b.covered || old.covered
		}
		p[s] = b
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// parseBlock reads "file:startLine.startCol,endLine.endCol statements count".
func parseBlock(line string) (span, block, error) {
	i := strings.LastIndexByte(line, ':')
	if i <= 0 {
		return span{}, block{}, fmt.Errorf("malformed block %q", line)
	}
	s := span{file: line[:i]}
	var b block
	var count int64
	var rest string
	n, _ := fmt.Sscanf(line[i+1:], "%d.%d,%d.%d %d %d%s", &s.startLine, &s.startCol, &s.endLine, &s.endCol, &b.stmts, &count, &rest)
	if n != 6 || b.stmts < 0 || count < 0 {
		return span{}, block{}, fmt.Errorf("malformed block %q", line)
	}
	b.covered = count > 0
	return s, b, nil
}

func (s span) String() string {
	return fmt.Sprintf("%s:%d.%d,%d.%d", s.file, s.startLine, s.startCol, s.endLine, s.endCol)
}

// write prints p as one profile in set mode, which go tool cover reads
// for -func and -html.
func (p profile) write(w io.Writer) error {
	spans := make([]span, 0, len(p))
	for s := range p {
		spans = append(spans, s)
	}
	slices.SortFunc(spans, func(a, b span) int {
		return cmp.Or(
			strings.Compare(a.file, b.file),
			cmp.Compare(a.startLine, b.startLine),
			cmp.Compare(a.startCol, b.startCol),
			cmp.Compare(a.endLine, b.endLine),
			cmp.Compare(a.endCol, b.endCol),
		)
	})
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "mode: set")
	for _, s := range spans {
		count := 0
		if p[s].covered {
			count = 1
		}
		fmt.Fprintf(bw, "%s %d %d\n", s, p[s].stmts, count)
	}
	return bw.Flush()
}
