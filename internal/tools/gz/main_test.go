package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

func TestWriteIsFixed(t *testing.T) {
	data := bytes.Repeat([]byte("\x7fELF agent bytes "), 4096)
	var a, b bytes.Buffer
	for _, w := range []*bytes.Buffer{&a, &b} {
		if err := Write(w, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two writes of the same input differ")
	}
	// RFC 1952: flags, MTIME (4 bytes), XFL, OS follow the two ID bytes
	// and the method.
	hdr := a.Bytes()[:10]
	if hdr[3] != 0 {
		t.Errorf("flags = %#x, want none (no name, comment or extra)", hdr[3])
	}
	if mtime := hdr[4:8]; !bytes.Equal(mtime, []byte{0, 0, 0, 0}) {
		t.Errorf("MTIME = %x, want zero", mtime)
	}
	if hdr[9] != 0xff {
		t.Errorf("OS = %#x, want unknown", hdr[9])
	}
	z, err := gzip.NewReader(&a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(z)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip: %d bytes, %v", len(got), err)
	}
}
