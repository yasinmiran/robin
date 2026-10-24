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

type options struct {
	out, module, baseline string
	write                 bool
	tolerance             float64
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fl := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() {
		fmt.Fprintln(fl.Output(), "usage: covercheck [-o merged.out] [-module path] [-baseline file [-write] [-tolerance points]] profile|dir...")
		fl.PrintDefaults()
	}
	fl.StringVar(&o.out, "o", "", "write the merged profile, in set mode, to this file")
	fl.StringVar(&o.module, "module", "", "module path to strip from file and package names")
	fl.StringVar(&o.baseline, "baseline", "", "per-package baseline to compare with")
	fl.BoolVar(&o.write, "write", false, "rewrite the -baseline file from this run instead of comparing")
	fl.Float64Var(&o.tolerance, "tolerance", 0.5, "points a package may fall below its baseline")
	switch err := fl.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	}
	if fl.NArg() == 0 || (o.write && o.baseline == "") {
		fl.Usage()
		return 2
	}
	err := check(fl.Args(), o, stdout)
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

func check(inputs []string, o options, stdout io.Writer) error {
	files, err := profileFiles(inputs, o.out)
	if err != nil {
		return err
	}
	p := profile{}
	for _, f := range files {
		if err := addFile(p, f); err != nil {
			return err
		}
	}
	if o.out != "" {
		if err := writeFile(o.out, p.write); err != nil {
			return err
		}
	}
	s := summarize(p, o.module)
	if err := s.print(stdout); err != nil {
		return err
	}
	switch {
	case o.baseline == "":
		return nil
	case o.write:
		return writeBaselineFile(o.baseline, s, stdout)
	default:
		return compareBaseline(o.baseline, o.tolerance, s, stdout)
	}
}

func writeBaselineFile(name string, s summary, stdout io.Writer) error {
	if err := writeFile(name, func(w io.Writer) error { return writeBaseline(w, s.packages) }); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nwrote %d packages to %s\n", len(s.packages), name)
	return nil
}

func compareBaseline(name string, tolerance float64, s summary, stdout io.Writer) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	base, err := readBaseline(f, name)
	if err != nil {
		return err
	}
	drops, added := compare(base, s.packages, tolerance)
	if len(added) > 0 {
		fmt.Fprintf(stdout, "\nnot in %s yet:\n  %s\n", name, strings.Join(added, "\n  "))
	}
	if len(drops) > 0 {
		fmt.Fprintf(stdout, "\nbelow %s:\n  %s\n", name, strings.Join(drops, "\n  "))
		return fmt.Errorf("%w in %s (%d of its packages)", errDrop, name, len(drops))
	}
	fmt.Fprintf(stdout, "\nno package below %s\n", name)
	return nil
}

// profileFiles expands directories into the *.out files beneath them,
// leaving out the merged output so a rerun does not read its own result.
func profileFiles(inputs []string, out string) ([]string, error) {
	skip := ""
	if out != "" {
		var err error
		if skip, err = filepath.Abs(out); err != nil {
			return nil, err
		}
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
			abs, err := filepath.Abs(p)
			if err == nil && abs != skip {
				files = append(files, p)
			}
			return err
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
