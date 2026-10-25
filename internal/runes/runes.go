// Package runes names the characters that make text read as other text,
// which neither a file name the agent takes nor host text the CLI prints
// may carry as they are.
package runes

import "unicode"

// ReordersText reports whether r rearranges how the text around it reads
// without showing itself: an invisible bidi mark, which reorders the text
// after it, or a Unicode line or paragraph separator, which breaks a line
// without being a control character. Any of them can make a name, a path
// or a command read as another.
func ReordersText(r rune) bool {
	return unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
}
