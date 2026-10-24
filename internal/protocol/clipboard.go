package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// BulkClipboard is the bulk kind of clipboard content a control message
// cannot carry: HTML, an image, or text longer than fits. The payload is
// EncodeClip's. Plain text that fits a control message still goes as
// ControlClipboard.
const BulkClipboard = "clipboard"

// MaxClip bounds the data of one clipboard content, every format of it
// together, in either direction.
const MaxClip = 16 << 20

// ClipBulkLimit is the most bytes a BulkClipboard transfer may carry:
// MaxClip of data and the framing of its three parts.
const ClipBulkLimit = MaxClip + 64

// The clipboard formats carried, by MIME type. They are the three types the
// browser's asynchronous clipboard API can write.
const (
	MIMEText = "text/plain"
	MIMEHTML = "text/html"
	MIMEPNG  = "image/png"
)

// ErrClip means a clipboard payload is malformed.
var ErrClip = errors.New("protocol: malformed clipboard payload")

// Clip is one clipboard content in each format it was offered in: the same
// thing as UTF-8 text, as HTML and as a PNG image. Empty fields are formats
// it lacks.
type Clip struct {
	Text string
	HTML string
	PNG  []byte
}

// Size is the bytes of data c holds.
func (c Clip) Size() int { return len(c.Text) + len(c.HTML) + len(c.PNG) }

// TextOnly reports whether c is plain text and nothing else.
func (c Clip) TextOnly() bool { return c.Text != "" && c.HTML == "" && len(c.PNG) == 0 }

// EncodeClip lays c out as a BulkClipboard payload: for each format it
// holds, in the order text, HTML, PNG, one byte of MIME type length, the
// MIME type, four bytes of big-endian data length and the data.
func EncodeClip(c Clip) ([]byte, error) {
	if c.Size() > MaxClip {
		return nil, fmt.Errorf("%w: %d bytes, the limit is %d", ErrClip, c.Size(), MaxClip)
	}
	var out []byte
	for _, p := range []struct {
		mime string
		data []byte
	}{{MIMEText, []byte(c.Text)}, {MIMEHTML, []byte(c.HTML)}, {MIMEPNG, c.PNG}} {
		if len(p.data) == 0 {
			continue
		}
		out = append(out, byte(len(p.mime)))
		out = append(out, p.mime...)
		out = binary.BigEndian.AppendUint32(out, uint32(len(p.data)))
		out = append(out, p.data...)
	}
	return out, nil
}

// DecodeClip reads a BulkClipboard payload. Formats it does not know are
// skipped, so a newer sender can add some; a format given twice, text that
// is not UTF-8, data past the end and more than MaxClip of data are errors.
func DecodeClip(b []byte) (Clip, error) {
	var c Clip
	seen := map[string]bool{}
	total := 0
	for len(b) > 0 {
		n := int(b[0])
		if len(b) < 1+n+4 {
			return Clip{}, fmt.Errorf("%w: part header runs past the end", ErrClip)
		}
		mime := string(b[1 : 1+n])
		size := binary.BigEndian.Uint32(b[1+n:])
		b = b[1+n+4:]
		if uint64(size) > uint64(len(b)) {
			return Clip{}, fmt.Errorf("%w: %s data runs past the end", ErrClip, mime)
		}
		data := b[:size]
		b = b[size:]
		if seen[mime] {
			return Clip{}, fmt.Errorf("%w: %s given twice", ErrClip, mime)
		}
		seen[mime] = true
		if total += len(data); total > MaxClip {
			return Clip{}, fmt.Errorf("%w: more than %d bytes", ErrClip, MaxClip)
		}
		switch mime {
		case MIMEText, MIMEHTML:
			if !utf8.Valid(data) {
				return Clip{}, fmt.Errorf("%w: %s is not UTF-8", ErrClip, mime)
			}
			if mime == MIMEText {
				c.Text = string(data)
			} else {
				c.HTML = string(data)
			}
		case MIMEPNG:
			c.PNG = data
		}
	}
	return c, nil
}
