//go:build linux

package hwenc

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// The fixtures are four 128x96 frames from vaapih264enc through multipartmux
// and from ffmpeg's h264_vaapi as FLV, recorded on an Intel iGPU.

func readAll(t *testing.T, s splitter) [][]byte {
	t.Helper()
	var aus [][]byte
	for {
		au, err := s.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("after %d access units: %v", len(aus), err)
			}
			return aus
		}
		aus = append(aus, au)
	}
}

func types(au []byte) []int {
	var ts []int
	for _, n := range nalUnits(au) {
		ts = append(ts, nalType(n))
	}
	return ts
}

func TestSplitRecordedStreams(t *testing.T) {
	for _, tc := range []struct {
		file  string
		split framing
		first []int // NAL types of the first access unit
	}{
		// The AUD (9) and SEI (6) are GStreamer's; the browser's payloader
		// drops the AUD.
		{"testdata/vaapi.multipart", newMultipartReader, []int{9, 7, 6, 8, 5}},
		// FLV carries the SPS and PPS in its sequence header; the reader
		// puts them back in front of the IDR.
		{"testdata/vaapi.flv", newFLVReader, []int{7, 8, 5}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			aus := readAll(t, tc.split(bytes.NewReader(data)))
			if len(aus) != 4 {
				t.Fatalf("%d access units, want 4", len(aus))
			}
			if got := types(aus[0]); !slices.Equal(got, tc.first) {
				t.Errorf("first access unit has NAL types %v, want %v", got, tc.first)
			}
			if idc, err := profile(aus[0]); err != nil || idc != profileBaseline {
				t.Errorf("profile %d, %v", idc, err)
			}
			for i, au := range aus[1:] {
				if hasIDR(au) || !slices.Contains(types(au), nalSlice) {
					t.Errorf("access unit %d has NAL types %v, want one P slice", i+1, types(au))
				}
			}
		})
	}
}

func TestMultipartRejectsGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"bad length":   "--b\r\nContent-Length: -4\r\n\r\n",
		"huge length":  "--b\r\nContent-Length: 99999999999\r\n\r\n",
		"long headers": strings.Repeat("X-Pad: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n", 200),
		"no newline":   strings.Repeat("a", 70000),
		"short data":   "--b\r\nContent-Length: 10\r\n\r\nabc",
	} {
		if _, err := newMultipartReader(strings.NewReader(in)).next(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestFLVRejectsGarbage(t *testing.T) {
	header := flvHeader
	sequence := string(flvSequence(nil, fakeSPS, fakePPS))
	for name, in := range map[string]string{
		"not flv":      "GIF89a" + strings.Repeat("\x00", 20),
		"video first":  header + string(flvTag(nil, flvTagVideo, []byte{0x17, flvAVCNALU, 0, 0, 0, 0, 0, 0, 1, 0x65})),
		"bad sequence": header + string(flvTag(nil, flvTagVideo, []byte{0x17, flvAVCSequence, 0, 0, 0, 1, 66, 0xc0, 31, 0xff, 0xe1, 0, 40})),
		"overrun":      header + sequence + string(flvTag(nil, flvTagVideo, []byte{0x17, flvAVCNALU, 0, 0, 0, 0, 0, 0, 9, 0x65})),
		"short tag":    header + sequence + "\x09\x00\x01\x00",
	} {
		if _, err := newFLVReader(strings.NewReader(in)).next(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParamSetsCompleteIDR(t *testing.T) {
	annexB := func(nals ...[]byte) []byte {
		var b []byte
		for _, n := range nals {
			b = append(append(b, startCode...), n...)
		}
		return b
	}
	idr, p := fakeSlice(0, 0, 8), fakeSlice(1, 0, 8)
	var s paramSets
	first := annexB(fakeSPS, fakePPS, idr)
	if got := s.complete(first); !bytes.Equal(got, first) {
		t.Fatal("an IDR that has its parameter sets was changed")
	}
	if got := s.complete(annexB(p)); !bytes.Equal(got, annexB(p)) {
		t.Fatal("a P frame was changed")
	}
	if got := s.complete(annexB(idr)); !bytes.Equal(got, first) {
		t.Fatalf("bare IDR came out with NAL types %v", types(got))
	}
	// Three-byte start codes and trailing zeros split the same way.
	odd := append([]byte{0, 0, 1}, append(append(slices.Clone(fakeSPS), 0, 0, 0, 1), idr...)...)
	if got := types(odd); !slices.Equal(got, []int{7, 5}) {
		t.Fatalf("NAL types %v", got)
	}
	if n := nalUnits(odd); !bytes.Equal(n[0], fakeSPS) {
		t.Fatalf("SPS came out as % x", n[0])
	}
}
