package gui

import (
	"fmt"
	"io"

	"github.com/yasinmiran/robin/internal/macros"
)

// userMacros opens the user's macros for a session to dest, the host
// argument as typed, or returns nil, for a session that keeps none, when
// the user has no config directory to keep them in.
func userMacros(dest string, stderr io.Writer) *macros.Store {
	path, err := macros.Path()
	if err != nil {
		fmt.Fprintf(stderr, "robin: macros are not kept: %v\n", err)
		return nil
	}
	return openMacros(macros.New(path, dest), stderr)
}

// openMacros tells the user once when the macros file does not read: the
// page then offers none, and nothing overwrites the file.
func openMacros(s *macros.Store, stderr io.Writer) *macros.Store {
	if _, err := s.Load(); err != nil {
		fmt.Fprintf(stderr, "robin: %v\n", err)
	}
	return s
}
