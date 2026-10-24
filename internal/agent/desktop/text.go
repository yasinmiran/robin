package desktop

import "context"

// Typist is a desktop that types text as it stands, characters missing from
// its keymap included, as an input method on the desktop itself would.
type Typist interface {
	// Type types text. It checks ctx before each character it types, or
	// before a paste of the whole, and once ctx has ended it stops, having
	// typed part of the text or none of it, and returns ctx's error. It may
	// wait on the desktop, so it is not for the peer's read loop.
	Type(ctx context.Context, text string) error
}
