//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// tailEvery is how often the proxy looks for new lines in the daemon's log.
const tailEvery = 200 * time.Millisecond

// finalLogGrace is how long the proxy keeps tailing after its connection
// ended, for what the daemon logs about this controller leaving.
const finalLogGrace = tailEvery

// Proxy connects to the daemon in dir and carries in to it and it to out,
// byte for byte, until either side ends or ctx does. The CLI's pings and
// the daemon's answers pass through untouched, so the two keep each other's
// idle watchdogs as if on one pipe. Lines the daemon logs meanwhile go to
// logOut, from the start of the log when fromStart is set, else from where
// it stands now.
func Proxy(ctx context.Context, dir string, in io.Reader, out, logOut io.Writer, fromStart bool) error {
	c, err := net.Dial("unix", filepath.Join(dir, SocketFile))
	if err != nil {
		return fmt.Errorf("%w: %v", &NoSessionError{filepath.Base(dir)}, err)
	}
	defer c.Close()

	tailCtx, stopTail := context.WithCancel(ctx)
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		tailLog(tailCtx, filepath.Join(dir, LogFile), logOut, fromStart)
	}()

	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(c, in)
		done <- err
	}()
	go func() {
		_, err := io.Copy(out, c)
		done <- err
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = nil
	}
	c.Close()
	time.Sleep(finalLogGrace)
	stopTail()
	<-tailDone
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// tailLog copies lines appended to path to w until ctx ends. The file may
// not exist yet, or stop existing when the daemon removes its directory.
func tailLog(ctx context.Context, path string, w io.Writer, fromStart bool) {
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	buf := make([]byte, 16<<10)
	drain := func() {
		if f == nil {
			if g, err := os.Open(path); err == nil {
				f = g
				if !fromStart {
					f.Seek(0, io.SeekEnd)
				}
			}
		}
		for f != nil {
			n, err := f.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}
	t := time.NewTicker(tailEvery)
	defer t.Stop()
	for {
		drain()
		select {
		case <-ctx.Done():
			// What the daemon wrote since the last tick still counts.
			drain()
			return
		case <-t.C:
		}
	}
}
