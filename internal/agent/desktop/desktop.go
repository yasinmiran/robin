// Package desktop defines what an agent session streams and drives, whether
// it is an X11 display the agent spawned or attached to, or a Wayland session
// reached through its portal.
package desktop

import (
	"errors"

	"github.com/yasinmiran/robin/internal/protocol"
)

// ErrFixedSize is returned by Resize when the desktop's size belongs to
// someone else, as for a session the agent attached to.
var ErrFixedSize = errors.New("desktop: size belongs to the attached session")

// Frame is one captured picture laid out as Format says. Implementations may
// reuse the buffer between captures.
type Frame struct {
	Width, Height, Stride int
	Pix                   []byte
	Format                Format
}

// Format is the pixel layout of a Frame.
type Format int

const (
	// BGRX is 4 bytes per pixel in B, G, R, X order, rows Stride bytes apart.
	BGRX Format = iota
	// I420 is planar 4:2:0: Height rows of Y, Stride bytes apart, then
	// (Height+1)/2 rows each of U and V, (Stride+1)/2 bytes apart.
	I420
)

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

// MultiMonitor is a desktop that spans several monitors and shows one of
// them, or all of them as one picture, at a time.
type MultiMonitor interface {
	// MonitorState returns the monitors and the one shown.
	MonitorState() protocol.Monitors
	// MonitorUpdates is signalled whenever MonitorState changes, and closed
	// when the desktop goes away.
	MonitorUpdates() <-chan struct{}
	// ShowMonitor shows the named monitor, or the whole desktop for
	// protocol.MonitorAll.
	ShowMonitor(name string) error
}

// RichClipboard is a desktop whose clipboard carries HTML and images as
// well as text. Its Clipboard channel still reports the text of each copy.
type RichClipboard interface {
	// SetClipboardContent offers c as the clipboard, in every format it
	// holds.
	SetClipboardContent(c protocol.Clip) error
	// ClipboardContent delivers what programs on the desktop copy, in each
	// format of protocol.Clip they offer, up to protocol.MaxClip in all.
	// Only the latest is kept if the reader falls behind. It is closed when
	// the desktop goes away.
	ClipboardContent() <-chan protocol.Clip
}
