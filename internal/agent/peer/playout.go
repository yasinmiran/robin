package peer

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// playoutDelayURI names the RTP header extension that bounds how long the
// receiver may hold a frame before showing it.
const playoutDelayURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

// playoutDelayFactory builds the interceptor that asks for no playout delay
// on every video packet. A still screen sends only the keyframes the
// browser asks for every few seconds, and Chrome's jitter estimate grows on
// those until it holds the first frames after a pause for about a second.
// A desktop is better served showing each frame once it is complete.
type playoutDelayFactory struct{}

func (playoutDelayFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &playoutDelay{}, nil
}

type playoutDelay struct {
	interceptor.NoOp
}

// zeroPlayoutDelay is the extension payload for a minimum and a maximum of
// zero: two 12-bit values in 10 ms units.
var zeroPlayoutDelay = []byte{0, 0, 0}

func (*playoutDelay) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var id uint8
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == playoutDelayURI {
			id = uint8(e.ID)
		}
	}
	if id == 0 {
		return writer
	}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if err := h.SetExtension(id, zeroPlayoutDelay); err != nil {
			return 0, err
		}
		return writer.Write(h, payload, a)
	})
}
