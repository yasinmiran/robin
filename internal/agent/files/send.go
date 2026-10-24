//go:build linux

package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ErrNotSaved means robin-agent send ended with a file the browser did not
// save.
var ErrNotSaved = errors.New("files: not every file was saved")

// FindSocket picks the session robin-agent send offers to: the one named
// by SocketEnv, else the one live session under runRoot. It refuses to
// guess between several.
func FindSocket(getenv func(string) string, runRoot string) (string, error) {
	if path := getenv(SocketEnv); path != "" {
		return path, nil
	}
	paths, _ := filepath.Glob(filepath.Join(runRoot, "*", SocketName))
	var live []string
	for _, p := range paths {
		// A session that died without cleaning up leaves a socket nobody
		// answers.
		if c, err := net.Dial("unixpacket", p); err == nil {
			c.Close()
			live = append(live, p)
		}
	}
	switch len(live) {
	case 0:
		return "", errors.New("no robin session is running here")
	case 1:
		return live[0], nil
	}
	return "", fmt.Errorf("%d robin sessions are running here; run this inside the one to send to, where %s is set", len(live), SocketEnv)
}

// Send offers the files at paths to the browser of the session listening
// at sock, and reports to out what becomes of each until every one is
// saved, declined or refused. It returns ErrNotSaved when any was not
// saved. Interrupting it withdraws the files still on offer.
func Send(sock string, paths []string, out io.Writer) error {
	switch {
	case len(paths) == 0:
		return errors.New("no files to send")
	case len(paths) > maxOffer:
		return fmt.Errorf("%d files; send at most %d at a time", len(paths), maxOffer)
	}
	req := request{Names: make([]string, len(paths))}
	fds := make([]int, len(paths))
	for i, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil {
			return err
		} else if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		req.Names[i], fds[i] = filepath.Base(p), int(f.Fd())
	}
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		return fmt.Errorf("reach the robin session: %w", err)
	}
	defer c.Close()
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, _, err := c.WriteMsgUnix(b, unix.UnixRights(fds...), nil); err != nil {
		return fmt.Errorf("offer the files: %w", err)
	}
	return follow(c, req.Names, out)
}

// follow prints the session's reports until every file is settled.
func follow(c *net.UnixConn, names []string, out io.Writer) error {
	settled := make([]Status, len(names))
	left := len(names)
	buf := make([]byte, maxRequest)
	for left > 0 {
		n, err := c.Read(buf)
		if err != nil {
			return errors.New("the robin session ended with files still on offer")
		}
		var rep report
		if err := json.Unmarshal(buf[:n], &rep); err != nil {
			return fmt.Errorf("malformed report from the session: %w", err)
		}
		if rep.I == -1 {
			return fmt.Errorf("the session refused the offer: %s", rep.Detail)
		}
		if rep.I < 0 || rep.I >= len(names) || settled[rep.I].Final() {
			continue
		}
		fmt.Fprintln(out, describe(names[rep.I], rep))
		if rep.Status.Final() {
			settled[rep.I] = rep.Status
			left--
		}
	}
	for _, s := range settled {
		if s != Saved {
			return ErrNotSaved
		}
	}
	return nil
}

// Main is robin-agent send, and robin-send: it offers the files named in
// args to the session FindSocket picks under runRoot and returns the exit
// status.
func Main(args []string, runRoot string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, "usage: robin-send FILE...\n\nOffers each FILE to the browser of this robin session, which saves it when you click Save there.")
		return 0
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	sock, err := FindSocket(getenv, runRoot)
	if err == nil {
		err = Send(sock, args, stdout)
	}
	switch {
	case errors.Is(err, ErrNotSaved):
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "robin-send:", err)
		return 1
	}
	return 0
}

func describe(name string, rep report) string {
	rep.Detail = printable(rep.Detail)
	switch rep.Status {
	case Offered:
		return name + ": offered; click Save in the browser to download it, or press Ctrl+C here to withdraw it"
	case Saved:
		return name + ": saved by the browser"
	case Declined:
		return name + ": declined in the browser"
	case Failed:
		return name + ": transfer failed, still on offer: " + rep.Detail
	case Refused:
		return name + ": not offered: " + rep.Detail
	}
	return fmt.Sprintf("%s: %s %s", name, printable(string(rep.Status)), rep.Detail)
}

// printable replaces control characters, since a failure's detail can carry
// the browser's own words to this terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return utf8.RuneError
		}
		return r
	}, s)
}
