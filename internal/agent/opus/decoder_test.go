//go:build linux && cgo && opus

package opus

import (
	"math"
	"testing"
)

// voice is seconds of a voiced, vowel-like sound in both channels: harmonics
// of 140 Hz under two formants, swelling four times a second. Unlike a pure
// tone, the encoder codes it in a mode that carries in-band FEC.
func voice(seconds float64) []int16 {
	n := int(seconds*SampleRate) / FrameSamples * FrameSamples
	pcm := make([]int16, n*Channels)
	for i := range n {
		t := float64(i) / SampleRate
		var v float64
		for h := 1; h <= 20; h++ {
			f := 140 * float64(h)
			gain := math.Exp(-math.Pow((f-700)/300, 2)) + 0.6*math.Exp(-math.Pow((f-1200)/400, 2))
			v += gain * math.Sin(2*math.Pi*f*t+float64(h))
		}
		v *= 0.55 + 0.45*math.Sin(2*math.Pi*4*t)
		s := int16(4000 * v)
		pcm[2*i], pcm[2*i+1] = s, s
	}
	return pcm
}

func encodeAll(t *testing.T, enc *Encoder, pcm []int16) [][]byte {
	t.Helper()
	var pkts [][]byte
	for off := 0; off < len(pcm); off += FrameSamples * Channels {
		pkt, err := enc.Encode(pcm[off : off+FrameSamples*Channels])
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, pkt)
	}
	return pkts
}

func openDecoder(t *testing.T, channels int) *Decoder {
	t.Helper()
	d, err := NewDecoder(channels)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestDecodeMonoFromStereo: the microphone path decodes to one channel
// whatever the browser sent, here a stereo stream with one tone per side.
func TestDecodeMonoFromStereo(t *testing.T) {
	pkts := encodeAll(t, openEncoder(t, Complexity), tone(1, 440, 440))
	d := openDecoder(t, 1)
	pcm := make([]int16, MaxFrameSamples)
	var out []int16
	for _, p := range pkts {
		n, err := d.Decode(p, pcm)
		if err != nil {
			t.Fatal(err)
		}
		if n != FrameSamples {
			t.Fatalf("decoded %d samples, want %d", n, FrameSamples)
		}
		out = append(out, pcm[:n]...)
	}
	stereo := make([]int16, 2*len(out))
	for i, v := range out {
		stereo[2*i], stereo[2*i+1] = v, v
	}
	if rms, hz := measure(stereo, 0); rms < 5000 || math.Abs(hz-440) > 10 {
		t.Errorf("mono decode: RMS %.0f at %.1f Hz, want a 440 Hz tone", rms, hz)
	}
}

// squaredError is the energy of the difference between two mono frames.
func squaredError(a, b []int16) float64 {
	var e float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		e += d * d
	}
	return e
}

// TestRecoverBeatsConcealment drops one packet of voice and fills its gap
// two ways: from the next packet's in-band FEC, and by concealment alone.
// Measured against a decode that lost nothing, the recovered frame has to
// be the closer of the two, or the FEC was never used.
func TestRecoverBeatsConcealment(t *testing.T) {
	enc, err := newEncoder(24_000, Complexity, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pkts := encodeAll(t, enc, voice(2))
	var clean [][]int16
	ref := openDecoder(t, 1)
	for _, p := range pkts {
		pcm := make([]int16, MaxFrameSamples)
		n, err := ref.Decode(p, pcm)
		if err != nil {
			t.Fatal(err)
		}
		clean = append(clean, pcm[:n])
	}

	var recovered, concealed float64
	for lost := 40; lost < len(pkts)-1; lost += 7 {
		for _, fec := range []bool{true, false} {
			d := openDecoder(t, 1)
			pcm := make([]int16, MaxFrameSamples)
			for _, p := range pkts[:lost] {
				if _, err := d.Decode(p, pcm); err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if fec {
				n, err = d.Recover(pkts[lost+1], pcm, FrameSamples)
			} else {
				n, err = d.Conceal(pcm, FrameSamples)
			}
			if err != nil || n != FrameSamples {
				t.Fatalf("filling the gap: %d samples, %v", n, err)
			}
			e := squaredError(pcm[:n], clean[lost])
			if fec {
				recovered += e
			} else {
				concealed += e
			}
		}
	}
	t.Logf("error energy over the lost frames: %.3g recovered, %.3g concealed", recovered, concealed)
	if recovered >= concealed/4 {
		t.Errorf("FEC recovered no better than concealment (%.3g against %.3g)", recovered, concealed)
	}
}

func TestPacketSamples(t *testing.T) {
	pkts := encodeAll(t, openEncoder(t, Complexity), tone(0.1, 440, 440))
	if n, err := PacketSamples(pkts[0]); err != nil || n != FrameSamples {
		t.Errorf("PacketSamples = %d, %v; want %d", n, err, FrameSamples)
	}
	if _, err := PacketSamples(nil); err == nil {
		t.Error("an empty packet has a length")
	}
	if _, err := PacketSamples([]byte{0xff}); err == nil {
		t.Error("a truncated code 3 packet has a length")
	}
}

func TestDecoderRefusesWhatItCannotDo(t *testing.T) {
	pkts := encodeAll(t, openEncoder(t, Complexity), tone(0.1, 440, 440))
	d := openDecoder(t, 1)
	if _, err := d.Decode(nil, make([]int16, MaxFrameSamples)); err == nil {
		t.Error("decoded an empty packet")
	}
	if _, err := d.Recover(nil, make([]int16, MaxFrameSamples), FrameSamples); err == nil {
		t.Error("recovered from an empty packet")
	}
	if _, err := d.Decode(pkts[0], make([]int16, 10)); err == nil {
		t.Error("decoded 20 ms into a buffer of 10 samples")
	}
	if _, err := d.Conceal(make([]int16, FrameSamples), 2*FrameSamples); err == nil {
		t.Error("concealed past the end of the buffer")
	}
	if _, err := d.Decode([]byte{0xff}, make([]int16, MaxFrameSamples)); err == nil {
		t.Error("decoded a corrupt packet")
	}
	d.Close()
	if _, err := d.Decode(pkts[0], make([]int16, MaxFrameSamples)); err == nil {
		t.Error("a closed decoder decoded")
	}
	d.Close()
	if _, err := NewDecoder(3); err == nil {
		t.Error("a three-channel decoder started")
	}
}
