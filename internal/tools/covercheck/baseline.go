package main

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const baselineHeader = `# Statement coverage per package, in percent, from the merged make cover
# on main. make cover fails when a package falls more than 0.5 points below
# its line; make cover-baseline rewrites this file from the last run.
`

// readBaseline parses "package percent" lines; # starts a comment.
func readBaseline(r io.Reader, name string) (map[string]float64, error) {
	base := map[string]float64{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"package percent\", have %q", name, n, sc.Text())
		}
		pct, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || pct < 0 || pct > 100 {
			return nil, fmt.Errorf("%s:%d: %q is not a percentage", name, n, fields[1])
		}
		if _, dup := base[fields[0]]; dup {
			return nil, fmt.Errorf("%s:%d: %s is listed twice", name, n, fields[0])
		}
		base[fields[0]] = pct
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return base, nil
}

// writeBaseline records every package of the run, sorted.
func writeBaseline(w io.Writer, packages map[string]total) error {
	var b strings.Builder
	b.WriteString(baselineHeader)
	for _, pkg := range slices.Sorted(maps.Keys(packages)) {
		fmt.Fprintf(&b, "%s %.1f\n", pkg, packages[pkg].percent())
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// compare returns a line for every baseline package that dropped more than
// tolerance points or vanished, and one for every package the baseline lacks.
// Only the first kind fails a run: a new package has nothing to drop from.
func compare(base map[string]float64, packages map[string]total, tolerance float64) (drops, added []string) {
	for _, pkg := range slices.Sorted(maps.Keys(base)) {
		want := base[pkg]
		t, ok := packages[pkg]
		switch {
		case !ok:
			drops = append(drops, fmt.Sprintf("%s: in the baseline at %.1f%% but in no profile; if the package is gone, make cover-baseline", pkg, want))
		// The baseline holds one decimal; the slack keeps a run that
		// matches it exactly from failing on float rounding.
		case t.percent() < want-tolerance-1e-9:
			drops = append(drops, fmt.Sprintf("%s: %.1f%%, %.1f points below the baseline's %.1f%%", pkg, t.percent(), want-t.percent(), want))
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(packages)) {
		if _, ok := base[pkg]; !ok {
			added = append(added, fmt.Sprintf("%s: %.1f%%, not in the baseline yet", pkg, packages[pkg].percent()))
		}
	}
	return drops, added
}
