package keysym

// Text returns the keysym that types r within a text: a character's own,
// Return for a newline and Tab for a tab. Other control characters, a
// carriage return among them, type nothing.
func Text(r rune) (uint32, bool) {
	switch {
	case r == '\n':
		return namedKeys["Enter"], true
	case r == '\t':
		return namedKeys["Tab"], true
	case r < 0x20, r >= 0x7f && r < 0xa0:
		return 0, false
	}
	return Rune(r), true
}
