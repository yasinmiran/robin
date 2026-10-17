// Package desktop defines what an agent session streams and drives, whether
// it is an X11 display the agent spawned or attached to, or a Wayland session
// reached through its portal.
package desktop

import (
	"errors"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

// ErrFixedSize is returned by Resize when the desktop's size belongs to
// someone else, as for a session the agent attached to.
var ErrFixedSize = errors.New("desktop: size belongs to the attached session")

// Frame is one captured picture: 4 bytes per pixel in B, G, R, X order, rows
// Stride bytes apart. Implementations may reuse the buffer between captures.
type Frame struct {
	Width, Height, Stride int
	Pix                   []byte
}

// Desktop is a screen with input. Methods are safe for concurrent use.
type Desktop interface {
	Size() (w, h int)
	// Resize asks for a new size and reports the one it got.
	Resize(w, h int) (gotW, gotH int, err error)
	// Capture returns the current frame; changed is false when nothing was
	// drawn since the last capture and force is false.
	Capture(force bool) (f *Frame, changed bool, err error)
	// Input applies a key, pointer, wheel or release message.
	Input(m protocol.Control) error
	SetClipboard(text string) error
	// Clipboard delivers clipboard text set by programs on the desktop. It is
	// closed when the desktop goes away.
	Clipboard() <-chan string
	// Cursor delivers pointer image changes and is closed when the desktop
	// goes away. A nil channel means the desktop cannot report them and the
	// browser keeps its own pointer.
	Cursor() <-chan protocol.Cursor
	Close() error
}
