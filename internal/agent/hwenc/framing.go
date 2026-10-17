package hwenc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// An access unit's end is not marked in a raw Annex-B stream: it shows only
// when the next one begins, a frame later. Encode waits for each frame's
// access unit, so the tools wrap theirs in a container that states its
// length: GStreamer in multipart parts, ffmpeg in FLV tags.

// splitter reads one access unit at a time, in Annex-B form, from an encoder
// tool's stdout. Every IDR it returns carries the SPS and PPS.
type splitter interface {
	next() ([]byte, error)
}

// framing makes a splitter for a tool's stdout.
type framing func(io.Reader) splitter

// maxAccessUnit bounds what a corrupt length field can make the reader
// allocate. A 4K IDR at the highest bitrate stays well below it.
const maxAccessUnit = 32 << 20

// multipartReader reads GStreamer's multipartmux output, one part per
// buffer:
//
//	--boundary\r\nContent-Type: video/x-h264\r\nContent-Length: 1735\r\n\r\n<data>\r\n
type multipartReader struct {
	r    *bufio.Reader
	sets paramSets
}

func newMultipartReader(r io.Reader) splitter {
	return &multipartReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// maxPartHeader bounds the header block, so a tool that prints something
// else on stdout fails the read instead of filling memory.
const maxPartHeader = 4 << 10

func (m *multipartReader) next() ([]byte, error) {
	size, header := -1, 0
	for {
		line, err := m.r.ReadSlice('\n')
		header += len(line)
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				err = errors.New("multipart: header line too long")
			}
			return nil, err
		}
		if header > maxPartHeader {
			return nil, errors.New("multipart: header block too long")
		}
		s := strings.TrimRight(string(line), "\r\n")
		if s == "" {
			// The blank line after the previous part's data, or the end of
			// this part's headers.
			if size >= 0 {
				break
			}
			continue
		}
		k, v, ok := strings.Cut(s, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			if size, err = strconv.Atoi(strings.TrimSpace(v)); err != nil || size < 0 || size > maxAccessUnit {
				return nil, fmt.Errorf("multipart: bad Content-Length %q", v)
			}
		}
	}
	au := make([]byte, size)
	if _, err := io.ReadFull(m.r, au); err != nil {
		return nil, fmt.Errorf("multipart: %w", err)
	}
	return m.sets.complete(au), nil
}

// flvReader reads ffmpeg's FLV output: a 13-byte file header, then tags. An
// H.264 stream starts with a sequence header tag carrying the SPS and PPS
// (an AVCDecoderConfigurationRecord); every later video tag is one access
// unit of length-prefixed NAL units.
type flvReader struct {
	r       *bufio.Reader
	started bool
	sets    paramSets
	lenSize int
}

func newFLVReader(r io.Reader) splitter {
	return &flvReader{r: bufio.NewReaderSize(r, 64<<10)}
}

const (
	flvTagVideo     = 9
	flvCodecAVC     = 7
	flvAVCSequence  = 0
	flvAVCNALU      = 1
	flvTagHeaderLen = 11
)

func (f *flvReader) next() ([]byte, error) {
	if !f.started {
		var h [13]byte
		if _, err := io.ReadFull(f.r, h[:]); err != nil {
			return nil, fmt.Errorf("flv: header: %w", err)
		}
		if !bytes.Equal(h[:3], []byte("FLV")) {
			return nil, fmt.Errorf("flv: not an FLV stream (% x)", h[:4])
		}
		// The header's own length, then the first PreviousTagSize.
		if skip := int(binary.BigEndian.Uint32(h[5:9])) - 9; skip > 0 {
			if _, err := f.r.Discard(skip); err != nil {
				return nil, fmt.Errorf("flv: header: %w", err)
			}
		}
		f.started = true
	}
	for {
		var th [flvTagHeaderLen]byte
		if _, err := io.ReadFull(f.r, th[:]); err != nil {
			return nil, fmt.Errorf("flv: %w", err)
		}
		size := int(th[1])<<16 | int(th[2])<<8 | int(th[3])
		if size > maxAccessUnit {
			return nil, fmt.Errorf("flv: tag of %d bytes", size)
		}
		// The tag's data, then its PreviousTagSize.
		data := make([]byte, size+4)
		if _, err := io.ReadFull(f.r, data); err != nil {
			return nil, fmt.Errorf("flv: %w", err)
		}
		data = data[:size]
		if th[0]&0x1f != flvTagVideo || len(data) < 5 || data[0]&0x0f != flvCodecAVC {
			continue // script data such as onMetaData
		}
		switch data[1] {
		case flvAVCSequence:
			if err := f.configure(data[5:]); err != nil {
				return nil, err
			}
		case flvAVCNALU:
			au, err := f.annexB(data[5:])
			if err != nil {
				return nil, err
			}
			return f.sets.complete(au), nil
		}
	}
}

var errSequenceHeader = errors.New("flv: malformed AVC sequence header")

// configure reads an AVCDecoderConfigurationRecord: five bytes of profile,
// level and NAL length size, then the SPS list and the PPS list, each entry
// prefixed with a 16-bit length.
func (f *flvReader) configure(rec []byte) error {
	if len(rec) < 6 {
		return errSequenceHeader
	}
	f.lenSize = int(rec[4]&3) + 1
	p := 5
	var nals [][]byte
	readList := func(count int) bool {
		for range count {
			if p+2 > len(rec) {
				return false
			}
			n := int(binary.BigEndian.Uint16(rec[p:]))
			if p+2+n > len(rec) {
				return false
			}
			nals = append(nals, rec[p+2:p+2+n])
			p += 2 + n
		}
		return true
	}
	sps := int(rec[p] & 0x1f)
	p++
	if !readList(sps) || p >= len(rec) {
		return errSequenceHeader
	}
	pps := int(rec[p])
	p++
	if !readList(pps) {
		return errSequenceHeader
	}
	f.sets.note(nals)
	return nil
}

// annexB rewrites length-prefixed NAL units with start codes.
func (f *flvReader) annexB(body []byte) ([]byte, error) {
	if f.lenSize == 0 {
		return nil, errors.New("flv: video before the AVC sequence header")
	}
	out := make([]byte, 0, len(body)+16)
	for len(body) > 0 {
		if len(body) < f.lenSize {
			return nil, errors.New("flv: truncated NAL length")
		}
		n := 0
		for _, b := range body[:f.lenSize] {
			n = n<<8 | int(b)
		}
		body = body[f.lenSize:]
		if n > len(body) {
			return nil, errors.New("flv: NAL unit overruns its tag")
		}
		out = append(append(out, startCode...), body[:n]...)
		body = body[n:]
	}
	return out, nil
}
