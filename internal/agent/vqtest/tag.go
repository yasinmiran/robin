package vqtest

import (
	"bytes"
	"strconv"
)

// tagUUID marks the SEI message that carries a frame's index in its clip.
var tagUUID = []byte("robin-vqtest-idx")

// Tag returns a copy of au with an SEI message that names the clip frame it
// encodes, so the browser side can score each decoded picture against its
// own source. The message goes before the first slice, where SEI belongs;
// decoders skip unregistered user data.
func Tag(au []byte, index int) []byte {
	digits := strconv.Itoa(index)
	sei := append([]byte{0, 0, 0, 1, nalSEI, 5, byte(len(tagUUID) + len(digits))}, tagUUID...)
	sei = append(append(sei, digits...), 0x80)
	at := firstSlice(au)
	out := make([]byte, 0, len(au)+len(sei))
	return append(append(append(out, au[:at]...), sei...), au[at:]...)
}

// firstSlice returns where the start code of the first slice in au begins,
// or len(au) when it has none.
func firstSlice(au []byte) int {
	for i := 0; i+3 < len(au); i++ {
		if au[i] != 0 || au[i+1] != 0 || au[i+2] != 1 {
			continue
		}
		if t := au[i+3] & 0x1f; t == 1 || t == nalIDR {
			if i > 0 && au[i-1] == 0 {
				return i - 1
			}
			return i
		}
	}
	return len(au)
}

// FrameIndex finds the index Tag put in au.
func FrameIndex(au []byte) (int, bool) {
	marker := []byte{0, 0, 1, nalSEI, 5}
	for at := 0; ; {
		i := bytes.Index(au[at:], marker)
		if i < 0 {
			return 0, false
		}
		at += i + len(marker)
		if at >= len(au) {
			return 0, false
		}
		size := int(au[at])
		body := au[at+1 : min(at+1+size, len(au))]
		if size <= len(tagUUID) || len(body) < size || !bytes.HasPrefix(body, tagUUID) {
			continue
		}
		n, err := strconv.Atoi(string(body[len(tagUUID):]))
		if err != nil {
			continue
		}
		return n, true
	}
}

// IsKeyframe reports whether an Annex-B access unit holds an IDR slice.
func IsKeyframe(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 && au[i+3]&0x1f == nalIDR {
			return true
		}
	}
	return false
}
