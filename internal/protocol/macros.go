package protocol

// TypeMacros carries the user's key macros between the CLI and the page.
// From the browser it asks for one change in Message.MacroChange; the CLI
// answers with the macros the page shows after it in Message.Macros, with
// Macros.Error set when the change failed. The CLI keeps the macros, not
// the page: its origin is a new loopback port every session, and the
// browser's storage goes with it. Never relayed to the agent, which replays
// nothing itself: the page plays a macro as key and text controls.
const TypeMacros Type = "macros"

// Macros is what a session's page shows: the macros pinned to its host and
// those pinned to none. It rides in Config too.
type Macros struct {
	// Host is the host argument as the user typed it, which a macro pinned
	// in this session names.
	Host string  `json:"host"`
	List []Macro `json:"list"`
	// Error says why the macros could not be read or the last change made.
	// While the file cannot be read, the CLI changes nothing.
	Error string `json:"error,omitempty"`
}

// MacroChange is one edit from the page: Put adds a macro or replaces the
// one with its ID, Delete removes the macro with that ID. Exactly one is
// set.
type MacroChange struct {
	Put    *Macro `json:"put,omitempty"`
	Delete string `json:"delete,omitempty"`
}

// Macro is a named run of steps the page plays on the remote desktop.
type Macro struct {
	// ID is the page's own name for the macro, MacroIDChars long at most.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Host pins the macro to the host argument it was made with; empty
	// shows it on every host.
	Host  string      `json:"host,omitempty"`
	Steps []MacroStep `json:"steps"`
}

// MacroStep is a chord, a run of text or a pause: exactly one is set.
type MacroStep struct {
	// Keys are pressed in order and released in reverse, as Control key
	// messages carry them.
	Keys []MacroKey `json:"keys,omitempty"`
	// Text is typed as ControlText, in pieces of MaxTextBytes.
	Text string `json:"text,omitempty"`
	// Pause is how many milliseconds to wait.
	Pause int `json:"pause,omitempty"`
}

// MacroKey is one key of a chord: KeyboardEvent.key and .code.
type MacroKey struct {
	Key  string `json:"key"`
	Code string `json:"code,omitempty"`
}

// Bounds on the macros file and what the page may put in it. The page
// restates them, so it never sends what the CLI would refuse; the file cap
// keeps one change well inside a websocket message (MaxLineBytes).
const (
	MaxMacrosBytes    = 256 << 10
	MaxMacros         = 100
	MaxMacroSteps     = 100
	MaxMacroChordKeys = 8
	MaxMacroNameBytes = 64
	MaxMacroTextBytes = 16 << 10
	MaxMacroPauseMs   = 60_000
	// MaxMacroKeyBytes bounds a key's Key and Code, and MaxMacroHostBytes
	// the host a macro is pinned to.
	MaxMacroKeyBytes  = 32
	MaxMacroHostBytes = 255
	MacroIDChars      = 32
)
