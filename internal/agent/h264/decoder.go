//go:build linux && cgo && openh264

package h264

/*
#cgo LDFLAGS: -lopenh264 -lstdc++ -lm
#include <stdlib.h>
#include <string.h>
#include <wels/codec_api.h>

typedef struct {
	ISVCDecoder* dec;
	unsigned char* planes[3];
	SBufferInfo info;
} decoder;

// dec_open starts a decoder that reports a damaged picture as an error
// instead of concealing it, so the caller can ask the sender for a
// keyframe.
static int dec_open(decoder* d) {
	if (WelsCreateDecoder(&d->dec) != 0 || d->dec == NULL) return -1;
	int quiet = WELS_LOG_QUIET;
	(*d->dec)->SetOption(d->dec, DECODER_OPTION_TRACE_LEVEL, &quiet);
	SDecodingParam p;
	memset(&p, 0, sizeof p);
	p.sVideoProperty.eVideoBsType = VIDEO_BITSTREAM_AVC;
	p.eEcActiveIdc = ERROR_CON_DISABLE;
	return (*d->dec)->Initialize(d->dec, &p);
}

static int dec_decode(decoder* d, const unsigned char* src, int n) {
	memset(&d->info, 0, sizeof d->info);
	d->planes[0] = d->planes[1] = d->planes[2] = NULL;
	return (*d->dec)->DecodeFrameNoDelay(d->dec, src, n, d->planes, &d->info);
}

static void dec_close(decoder* d) {
	(*d->dec)->Uninitialize(d->dec);
	WelsDestroyDecoder(d->dec);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Decoder is one openh264 decoder. It is not safe for concurrent use.
type Decoder struct {
	d *C.decoder
}

// NewDecoder starts a decoder.
func NewDecoder() (*Decoder, error) {
	d := (*C.decoder)(C.calloc(1, C.size_t(unsafe.Sizeof(C.decoder{}))))
	if rc := C.dec_open(d); rc != 0 {
		C.free(unsafe.Pointer(d))
		return nil, fmt.Errorf("h264: start decoder: openh264 error %d", rc)
	}
	return &Decoder{d: d}, nil
}

// Decode takes one access unit in Annex-B form and returns the picture it
// completes, or nil when it completes none. The picture's planes belong to
// the decoder and hold until the next call. An error means the unit was
// damaged or refers to a picture the decoder lost; nothing decodes right
// until the next IDR.
func (d *Decoder) Decode(au []byte) (*Picture, error) {
	if d.d == nil {
		return nil, errors.New("h264: decoder closed")
	}
	if len(au) == 0 {
		return nil, nil
	}
	state := C.dec_decode(d.d, (*C.uchar)(unsafe.Pointer(&au[0])), C.int(len(au)))
	if state != C.dsErrorFree {
		return nil, fmt.Errorf("h264: decode: openh264 state %#x", int(state))
	}
	if d.d.info.iBufferStatus != 1 {
		return nil, nil
	}
	buf := (*C.SSysMEMBuffer)(unsafe.Pointer(&d.d.info.UsrData))
	w, h := int(buf.iWidth), int(buf.iHeight)
	ys, cs := int(buf.iStride[0]), int(buf.iStride[1])
	return &Picture{
		Width: w, Height: h, YStride: ys, CStride: cs,
		Y: unsafe.Slice((*byte)(unsafe.Pointer(d.d.planes[0])), ys*h),
		U: unsafe.Slice((*byte)(unsafe.Pointer(d.d.planes[1])), cs*((h+1)/2)),
		V: unsafe.Slice((*byte)(unsafe.Pointer(d.d.planes[2])), cs*((h+1)/2)),
	}, nil
}

// Close releases the decoder. It is safe to call more than once.
func (d *Decoder) Close() error {
	if d.d != nil {
		C.dec_close(d.d)
		C.free(unsafe.Pointer(d.d))
		d.d = nil
	}
	return nil
}
