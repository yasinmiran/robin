// Command covercheck merges Go coverage profiles, prints statement coverage
// per file and per package, and holds every package to a baseline:
//
//	covercheck [-o merged.out] [-module path] [-baseline file [-write] [-tolerance points]] profile|dir...
//
// A directory stands for every *.out file beneath it. Profiles may mix
// modes (set, count, atomic); a block is covered when any of them ran it.
// With -baseline, a package that falls more than -tolerance points below
// its baseline line, or vanishes, fails the run; with -write as well, the
// run rewrites the baseline instead. make cover runs it over every suite.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// errDrop marks a run that worked but found packages below the baseline.
var errDrop = errors.New("coverage fell below the baseline")

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fl.SetOutput(stderr)
	out := fl.String("o", "", "write the merged profile, in set mode, to this file")
	module := fl.String("module", "", "module path to strip from file and package names")
	baseline := fl.String("baseline", "", "per-package baseline to compare with")
	write := fl.Bool("write", false, "rewrite the -baseline file from this run instead of comparing")
	tolerance := fl.Float64("tolerance", 0.5, "points a package may fall below its baseline")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() == 0 || (*write && *baseline == "") {
		fmt.Fprintln(stderr, "usage: covercheck [-o merged.out] [-module path] [-baseline file [-write] [-tolerance points]] profile|dir...")
		return 2
	}
	err := check(fl.Args(), *out, *module, *baseline, *write, *tolerance, stdout)
	switch {
	case errors.Is(err, errDrop):
		fmt.Fprintln(stderr, "covercheck:", err)
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "covercheck:", err)
		return 2
	}
	return 0
}

func check(inputs []string, out, module, baseline string, write bool, tolerance float64, stdout io.Writer) error {
	files, err := profileFiles(inputs, out)
	if err != nil {
		return err
	}
	p := profile{}
	for _, f := range files {
		if err := addFile(p, f); err != nil {
			return err
		}
	}
	if out != "" {
		if err := writeFile(out, p.write); err != nil {
			return err
		}
	}
	s := summarize(p, module)
	if err := s.print(stdout); err != nil {
		return err
	}
	switch {
	case baseline == "":
		return nil
	case write:
		if err := writeFile(baseline, func(w io.Writer) error { return writeBaseline(w, s.packages) }); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nwrote %d packages to %s\n", len(s.packages), baseline)
		return nil
	}
	f, err := os.Open(baseline)
	if err != nil {
		return err
	}
	defer f.Close()
	base, err := readBaseline(f, baseline)
	if err != nil {
		return err
	}
	drops, added := compare(base, s.packages, tolerance)
	if len(added) > 0 {
		fmt.Fprintf(stdout, "\nnot in %s yet:\n  %s\n", baseline, strings.Join(added, "\n  "))
	}
	if len(drops) > 0 {
		fmt.Fprintf(stdout, "\nbelow %s:\n  %s\n", baseline, strings.Join(drops, "\n  "))
		return fmt.Errorf("%w in %s (%d of its packages)", errDrop, baseline, len(drops))
	}
	fmt.Fprintf(stdout, "\nno package below %s\n", baseline)
	return nil
}

// profileFiles expands directories into the *.out files beneath them,
// leaving out the merged output so a rerun does not read its own result.
func profileFiles(inputs []string, out string) ([]string, error) {
	skip := ""
	if out != "" {
		skip, _ = filepath.Abs(out)
	}
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
			if err != nil || d.IsDir() || filepath.Ext(p) != ".out" {
				return err
			}
			if abs, _ := filepath.Abs(p); abs != skip {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no profiles in %s", strings.Join(inputs, ", "))
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

func addFile(p profile, name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return p.add(f, name)
}

// writeFile writes through a temporary file, so a reader never sees half a
// profile or baseline.
func writeFile(name string, fill func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".covercheck-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := errors.Join(fill(tmp), tmp.Chmod(0o644), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}
