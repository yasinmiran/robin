//go:build linux && cgo && opus

package opus

/*
#cgo LDFLAGS: -lopus -lm
#include <opus/opus.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Decoder is a libopus decoder at SampleRate. It is not safe for concurrent
// use.
type Decoder struct {
	d        *C.OpusDecoder
	channels int
}

// NewDecoder starts a decoder that writes channels interleaved channels,
// mixing a packet of the other count up or down.
func NewDecoder(channels int) (*Decoder, error) {
	var rc C.int
	d := C.opus_decoder_create(SampleRate, C.int(channels), &rc)
	if rc != C.OPUS_OK {
		return nil, fmt.Errorf("opus: create decoder: %s", strerror(rc))
	}
	return &Decoder{d: d, channels: channels}, nil
}

// Decode decodes one packet into pcm and returns the samples written per
// channel. pcm must hold the whole packet; MaxFrameSamples per channel
// always does.
func (d *Decoder) Decode(pkt []byte, pcm []int16) (int, error) {
	if len(pkt) == 0 {
		return 0, errors.New("opus: empty packet")
	}
	return d.decode(pkt, pcm, len(pcm)/d.channels, 0)
}

// Recover decodes the samples per channel lost just before pkt from the copy
// pkt carries in its in-band FEC. Where pkt carries none, libopus conceals
// the loss instead.
func (d *Decoder) Recover(pkt []byte, pcm []int16, samples int) (int, error) {
	if len(pkt) == 0 {
		return 0, errors.New("opus: empty packet")
	}
	return d.decode(pkt, pcm, samples, 1)
}

// Conceal fills samples per channel of pcm with libopus's packet loss
// concealment, continuing what it decoded last. samples must be a multiple
// of 2.5 ms.
func (d *Decoder) Conceal(pcm []int16, samples int) (int, error) {
	return d.decode(nil, pcm, samples, 0)
}

func (d *Decoder) decode(pkt []byte, pcm []int16, samples, fec int) (int, error) {
	if d.d == nil {
		return 0, errors.New("opus: decoder closed")
	}
	if samples <= 0 || samples*d.channels > len(pcm) {
		return 0, fmt.Errorf("opus: %d samples per channel do not fit a buffer of %d", samples, len(pcm))
	}
	var data *C.uchar
	if len(pkt) > 0 {
		data = (*C.uchar)(unsafe.Pointer(&pkt[0]))
	}
	n := C.opus_decode(d.d, data, C.opus_int32(len(pkt)), (*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(samples), C.int(fec))
	if n < 0 {
		return 0, fmt.Errorf("opus: decode: %s", strerror(n))
	}
	return int(n), nil
}

// Close releases the decoder. It is safe to call more than once.
func (d *Decoder) Close() error {
	if d.d != nil {
		C.opus_decoder_destroy(d.d)
		d.d = nil
	}
	return nil
}

// PacketSamples is how many samples per channel pkt decodes to.
func PacketSamples(pkt []byte) (int, error) {
	if len(pkt) == 0 {
		return 0, errors.New("opus: empty packet")
	}
	n := C.opus_packet_get_nb_samples((*C.uchar)(unsafe.Pointer(&pkt[0])), C.opus_int32(len(pkt)), SampleRate)
	if n < 0 {
		return 0, fmt.Errorf("opus: bad packet: %s", strerror(n))
	}
	return int(n), nil
}
