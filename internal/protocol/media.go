package protocol

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
