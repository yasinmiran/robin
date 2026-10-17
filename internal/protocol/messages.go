// Package protocol defines the messages the remotejump CLI, the host agent and
// the browser client exchange.
//
// Signaling (Message) travels as JSON lines over the agent's stdin and stdout,
// which ride the user's SSH connection, and between the CLI and the browser over
// a localhost websocket. Input, clipboard and resize (Control) travel over the
// WebRTC data channel labelled ControlChannel.
package protocol

// Version is bumped on any incompatible change to the messages in this package.
// The CLI refuses an agent that reports a different version.
const Version = 1

// ControlChannel is the label of the data channel that carries Control messages.
const ControlChannel = "control"

// Type identifies a signaling message.
type Type string

// Signaling message types, grouped by who sends them.
const (
	// CLI to agent.
	TypeStart  Type = "start"
	TypeHangup Type = "hangup" // drop the current peer connection, keep the display
	TypeBye    Type = "bye"

	// Agent to CLI.
	TypeReady Type = "ready"
	TypeError Type = "error"

	// Relayed between browser and agent by the CLI.
	TypeOffer     Type = "offer"
	TypeAnswer    Type = "answer"
	TypeCandidate Type = "candidate"

	// CLI to browser.
	TypeConfig Type = "config"

	// Either direction on the agent's stdio. Consumed by Conn, never relayed.
	TypePing Type = "ping"
)

// Message is one signaling message. Only the fields that belong to Type are set.
type Message struct {
	Type      Type       `json:"type"`
	Start     *Start     `json:"start,omitempty"`
	Ready     *Ready     `json:"ready,omitempty"`
	Config    *Config    `json:"config,omitempty"`
	SDP       string     `json:"sdp,omitempty"`
	Candidate *Candidate `json:"candidate,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Start asks the agent to bring up a display and get ready for an offer.
type Start struct {
	Version   int `json:"version"`
	Width     int `json:"width"`
	Height    int `json:"height"`
	MaxWidth  int `json:"maxWidth"`
	MaxHeight int `json:"maxHeight"`
	FPS       int `json:"fps"`
	MaxKbps   int `json:"maxKbps"`
	// Session is the command to run inside a spawned display. Empty lets the
	// agent pick the lightest desktop it finds.
	Session string   `json:"session,omitempty"`
	STUN    []string `json:"stun,omitempty"`
	// Attach names a running desktop to stream instead of spawning one: an
	// X11 display such as ":0", or AttachWayland. Empty spawns.
	Attach string `json:"attach,omitempty"`
	// Audio asks for the desktop's sound as an Opus track.
	Audio bool `json:"audio,omitempty"`
	// Encoder picks the video encoder by name; empty is EncoderSoftware.
	Encoder string `json:"encoder,omitempty"`
}

// Ready reports that the display is up and the agent accepts offers.
type Ready struct {
	Version    int    `json:"version"`
	AgentBuild string `json:"agentBuild"`
	Display    string `json:"display"`
	Session    string `json:"session"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	// TunnelAddr is where the agent accepts ICE-TCP on the host, for the CLI to
	// forward over SSH: "host:port", or a path for a unix socket. Empty when the
	// agent offers no tunnel.
	TunnelAddr string `json:"tunnelAddr,omitempty"`
	// Mode says how the desktop was obtained: ModeSpawn or ModeAttach.
	Mode string `json:"mode"`
	// Audio reports whether an audio track will be offered.
	Audio bool `json:"audio"`
	// Encoder is the video encoder in use.
	Encoder string `json:"encoder"`
}

// Config tells the browser how to build its RTCPeerConnection.
type Config struct {
	ICEServers []ICEServer `json:"iceServers"`
}

// ICEServer mirrors the browser's RTCIceServer.
type ICEServer struct {
	URLs []string `json:"urls"`
}

// Candidate mirrors the browser's RTCIceCandidateInit. An empty Candidate string
// marks the end of gathering.
type Candidate struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// ControlType identifies a data channel message.
type ControlType string

// Data channel message types. Clipboard flows both ways, heartbeats from agent
// to browser, the rest from browser to agent.
const (
	ControlKey       ControlType = "key"
	ControlMove      ControlType = "move"
	ControlButton    ControlType = "button"
	ControlWheel     ControlType = "wheel"
	ControlResize    ControlType = "resize"
	ControlClipboard ControlType = "clipboard"
	ControlRelease   ControlType = "release" // release every held key and button
	// ControlHeartbeat goes from agent to browser every second. The page
	// restarts ICE when heartbeats stop, which catches a path that died in
	// one direction only.
	ControlHeartbeat ControlType = "heartbeat"
	// ControlCursor goes from agent to browser when the remote pointer image
	// changes.
	ControlCursor ControlType = "cursor"
)

// Control is one message on the control data channel. Coordinates are in
// remote display pixels; the browser scales from the video element.
type Control struct {
	T ControlType `json:"t"`

	// Key: the browser's KeyboardEvent.key and .code.
	Down   bool   `json:"down,omitempty"`
	Repeat bool   `json:"repeat,omitempty"`
	Key    string `json:"key,omitempty"`
	Code   string `json:"code,omitempty"`

	// Move and Button. Button is the browser's MouseEvent.button.
	X      int `json:"x,omitempty"`
	Y      int `json:"y,omitempty"`
	Button int `json:"button,omitempty"`

	// Wheel, in pixels as the browser reports them.
	DX float64 `json:"dx,omitempty"`
	DY float64 `json:"dy,omitempty"`

	// Resize, in CSS pixels of the client viewport.
	W int `json:"w,omitempty"`
	H int `json:"h,omitempty"`

	// Clipboard text.
	Text string `json:"text,omitempty"`

	// Cursor.
	Cursor *Cursor `json:"cursor,omitempty"`
}

// Values for Start.Attach, Ready.Mode and Start.Encoder.
const (
	AttachWayland = "wayland"

	ModeSpawn  = "spawn"
	ModeAttach = "attach"

	EncoderSoftware = "software"
)

// Cursor is the remote pointer image. The agent sends each distinct image
// once with its PNG; later switches back to it carry only the ID.
type Cursor struct {
	ID   uint32 `json:"id"`
	PNG  []byte `json:"png,omitempty"` // base64 in JSON
	HotX int    `json:"hotX"`
	HotY int    `json:"hotY"`
	// Hidden means the remote pointer is invisible.
	Hidden bool `json:"hidden,omitempty"`
}
