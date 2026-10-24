package protocol

// The names the host's programs see the browser's devices by, on PulseAudio
// and on PipeWire, and the CLI tells the user to look for. A v4l2loopback
// device keeps the label it was loaded with.
const (
	MicName    = "Robin microphone"
	CameraName = "Robin camera"
)

// Device reports one of the browser's devices the agent offers the host's
// programs: the microphone or the camera.
type Device struct {
	// Available says the host has somewhere to put it; Error says why not.
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	// Where says what carries it on the host, such as PipeWire or a
	// v4l2loopback device, for the user to look for it there.
	Where string `json:"where,omitempty"`
}
