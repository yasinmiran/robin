//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yasinmiran/robin/internal/agent/files"
)

// send is robin-agent send, and robin-send: it offers the files named in
// args to the session files.FindSocket picks under runRoot until each is
// settled or ctx ends, and returns the exit status.
func send(ctx context.Context, args []string, runRoot string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, "usage: robin-send FILE...\n\nOffers each FILE to the browser of this robin session, which saves it when you click Save there.")
		return 0
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	sock, err := files.FindSocket(getenv, runRoot)
	if err == nil {
		err = files.Send(ctx, sock, args, stdout)
	}
	switch {
	case errors.Is(err, files.ErrNotSaved):
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "robin-send:", err)
		if ctx.Err() != nil {
			return 130 // as a shell reports a command Ctrl+C stopped
		}
		return 1
	}
	return 0
}
