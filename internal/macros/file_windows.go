package macros

import (
	"io/fs"
	"os"
)

// openFile opens path to read.
func openFile(path string) (*os.File, error) {
	return os.Open(path)
}

// privateDir makes dir as private as Windows modes go; the user's profile
// keeps others out of it.
func privateDir(dir string) error {
	return os.Chmod(dir, 0o700)
}

// ownedAlone has nothing to check: the file's ACL comes from the user's
// profile.
func ownedAlone(fs.FileInfo) error { return nil }
