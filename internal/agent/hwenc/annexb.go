package hwenc

import (
	"bytes"
	"errors"
)

// NAL unit types the encoder cares about.
const (
	nalSlice = 1
	nalIDR   = 5
	nalSPS   = 7
	nalPPS   = 8
)

var startCode = []byte{0, 0, 0, 1}

// nalUnits splits an Annex-B access unit into its NAL units, start codes
// removed.
func nalUnits(au []byte) [][]byte {
	var out [][]byte
	begin := -1
	for i := 0; i+2 < len(au); {
		if au[i] != 0 || au[i+1] != 0 || au[i+2] != 1 {
			i++
			continue
		}
		if begin >= 0 {
			out = append(out, trimZeros(au[begin:i]))
		}
		i += 3
		begin = i
	}
	if begin >= 0 && begin < len(au) {
		out = append(out, au[begin:])
	}
	return out
}

// trimZeros drops the trailing zeros before a start code: the leading zero
// of a four-byte start code, or trailing_zero_8bits.
func trimZeros(nal []byte) []byte {
	return bytes.TrimRight(nal, "\x00")
}

func nalType(nal []byte) int {
	if len(nal) == 0 {
		return -1
	}
	return int(nal[0] & 0x1f)
}

// paramSets keeps the latest SPS and PPS so that every IDR the browser gets
// carries them: a decoder that joins or recovers starts from the IDR alone,
// and some encoders, and every encoder behind FLV, send them only once.
type paramSets struct {
	sps, pps [][]byte
}

// note records the parameter sets in au, if it has any.
func (p *paramSets) note(nals [][]byte) {
	var sps, pps [][]byte
	for _, n := range nals {
		switch nalType(n) {
		case nalSPS:
			sps = append(sps, bytes.Clone(n))
		case nalPPS:
			pps = append(pps, bytes.Clone(n))
		}
	}
	if len(sps) > 0 {
		p.sps = sps
	}
	if len(pps) > 0 {
		p.pps = pps
	}
}

// complete returns au with the remembered SPS and PPS in front when it holds
// an IDR slice without them.
func (p *paramSets) complete(au []byte) []byte {
	nals := nalUnits(au)
	p.note(nals)
	idr, hasSPS := false, false
	for _, n := range nals {
		switch nalType(n) {
		case nalIDR:
			idr = true
		case nalSPS:
			hasSPS = true
		}
	}
	if !idr || hasSPS || len(p.sps) == 0 {
		return au
	}
	var out []byte
	for _, n := range append(p.sps, p.pps...) {
		out = append(append(out, startCode...), n...)
	}
	return append(out, au...)
}

const profileBaseline = 66

// profile reports the profile_idc of the first SPS in au.
func profile(au []byte) (byte, error) {
	for _, n := range nalUnits(au) {
		if nalType(n) == nalSPS {
			if len(n) < 2 {
				return 0, errors.New("short SPS")
			}
			return n[1], nil
		}
	}
	return 0, errors.New("no SPS")
}

// hasIDR reports whether au holds an IDR slice.
func hasIDR(au []byte) bool {
	for _, n := range nalUnits(au) {
		if nalType(n) == nalIDR {
			return true
		}
	}
	return false
}
