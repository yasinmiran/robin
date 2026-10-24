//go:build linux && cgo && openh264

package vqtest

/*
#cgo LDFLAGS: -lopenh264 -lstdc++ -lm
#include <stdlib.h>
#include <string.h>
#include <wels/codec_api.h>

typedef struct {
	ISVCDecoder* dec;
	SBufferInfo info;
	unsigned char* planes[3];
} decoder;

static int dec_open(decoder* d) {
	if (WelsCreateDecoder(&d->dec) != 0 || d->dec == NULL) return -1;
	int quiet = WELS_LOG_QUIET;
	(*d->dec)->SetOption(d->dec, DECODER_OPTION_TRACE_LEVEL, &quiet);
	SDecodingParam p;
	memset(&p, 0, sizeof p);
	p.sVideoProperty.eVideoBsType = VIDEO_BITSTREAM_AVC;
	p.uiTargetDqLayer = 0xff;
	p.eEcActiveIdc = ERROR_CON_DISABLE;
	return (int)(*d->dec)->Initialize(d->dec, &p);
}

// dec_decode decodes one access unit. It returns openh264's decoding state
// and sets *ready when a picture came out.
static int dec_decode(decoder* d, unsigned char* au, int n, int* ready) {
	memset(&d->info, 0, sizeof d->info);
	d->planes[0] = d->planes[1] = d->planes[2] = NULL;
	int st = (*d->dec)->DecodeFrameNoDelay(d->dec, au, n, d->planes, &d->info);
	*ready = d->info.iBufferStatus == 1;
	return st;
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

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// Decoder is an openh264 decoder. It is not safe for concurrent use.
type Decoder struct {
	d *C.decoder
}

// NewDecoder starts a decoder with error concealment off, so a picture is
// either decoded whole or reported as an error.
func NewDecoder() (*Decoder, error) {
	d := (*C.decoder)(C.calloc(1, C.size_t(unsafe.Sizeof(C.decoder{}))))
	if rc := C.dec_open(d); rc != 0 {
		C.free(unsafe.Pointer(d))
		return nil, fmt.Errorf("vqtest: openh264 decoder: error %d", rc)
	}
	return &Decoder{d: d}, nil
}

// Decode decodes one Annex-B access unit into a new picture, or returns nil
// when it produced none.
func (d *Decoder) Decode(au []byte) (*yuv.I420, error) {
	if d.d == nil {
		return nil, errors.New("vqtest: decoder closed")
	}
	if len(au) == 0 {
		return nil, errors.New("vqtest: empty access unit")
	}
	buf := C.CBytes(au)
	defer C.free(buf)
	var ready C.int
	if st := C.dec_decode(d.d, (*C.uchar)(buf), C.int(len(au)), &ready); st != 0 {
		return nil, fmt.Errorf("vqtest: decode: openh264 state %#x", int(st))
	}
	if ready == 0 {
		return nil, nil
	}
	sb := (*C.SSysMEMBuffer)(unsafe.Pointer(&d.d.info.UsrData))
	w, h := int(sb.iWidth), int(sb.iHeight)
	ys, cs := int(sb.iStride[0]), int(sb.iStride[1])
	img := yuv.NewI420(w, h)
	copyPlane(img.Y, d.d.planes[0], w, h, ys)
	copyPlane(img.U, d.d.planes[1], w/2, h/2, cs)
	copyPlane(img.V, d.d.planes[2], w/2, h/2, cs)
	return img, nil
}

func copyPlane(dst []byte, src *C.uchar, w, h, stride int) {
	plane := unsafe.Slice((*byte)(unsafe.Pointer(src)), (h-1)*stride+w)
	for y := range h {
		copy(dst[y*w:y*w+w], plane[y*stride:y*stride+w])
	}
}

// Close releases the decoder.
func (d *Decoder) Close() error {
	if d.d != nil {
		C.dec_close(d.d)
		C.free(unsafe.Pointer(d.d))
		d.d = nil
	}
	return nil
}
