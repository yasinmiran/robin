package macros

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// openFile opens path to read.
func openFile(path string) (*os.File, error) {
	return os.Open(path)
}

// openLock opens the lock file beside the macros, making it if need be.
func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// privateDir makes dir as private as Windows modes go; the user's profile
// keeps others out of it.
func privateDir(dir string) error {
	return os.Chmod(dir, 0o700)
}

// ownedAlone has nothing to check: the file's ACL comes from the user's
// profile.
func ownedAlone(fs.FileInfo) error { return nil }

// tryLock takes f's exclusive lock if no one holds it.
func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
