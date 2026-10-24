package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func part(mime string, data []byte) []byte {
	out := append([]byte{byte(len(mime))}, mime...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

func TestClipRoundTrip(t *testing.T) {
	for name, c := range map[string]Clip{
		"empty":       {},
		"text":        {Text: "héllo"},
		"html":        {HTML: "<b>hi</b>"},
		"png":         {PNG: []byte{0x89, 'P', 'N', 'G', 0}},
		"all three":   {Text: "hi", HTML: "<i>hi</i>", PNG: []byte{1, 2, 3}},
		"at the cap":  {Text: strings.Repeat("x", MaxClip-1), PNG: []byte{7}},
		"binary text": {Text: "\x00\x01"},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := EncodeClip(c)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) > ClipBulkLimit {
				t.Errorf("encoded %d bytes, over ClipBulkLimit %d", len(b), ClipBulkLimit)
			}
			got, err := DecodeClip(b)
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != c.Text || got.HTML != c.HTML || !bytes.Equal(got.PNG, c.PNG) {
				t.Errorf("round trip changed the content: %d/%d/%d bytes, want %d/%d/%d",
					len(got.Text), len(got.HTML), len(got.PNG), len(c.Text), len(c.HTML), len(c.PNG))
			}
		})
	}
}

func TestClipWireShape(t *testing.T) {
	b, err := EncodeClip(Clip{Text: "a", PNG: []byte{9, 8}})
	if err != nil {
		t.Fatal(err)
	}
	want := append(part(MIMEText, []byte("a")), part(MIMEPNG, []byte{9, 8})...)
	if !bytes.Equal(b, want) {
		t.Errorf("EncodeClip = %x, want %x", b, want)
	}
}

func TestClipRefusesOversize(t *testing.T) {
	if _, err := EncodeClip(Clip{Text: strings.Repeat("x", MaxClip), HTML: "y"}); !errors.Is(err, ErrClip) {
		t.Errorf("EncodeClip over MaxClip: %v, want ErrClip", err)
	}
	big := append(part(MIMEText, bytes.Repeat([]byte("x"), MaxClip)), part(MIMEPNG, []byte{1})...)
	if _, err := DecodeClip(big); !errors.Is(err, ErrClip) {
		t.Errorf("DecodeClip over MaxClip: %v, want ErrClip", err)
	}
}

func TestDecodeClipSkipsUnknownFormats(t *testing.T) {
	b := append(part("image/jpeg", []byte{1, 2}), part(MIMEHTML, []byte("<p>"))...)
	c, err := DecodeClip(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTML != "<p>" || c.Text != "" || c.PNG != nil {
		t.Errorf("DecodeClip = %+v, want only the HTML", c)
	}
}

func TestDecodeClipRefusesJunk(t *testing.T) {
	text := part(MIMEText, []byte("hi"))
	for name, b := range map[string][]byte{
		"short header":     {4, 't', 'e'},
		"no length":        {1, 'x', 0, 0},
		"data past end":    text[:len(text)-1],
		"twice":            append(append([]byte{}, text...), text...),
		"text not utf-8":   part(MIMEText, []byte{0xff, 0xfe}),
		"html not utf-8":   part(MIMEHTML, []byte{0xc3}),
		"huge length":      append([]byte{1, 'x'}, 0xff, 0xff, 0xff, 0xff),
		"trailing garbage": append(append([]byte{}, text...), 9),
	} {
		t.Run(name, func(t *testing.T) {
			if c, err := DecodeClip(b); !errors.Is(err, ErrClip) {
				t.Errorf("DecodeClip = %+v, %v, want ErrClip", c, err)
			}
		})
	}
}

func TestClipTextOnly(t *testing.T) {
	for _, tc := range []struct {
		c    Clip
		want bool
	}{
		{Clip{Text: "a"}, true},
		{Clip{}, false},
		{Clip{Text: "a", HTML: "<b>a</b>"}, false},
		{Clip{Text: "a", PNG: []byte{1}}, false},
	} {
		if got := tc.c.TextOnly(); got != tc.want {
			t.Errorf("%+v.TextOnly() = %v, want %v", tc.c, got, tc.want)
		}
	}
}
