// Text from the host that the page shows: the names of files and monitors,
// and the reasons that come with them. It goes in as text, never as markup,
// but the browser still applies the invisible marks in it that reorder what
// follows, so a host could make a name read as another.

// REORDERS is what runes.ReordersText reports: the bidi controls and the
// Unicode line and paragraph separators.
const REORDERS = /[\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069\u2028\u2029]/g;

// shown is s with each of those as a visible escape, as the CLI prints
// them on the terminal.
export function shown(s) {
  return String(s).replace(REORDERS, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`);
}
