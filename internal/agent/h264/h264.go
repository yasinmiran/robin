//go:build linux && cgo && openh264

package h264

/*
#cgo LDFLAGS: -lopenh264 -lstdc++ -lm
#include <stdlib.h>
#include <string.h>
#include <wels/codec_api.h>

typedef struct {
	ISVCEncoder* enc;
	SFrameBSInfo info;
} encoder;

typedef struct {
	int width, height, bitrate, max_bitrate, threads, min_qp, max_qp, usage;
	float fps;
	int scene_change, frame_skip;
} params;

static int enc_copy(encoder* e, unsigned char* dst, int cap);

static int enc_open(encoder* e, params* in) {
	if (WelsCreateSVCEncoder(&e->enc) != 0 || e->enc == NULL) return -1;
	int quiet = WELS_LOG_QUIET;
	(*e->enc)->SetOption(e->enc, ENCODER_OPTION_TRACE_LEVEL, &quiet);

	SEncParamExt p;
	(*e->enc)->GetDefaultParams(e->enc, &p);
	p.iUsageType = in->usage;
	p.iPicWidth = in->width;
	p.iPicHeight = in->height;
	p.iRCMode = RC_BITRATE_MODE;
	p.iTargetBitrate = in->bitrate;
	p.iMaxBitrate = in->max_bitrate;
	p.fMaxFrameRate = in->fps;
	p.iSpatialLayerNum = 1;
	p.iTemporalLayerNum = 1;
	p.uiIntraPeriod = 0;
	p.bEnableFrameSkip = in->frame_skip;
	p.bEnableSceneChangeDetect = in->scene_change;
	p.bEnableBackgroundDetection = true;
	p.bEnableAdaptiveQuant = true;
	p.bEnableDenoise = false;
	p.bEnableLongTermReference = false;
	p.iEntropyCodingModeFlag = 0;
	p.iMultipleThreadIdc = in->threads;
	p.eSpsPpsIdStrategy = CONSTANT_ID;
	p.iMinQp = in->min_qp;
	p.iMaxQp = in->max_qp;

	SSpatialLayerConfig* l = &p.sSpatialLayers[0];
	l->iVideoWidth = in->width;
	l->iVideoHeight = in->height;
	l->fFrameRate = in->fps;
	l->iSpatialBitrate = in->bitrate;
	l->iMaxSpatialBitrate = in->max_bitrate;
	l->uiProfileIdc = PRO_BASELINE;
	l->sSliceArgument.uiSliceMode = in->threads > 1 ? SM_FIXEDSLCNUM_SLICE : SM_SINGLE_SLICE;
	l->sSliceArgument.uiSliceNum = in->threads > 1 ? in->threads : 1;

	return (*e->enc)->InitializeExt(e->enc, &p);
}

static int enc_force_idr(encoder* e) { return (*e->enc)->ForceIntraFrame(e->enc, true); }

static int enc_set_option(encoder* e, int opt, int layer, int bps) {
	SBitrateInfo b = {layer, bps};
	return (*e->enc)->SetOption(e->enc, opt, &b);
}

// enc_set_bitrate retargets rate control. openh264 checks every layer's target
// against that layer's own peak, so the peaks move first when going up and
// last when going down.
static int enc_set_bitrate(encoder* e, int bps) {
	SBitrateInfo cur = {SPATIAL_LAYER_0, 0};
	(*e->enc)->GetOption(e->enc, ENCODER_OPTION_BITRATE, &cur);
	int rc = 0;
	if (bps > cur.iBitrate) {
		if ((rc = enc_set_option(e, ENCODER_OPTION_MAX_BITRATE, SPATIAL_LAYER_ALL, 2 * bps))) return rc;
		if ((rc = enc_set_option(e, ENCODER_OPTION_MAX_BITRATE, SPATIAL_LAYER_0, 2 * bps))) return rc;
		return enc_set_option(e, ENCODER_OPTION_BITRATE, SPATIAL_LAYER_ALL, bps);
	}
	if ((rc = enc_set_option(e, ENCODER_OPTION_BITRATE, SPATIAL_LAYER_ALL, bps))) return rc;
	if ((rc = enc_set_option(e, ENCODER_OPTION_MAX_BITRATE, SPATIAL_LAYER_0, 2 * bps))) return rc;
	return enc_set_option(e, ENCODER_OPTION_MAX_BITRATE, SPATIAL_LAYER_ALL, 2 * bps);
}

// enc_encode encodes one frame and copies its NAL units into dst. It returns
// the byte count, 0 for a skipped frame, -1 on error, or -2 when dst is too
// small (the caller grows it and calls enc_copy).
static int enc_encode(encoder* e, unsigned char* y, unsigned char* u, unsigned char* v,
		int w, int h, long long ts_ms, unsigned char* dst, int cap, int* frame_type) {
	SSourcePicture pic;
	memset(&pic, 0, sizeof pic);
	pic.iColorFormat = videoFormatI420;
	pic.iPicWidth = w;
	pic.iPicHeight = h;
	pic.iStride[0] = w;
	pic.iStride[1] = pic.iStride[2] = w / 2;
	pic.pData[0] = y;
	pic.pData[1] = u;
	pic.pData[2] = v;
	pic.uiTimeStamp = ts_ms;

	memset(&e->info, 0, sizeof e->info);
	if ((*e->enc)->EncodeFrame(e->enc, &pic, &e->info) != cmResultSuccess) return -1;
	*frame_type = e->info.eFrameType;
	if (e->info.eFrameType == videoFrameTypeSkip) return 0;
	return enc_copy(e, dst, cap);
}

static int enc_copy(encoder* e, unsigned char* dst, int cap) {
	int n = 0;
	for (int i = 0; i < e->info.iLayerNum; i++) {
		SLayerBSInfo* layer = &e->info.sLayerInfo[i];
		int size = 0;
		for (int j = 0; j < layer->iNalCount; j++) size += layer->pNalLengthInByte[j];
		if (n + size > cap) return -2;
		memcpy(dst + n, layer->pBsBuf, size);
		n += size;
	}
	return n;
}

static int enc_size(encoder* e) {
	int n = 0;
	for (int i = 0; i < e->info.iLayerNum; i++)
		for (int j = 0; j < e->info.sLayerInfo[i].iNalCount; j++) n += e->info.sLayerInfo[i].pNalLengthInByte[j];
	return n;
}

static void enc_close(encoder* e) {
	(*e->enc)->Uninitialize(e->enc);
	WelsDestroySVCEncoder(e->enc);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// FrameType classifies an encoded frame.
type FrameType int

// Frame types as openh264 reports them.
const (
	FrameSkip FrameType = C.videoFrameTypeSkip
	FrameIDR  FrameType = C.videoFrameTypeIDR
	FrameI    FrameType = C.videoFrameTypeI
	FrameP    FrameType = C.videoFrameTypeP
)

// Params tune the encoder. Defaults returns the values the agent uses.
type Params struct {
	Width, Height, FPS int
	Bitrate            int // target, bits per second
	Threads            int
	MinQP, MaxQP       int
	// Screen selects openh264's screen-content mode, which always detects
	// scene changes; camera mode lets SceneChange be switched off.
	Screen      bool
	SceneChange bool
	FrameSkip   bool
}

// Defaults returns the agent's encoder settings for a frame size. Camera mode
// with scene-change detection off never inserts IDR frames of its own (screen
// mode turns full-screen motion into an IDR per frame) and encodes 3 to 4
// times faster. Frame skipping is what holds the target on incompressible
// content; its skips cluster after a keyframe, while the link is still busy
// carrying it. QP 51 leaves rate control room to back off under congestion.
func Defaults(width, height, fps, bitrate int) Params {
	return Params{
		Width: width, Height: height, FPS: fps, Bitrate: bitrate,
		Threads: min(max(runtime.NumCPU()/2, 1), 4),
		MinQP:   16, MaxQP: 51,
		Screen: false, SceneChange: false, FrameSkip: true,
	}
}

// Encoder is one openh264 instance for a fixed frame size. It is not safe for
// concurrent use.
type Encoder struct {
	e      *C.encoder
	p      Params
	buf    []byte
	last   FrameType
	closed bool
}

// New starts an encoder with Defaults.
func New(width, height, fps, bitrate int) (*Encoder, error) {
	return Open(Defaults(width, height, fps, bitrate))
}

// Open starts an encoder with explicit parameters.
func Open(p Params) (*Encoder, error) {
	if p.Width < 16 || p.Height < 16 || p.Width%2 != 0 || p.Height%2 != 0 {
		return nil, fmt.Errorf("h264: bad frame size %dx%d", p.Width, p.Height)
	}
	usage := C.CAMERA_VIDEO_REAL_TIME
	if p.Screen {
		usage = C.SCREEN_CONTENT_REAL_TIME
	}
	e := (*C.encoder)(C.calloc(1, C.size_t(unsafe.Sizeof(C.encoder{}))))
	in := C.params{
		width: C.int(p.Width), height: C.int(p.Height), bitrate: C.int(p.Bitrate),
		max_bitrate: C.int(p.Bitrate * 2), threads: C.int(max(p.Threads, 1)),
		min_qp: C.int(p.MinQP), max_qp: C.int(p.MaxQP), usage: C.int(usage), fps: C.float(p.FPS),
		scene_change: cbool(p.SceneChange), frame_skip: cbool(p.FrameSkip),
	}
	if rc := C.enc_open(e, &in); rc != 0 {
		C.free(unsafe.Pointer(e))
		return nil, fmt.Errorf("h264: initialize: openh264 error %d", rc)
	}
	return &Encoder{e: e, p: p, buf: make([]byte, p.Width*p.Height)}, nil
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Encode compresses img, taken at ts since the stream began. The returned
// slice is valid until the next call. It is empty when rate control skipped
// the frame, which never happens to a requested keyframe.
func (e *Encoder) Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error) {
	if e.closed {
		return nil, errors.New("h264: encoder closed")
	}
	if img.Width != e.p.Width || img.Height != e.p.Height {
		return nil, fmt.Errorf("h264: frame is %dx%d, encoder is %dx%d", img.Width, img.Height, e.p.Width, e.p.Height)
	}
	if keyframe {
		if rc := C.enc_force_idr(e.e); rc != 0 {
			return nil, fmt.Errorf("h264: force IDR: openh264 error %d", rc)
		}
	}
	var ft C.int
	n := C.enc_encode(e.e,
		(*C.uchar)(unsafe.Pointer(&img.Y[0])), (*C.uchar)(unsafe.Pointer(&img.U[0])), (*C.uchar)(unsafe.Pointer(&img.V[0])),
		C.int(img.Width), C.int(img.Height), C.longlong(ts.Milliseconds()),
		(*C.uchar)(unsafe.Pointer(&e.buf[0])), C.int(len(e.buf)), &ft)
	if n == -2 {
		e.buf = make([]byte, int(C.enc_size(e.e))*2)
		n = C.enc_copy(e.e, (*C.uchar)(unsafe.Pointer(&e.buf[0])), C.int(len(e.buf)))
	}
	runtime.KeepAlive(img)
	if n < 0 {
		return nil, fmt.Errorf("h264: encode: openh264 error %d", n)
	}
	e.last = FrameType(ft)
	return e.buf[:n], nil
}

// LastFrameType reports the type of the most recent Encode result.
func (e *Encoder) LastFrameType() FrameType { return e.last }

// SetBitrate retargets rate control; it applies from the next frame.
func (e *Encoder) SetBitrate(bps int) error {
	if e.closed {
		return errors.New("h264: encoder closed")
	}
	if rc := C.enc_set_bitrate(e.e, C.int(bps)); rc != 0 {
		return fmt.Errorf("h264: set bitrate %d: openh264 error %d", bps, rc)
	}
	e.p.Bitrate = bps
	return nil
}

// Close releases the encoder.
func (e *Encoder) Close() error {
	if !e.closed {
		e.closed = true
		C.enc_close(e.e)
		C.free(unsafe.Pointer(e.e))
	}
	return nil
}
