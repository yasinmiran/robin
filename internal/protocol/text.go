package protocol

// ControlText goes from browser to agent with text to type as it stands, in
// Control.Text: what an input method composed, or what a soft keyboard
// typed without a key behind it. It holds no key down, and the agent types
// characters the remote keymap lacks as well.
const ControlText ControlType = "text"

// MaxTextBytes bounds the text of one ControlText message; the page splits
// longer text. Every character can take a keymap change on the host, so
// the bound keeps one message from holding up the input behind it for long.
const MaxTextBytes = 4096
