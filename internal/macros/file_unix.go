//go:build unix

package macros

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

var errLink = errors.New("a symbolic link, which Robin does not follow")

// openFile opens path to read. It follows no symbolic link and does not
// wait on a FIFO, so the descriptor's own stat says what was opened.
func openFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, errLink
	}
	return f, err
}

// privateDir makes dir 0700 once it has opened it and found it a directory
// of the user's own, not a link: a link could name anyone's.
func privateDir(dir string) error {
	f, err := openFile(dir)
	if errors.Is(err, errLink) {
		return fmt.Errorf("%s is %w", dir, errLink)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := mine(st); err != nil {
		return fmt.Errorf("%s is %w; Robin keeps macros in a directory of your own", dir, err)
	}
	return f.Chmod(0o700)
}

// mine reports why fi, as the descriptor's stat gave it, is not the
// user's own.
func mine(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("of no owner Robin can tell")
	}
	if euid := os.Geteuid(); int(st.Uid) != euid {
		return fmt.Errorf("owned by uid %d, and you are uid %d", st.Uid, euid)
	}
	return nil
}

// ownedAlone reports why the file fi stats is not one only the user can
// change.
func ownedAlone(fi fs.FileInfo) error {
	if err := mine(fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errors.New("others can write it")
	}
	return nil
}
