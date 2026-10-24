//go:build linux && cgo && opus

package opus

/*
#cgo LDFLAGS: -lopus -lm
#include <opus/opus.h>

// opus_encoder_ctl is variadic, which cgo cannot call.

static int enc_setup(OpusEncoder* e, int bitrate, int complexity, int fec, int loss) {
	int rc;
	if ((rc = opus_encoder_ctl(e, OPUS_SET_BITRATE(bitrate))) != OPUS_OK) return rc;
	if ((rc = opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(complexity))) != OPUS_OK) return rc;
	if ((rc = opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(fec))) != OPUS_OK) return rc;
	return opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(loss));
}

static int enc_get(OpusEncoder* e, int request, opus_int32* value) {
	return opus_encoder_ctl(e, request, value);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// maxPacket bounds one packet. At Bitrate a 20 ms packet averages 240 bytes;
// the cap keeps a VBR peak inside one RTP packet on any path MTU.
const maxPacket = 1000

// Encoder is one libopus encoder in OPUS_APPLICATION_AUDIO mode with in-band
// FEC on. It is not safe for concurrent use.
type Encoder struct {
	e   *C.OpusEncoder
	buf []byte
}

// NewEncoder starts an encoder for SampleRate stereo at bitrate bits per
// second. complexity trades CPU for quality, 0 to 10.
func NewEncoder(bitrate, complexity int) (*Encoder, error) {
	return newEncoder(bitrate, complexity, 1, ExpectedLoss)
}

func newEncoder(bitrate, complexity, fec, loss int) (*Encoder, error) {
	var rc C.int
	e := C.opus_encoder_create(SampleRate, Channels, C.OPUS_APPLICATION_AUDIO, &rc)
	if rc != C.OPUS_OK {
		return nil, fmt.Errorf("opus: create encoder: %s", strerror(rc))
	}
	if rc := C.enc_setup(e, C.int(bitrate), C.int(complexity), C.int(fec), C.int(loss)); rc != C.OPUS_OK {
		C.opus_encoder_destroy(e)
		return nil, fmt.Errorf("opus: configure encoder: %s", strerror(rc))
	}
	return &Encoder{e: e, buf: make([]byte, maxPacket)}, nil
}

// Encode compresses one frame: FrameSamples interleaved stereo samples. The
// packet is the caller's to keep.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if e.e == nil {
		return nil, fmt.Errorf("opus: encoder closed")
	}
	if len(pcm) != FrameSamples*Channels {
		return nil, fmt.Errorf("opus: frame has %d samples, want %d", len(pcm), FrameSamples*Channels)
	}
	n := C.opus_encode(e.e, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), FrameSamples,
		(*C.uchar)(unsafe.Pointer(&e.buf[0])), C.opus_int32(len(e.buf)))
	if n < 0 {
		return nil, fmt.Errorf("opus: encode: %s", strerror(C.int(n)))
	}
	return append([]byte(nil), e.buf[:n]...), nil
}

// Settings tests read back; cgo is not available in test files.
const (
	getApplication = C.OPUS_GET_APPLICATION_REQUEST
	getBitrate     = C.OPUS_GET_BITRATE_REQUEST
	getComplexity  = C.OPUS_GET_COMPLEXITY_REQUEST
	getInbandFEC   = C.OPUS_GET_INBAND_FEC_REQUEST
	getLossPerc    = C.OPUS_GET_PACKET_LOSS_PERC_REQUEST
	getDTX         = C.OPUS_GET_DTX_REQUEST
	getSampleRate  = C.OPUS_GET_SAMPLE_RATE_REQUEST

	applicationAudio = C.OPUS_APPLICATION_AUDIO
)

func (e *Encoder) get(request int) int {
	var v C.opus_int32
	C.enc_get(e.e, C.int(request), &v)
	return int(v)
}

// Close releases the encoder. It is safe to call more than once.
func (e *Encoder) Close() error {
	if e.e != nil {
		C.opus_encoder_destroy(e.e)
		e.e = nil
	}
	return nil
}

func strerror(rc C.int) string { return C.GoString(C.opus_strerror(rc)) }
