// Package track names what the agent reads a media track the browser sends
// through, so the microphone, the camera and the session take one type.
package track

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// Reader yields the packets of one media track the browser sends.
// *webrtc.TrackRemote is one.
type Reader interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
}
