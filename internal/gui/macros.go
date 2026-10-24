package gui

import (
	"fmt"
	"io"

	"github.com/yasinmiran/robin/internal/macros"
)

// openMacros opens the user's macros for a session to dest, the host
// argument as typed, and tells the user once when the file does not read:
// the page then offers none, and nothing overwrites the file.
func openMacros(dest string, stderr io.Writer) *macros.Store {
	s := macros.New(dest)
	if m := s.Load(); m.Error != "" {
		fmt.Fprintf(stderr, "robin: macros: %s\n", m.Error)
	}
	return s
}
