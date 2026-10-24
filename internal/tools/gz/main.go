// Command gz writes a gzip of one file whose bytes depend on the input and
// the Go version alone: no name, no time, no OS byte, best compression.
// The Makefile compresses the agents with it so the CLI that embeds them
// is reproducible, which a host gzip does not promise.
package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gz SRC DST")
		os.Exit(2)
	}
	if err := compress(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "gz:", err)
		os.Exit(1)
	}
}

func compress(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if err := Write(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// Write compresses r into w with a fixed header.
func Write(w io.Writer, r io.Reader) error {
	z, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	z.OS = 0xff // unknown, the RFC 1952 value for "no particular system"
	if _, err := io.Copy(z, r); err != nil {
		return err
	}
	return z.Close()
}
