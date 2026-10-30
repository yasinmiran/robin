package peer

import (
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// h264Clock is the RTP clock rate of H.264, in ticks a second.
const h264Clock = 90000

// VideoTrack is the H.264 track together with the RTP clock its packetizer
// keeps, so the timestamp of the next frame is known before it is encoded:
// the page's tile manifests are keyed by it. One track may be bound to any
// number of peers: a frame written once is packetized once, on one clock,
// and sent to each of them under its own SSRC.
//
// Pion's packetizer stamps a sample with its clock and then advances the
// clock by the sample's duration, carrying the fraction of a tick over to
// the next. It does so only once the track is first bound; before that,
// written samples go nowhere and the clock stands still. VideoTrack does
// the same sums under one lock with Bind, so the two never disagree.
type VideoTrack struct {
	*webrtc.TrackLocalStaticSample

	mu        sync.Mutex
	bound     bool
	next      uint32
	remainder float64
}

// NewVideoTrack makes a track for Config.Video.
func NewVideoTrack() (*VideoTrack, error) {
	base := rand.Uint32()
	t, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: h264Clock, SDPFmtpLine: H264Fmtp},
		"video", "robin", webrtc.WithRTPTimestamp(base),
	)
	if err != nil {
		return nil, err
	}
	return &VideoTrack{TrackLocalStaticSample: t, next: base}, nil
}

// Bind binds the underlying track and starts the frame clock under the lock
// write takes, so NextFrameRTP never sees one without the other.
func (t *VideoTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	codec, err := t.TrackLocalStaticSample.Bind(ctx)
	if err == nil {
		t.bound = true
	}
	return codec, err
}

// Write sends one encoded access unit in Annex-B form to every peer the
// track is bound to.
func (t *VideoTrack) Write(annexB []byte, d time.Duration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	err := t.TrackLocalStaticSample.WriteSample(media.Sample{Data: annexB, Duration: d})
	if t.bound {
		total := d.Seconds()*h264Clock + t.remainder
		ticks := uint32(total)
		t.remainder = total - float64(ticks)
		t.next += ticks
	}
	return err
}

// NextRTP returns the timestamp the next Write will carry, and whether the
// track was ever bound: before that no frame written reaches anyone.
func (t *VideoTrack) NextRTP() (uint32, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.next, t.bound
}

// NextFrameRTP returns the RTP timestamp the next WriteFrame will carry. It
// reports false while the browser has not been answered yet, when no frame
// written reaches it.
func (p *Peer) NextFrameRTP() (uint32, bool) { return p.track.NextRTP() }
