package protocol

// Control types of a private session on an attached desktop. The browser
// sends ControlCurtain and ControlSilent with Control.On to switch the
// curtain or the silent room; the agent answers with ControlPrivate when
// the control channel opens and whenever either switch changes.
const (
	ControlCurtain ControlType = "curtain"
	ControlSilent  ControlType = "silent"
	ControlPrivate ControlType = "private"
)

// Private is the state of an attached desktop's private session: the
// curtain blacks out the screens at the host while the stream still shows
// them, and the silent room keeps the host's speakers quiet while the
// stream still carries the sound.
type Private struct {
	Curtain PrivateSwitch `json:"curtain"`
	Silent  PrivateSwitch `json:"silent"`
}

// PrivateSwitch is one switch of a private session.
type PrivateSwitch struct {
	On bool `json:"on,omitempty"`
	// Unsupported says why this desktop cannot do it at all.
	Unsupported string `json:"unsupported,omitempty"`
	// Error says why the last change failed.
	Error string `json:"error,omitempty"`
}
