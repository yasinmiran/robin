//go:build linux && cgo && opus

package opus

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// tone fills frames of interleaved stereo with a different sine per channel,
// so a swapped or collapsed channel shows up.
func tone(seconds float64, left, right float64) []int16 {
	n := int(seconds*SampleRate) / FrameSamples * FrameSamples
	pcm := make([]int16, n*Channels)
	for i := range n {
		t := float64(i) / SampleRate
		pcm[2*i] = int16(0.3 * 32767 * math.Sin(2*math.Pi*left*t))
		pcm[2*i+1] = int16(0.3 * 32767 * math.Sin(2*math.Pi*right*t))
	}
	return pcm
}

func noise(seconds float64) []int16 {
	r := rand.New(rand.NewPCG(1, 2))
	pcm := make([]int16, int(seconds*SampleRate)/FrameSamples*FrameSamples*Channels)
	for i := range pcm {
		pcm[i] = int16(r.IntN(1<<15) - 1<<14)
	}
	return pcm
}

func openEncoder(t testing.TB, complexity int) *Encoder {
	t.Helper()
	enc, err := NewEncoder(Bitrate, complexity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enc.Close() })
	return enc
}

// roundTrip encodes pcm frame by frame and decodes every packet.
func roundTrip(t *testing.T, enc *Encoder, pcm []int16) (out []int16, bytes int) {
	t.Helper()
	dec, err := NewDecoder(Channels)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	frame := make([]int16, FrameSamples*Channels)
	for off := 0; off < len(pcm); off += len(frame) {
		pkt, err := enc.Encode(pcm[off : off+len(frame)])
		if err != nil {
			t.Fatal(err)
		}
		bytes += len(pkt)
		n, err := dec.Decode(pkt, frame)
		if err != nil {
			t.Fatal(err)
		}
		if n != FrameSamples {
			t.Fatalf("decoded %d samples per channel, want %d", n, FrameSamples)
		}
		out = append(out, frame...)
	}
	return out, bytes
}

// measure returns the RMS and the zero-crossing frequency of one channel,
// skipping the first tenth of a second while the codec settles.
func measure(pcm []int16, ch int) (rms, hz float64) {
	skip := SampleRate / 10
	var sum float64
	crossings := 0
	n := 0
	for i := skip; i < len(pcm)/Channels; i++ {
		v := float64(pcm[i*Channels+ch])
		sum += v * v
		n++
		if prev := pcm[(i-1)*Channels+ch]; (prev < 0) != (v < 0) {
			crossings++
		}
	}
	return math.Sqrt(sum / float64(n)), float64(crossings) / 2 / (float64(n) / SampleRate)
}

func TestRoundTripKeepsEachChannel(t *testing.T) {
	in := tone(2, 440, 1000)
	out, bytes := roundTrip(t, openEncoder(t, Complexity), in)
	for ch, want := range []float64{440, 1000} {
		rmsIn, _ := measure(in, ch)
		rmsOut, hz := measure(out, ch)
		if r := rmsOut / rmsIn; r < 0.8 || r > 1.2 {
			t.Errorf("channel %d: RMS %.0f after the round trip, %.0f before", ch, rmsOut, rmsIn)
		}
		if math.Abs(hz-want) > want*0.02 {
			t.Errorf("channel %d: %.1f Hz after the round trip, want %.0f", ch, hz, want)
		}
	}
	t.Logf("tone: %.1f kbit/s", kbps(bytes, len(in)))
}

func kbps(bytes, samples int) float64 {
	return float64(bytes*8) / (float64(samples/Channels) / SampleRate) / 1000
}

// TestBitrate holds noise near the target and lets digital silence, which
// is most of a desktop's time, cost almost nothing.
func TestBitrate(t *testing.T) {
	_, b := roundTrip(t, openEncoder(t, Complexity), noise(3))
	if k := kbps(b, 3*SampleRate*Channels); k < Bitrate/1000*0.7 || k > Bitrate/1000*1.3 {
		t.Errorf("noise at %.1f kbit/s, want about %d", k, Bitrate/1000)
	} else {
		t.Logf("noise: %.1f kbit/s", k)
	}
	silence := make([]int16, 3*SampleRate*Channels)
	_, b = roundTrip(t, openEncoder(t, Complexity), silence)
	if k := kbps(b, len(silence)); k > 2 {
		t.Errorf("silence at %.1f kbit/s, want under 2", k)
	} else {
		t.Logf("silence: %.1f kbit/s", k)
	}
}

func TestSettings(t *testing.T) {
	enc := openEncoder(t, 7)
	for _, s := range []struct {
		name    string
		request int
		want    int
	}{
		{"application", getApplication, applicationAudio},
		{"bitrate", getBitrate, Bitrate},
		{"complexity", getComplexity, 7},
		{"in-band FEC", getInbandFEC, 1},
		{"expected loss", getLossPerc, ExpectedLoss},
		{"DTX", getDTX, 0},
		{"sample rate", getSampleRate, SampleRate},
	} {
		if got := enc.get(s.request); got != s.want {
			t.Errorf("%s = %d, want %d", s.name, got, s.want)
		}
	}
}

func TestRejectsAPartialFrame(t *testing.T) {
	enc := openEncoder(t, Complexity)
	if _, err := enc.Encode(make([]int16, FrameSamples)); err == nil {
		t.Error("a mono-sized frame was accepted")
	}
	enc.Close()
	if _, err := enc.Encode(make([]int16, FrameSamples*Channels)); err == nil {
		t.Error("a closed encoder encoded")
	}
}

// TestCost reports encoder CPU as a share of one core in real time, which is
// what choosing a complexity trades against.
func TestCost(t *testing.T) {
	for _, in := range []struct {
		name string
		pcm  []int16
	}{{"tone", tone(5, 440, 1000)}, {"noise", noise(5)}} {
		for _, c := range []int{5, 8, 10} {
			enc := openEncoder(t, c)
			start := time.Now()
			for off := 0; off < len(in.pcm); off += FrameSamples * Channels {
				if _, err := enc.Encode(in.pcm[off : off+FrameSamples*Channels]); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%s at complexity %d: %.2f%% of a core", in.name, c, 100*time.Since(start).Seconds()/5)
		}
	}
}
