package peer

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

// estimator is Pion's GCC, kept from learning while the agent sends too
// little to test the link. On a still screen the agent sends a refresh
// every 500 ms and sound, a few hundred kbit/s at most, and GCC read
// those as the link's measure: with no overuse its target rose 8% a
// second to the ceiling, which the first motion then overflowed, and on
// overuse it fell to 0.85 times a rate the browser received from a sender
// with nothing to send. libwebrtc tells such spells apart (AlrDetector);
// Pion does not, so the feedback of a spell never reaches it, and its
// target stays where the spell found it. Motion then starts at that
// target, and its own frames probe the link: their feedback reaches GCC,
// which answers overuse at once, now from what a full sender delivered.
type estimator struct {
	inner cc.BandwidthEstimator
	pacer gcc.Pacer
	now   func() time.Time
	log   *slog.Logger
	// feed and soundQueued put back what Pion does, for comparison.
	feed, soundQueued bool

	mu      sync.Mutex
	alr     alrDetector
	limited bool
	target  int // GCC's target, bits per second
}

// newEstimator builds the congestion controller over pacer, which it
// owns.
func newEstimator(cfg Config, pacer gcc.Pacer, log *slog.Logger, now func() time.Time) (*estimator, error) {
	e := &estimator{
		pacer: pacer, now: now, log: log, target: cfg.StartBitrate,
		feed: cfg.FeedWhileLimited, soundQueued: cfg.SoundBehindVideo,
	}
	e.alr.setTarget(cfg.StartBitrate)
	inner, err := gcc.NewSendSideBWE(
		gcc.SendSideBWEPacer(meteredPacer{e}),
		gcc.SendSideBWEInitialBitrate(cfg.StartBitrate),
		gcc.SendSideBWEMinBitrate(cfg.MinBitrate),
		gcc.SendSideBWEMaxBitrate(cfg.MaxBitrate),
	)
	if err != nil {
		pacer.Close()
		return nil, err
	}
	e.inner = inner
	return e, nil
}

// AddStream sends sound around the pacer's queue: an Opus packet is a few
// hundred bytes that must not wait behind a keyframe.
func (e *estimator) AddStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	if p, ok := e.pacer.(*pacer); ok {
		for _, ext := range info.RTPHeaderExtensions {
			if ext.URI == sdp.TransportCCURI {
				p.SetTransportCC(info.SSRC, uint8(ext.ID))
			}
		}
		if !e.soundQueued && strings.HasPrefix(strings.ToLower(info.MimeType), "audio/") {
			p.Prioritize(info.SSRC)
		}
	}
	return e.inner.AddStream(info, w)
}

// WriteRTCP hands GCC the browser's feedback unless the sender is limited.
func (e *estimator) WriteRTCP(pkts []rtcp.Packet, attrs interceptor.Attributes) error {
	e.mu.Lock()
	limited := e.spell(e.alr.at(e.now()))
	e.mu.Unlock()
	if limited && !e.feed {
		return nil
	}
	return e.inner.WriteRTCP(pkts, attrs)
}

func (e *estimator) GetTargetBitrate() int { return e.inner.GetTargetBitrate() }

func (e *estimator) OnTargetBitrateChange(f func(int)) { e.inner.OnTargetBitrateChange(f) }

// GetStats is GCC's, with whether the sender is application limited.
func (e *estimator) GetStats() map[string]any {
	stats := e.inner.GetStats()
	e.mu.Lock()
	stats["applicationLimited"] = e.limited
	e.mu.Unlock()
	return stats
}

// Close stops GCC, which closes the pacer.
func (e *estimator) Close() error { return e.inner.Close() }

// spell notes whether the sender is limited, with e.mu held.
func (e *estimator) spell(limited bool) bool {
	if limited != e.limited {
		e.limited = limited
		e.log.Debug("congestion control", "applicationLimited", limited, "target", e.target)
	}
	return limited
}

// sent counts n bytes handed to the pacer.
func (e *estimator) sent(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spell(e.alr.sent(e.now(), n))
}

func (e *estimator) setTarget(bps int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.target = bps
	e.alr.setTarget(bps)
}

// meteredPacer is the pacer as GCC sees it: the estimator hears what goes
// in and what the target is on the way.
type meteredPacer struct{ e *estimator }

func (m meteredPacer) Write(h *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	m.e.sent(h.MarshalSize() + len(payload))
	return m.e.pacer.Write(h, payload, attrs)
}

func (m meteredPacer) AddStream(ssrc uint32, w interceptor.RTPWriter) { m.e.pacer.AddStream(ssrc, w) }

func (m meteredPacer) SetTargetBitrate(bps int) {
	m.e.setTarget(bps)
	m.e.pacer.SetTargetBitrate(bps)
}

func (m meteredPacer) Close() error { return m.e.pacer.Close() }
