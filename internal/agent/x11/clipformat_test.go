//go:build linux

package x11

import (
	"slices"
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestDecodeHTML(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []byte
		want string
	}{
		"utf-8":            {[]byte("<b>é</b>"), "<b>é</b>"},
		"utf-16le bom":     {[]byte{0xff, 0xfe, '<', 0, 'p', 0, '>', 0, 0xe9, 0}, "<p>é"},
		"utf-16be bom":     {[]byte{0xfe, 0xff, 0, '<', 0, 'i', 0, '>'}, "<i>"},
		"surrogate pair":   {[]byte{0xff, 0xfe, 0x3d, 0xd8, 0x00, 0xde}, "😀"},
		"odd trailing":     {[]byte{0xff, 0xfe, 'a', 0, 'b'}, "a"},
		"invalid utf-8":    {[]byte("a\xffb"), "a�b"},
		"just a bom":       {[]byte{0xff, 0xfe}, ""},
		"one byte, no bom": {[]byte{0xff}, "�"},
	} {
		if got := decodeHTML(tc.in); got != tc.want {
			t.Errorf("%s: decodeHTML = %q, want %q", name, got, tc.want)
		}
	}
}

func TestDecodeText(t *testing.T) {
	if got := decodeText(xproto.AtomString, []byte{'c', 'a', 'f', 0xe9}); got != "café" {
		t.Errorf("STRING as Latin-1: %q", got)
	}
	if got := decodeText(0, []byte("ok \xff")); got != "ok �" {
		t.Errorf("UTF8_STRING with a bad byte: %q", got)
	}
}

func TestWantedTargets(t *testing.T) {
	cb := &clipboard{a: atoms{utf8: 10, plain: 11, text: 12, html: 20, png: 21, targets: 30}}
	for _, tc := range []struct {
		offered, want []xproto.Atom
	}{
		{[]xproto.Atom{30, 12, 11, 10, xproto.AtomString}, []xproto.Atom{10}},
		{[]xproto.Atom{xproto.AtomString, 11}, []xproto.Atom{11}},
		{[]xproto.Atom{xproto.AtomString}, []xproto.Atom{xproto.AtomString}},
		{[]xproto.Atom{12}, nil}, // TEXT alone may be COMPOUND_TEXT
		{[]xproto.Atom{21, 30, 20, 10}, []xproto.Atom{10, 20, 21}},
		{[]xproto.Atom{21}, []xproto.Atom{21}},
		{nil, nil},
	} {
		if got := cb.wanted(tc.offered); !slices.Equal(got, tc.want) {
			t.Errorf("wanted(%v) = %v, want %v", tc.offered, got, tc.want)
		}
	}
	list := []xproto.Atom{1, 2, 0xdeadbeef}
	if got := atomList(atomBytes(list)); !slices.Equal(got, list) {
		t.Errorf("atomList(atomBytes(%v)) = %v", list, got)
	}
}
