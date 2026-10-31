package relay

import (
	"bytes"
	"encoding/binary"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/pion/sdp/v3"
	"github.com/pion/stun/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// sdpSeeds are answers as the agent writes them, with candidates of every
// kind the relay filters, and some no agent would write.
func sdpSeeds() []string {
	all := []string{udpHost, udpSrflx, udpLoop, udpLoop6, tcpHost, tcpLoop}
	creds := "a=ice-ufrag:" + agentUfrag + "\r\na=ice-pwd:" + agentPwd + "\r\n"
	return []string{
		answer(fpA, all...),
		answer(fpA),
		answer(""),
		strings.Replace(answer(fpA, udpHost), "a=mid:0\r\n", "a=mid:0\r\n"+creds, 1),
		answer(fpA) + "a=fingerprint:sha-256 00:11\r\n",
		answer("sha-256"),
		"a=candidate:1 1 udp 1 127.0.0.1 1 typ host\nm=video\na=mid:\n",
		"m=a\nm=b\na=mid:0\n",
		strings.Replace(answer(fpA, udpHost), "a=mid:0\r\n", "a=mid:0\ra="+udpLoop+"\r\n", 1),
		"a=fingerprint: \r\n",
		"",
	}
}

// FuzzFromAgent passes an answer and a candidate from the agent through
// the relay under every policy. The browser never gets a TCP candidate of
// the agent's or a loopback one, the answer keeps every line but the
// candidates it drops, filtering is idempotent, and the pinned fingerprint
// never changes once set.
func FuzzFromAgent(f *testing.F) {
	candidates := []string{udpHost, tcpLoop, udpLoop6, ""}
	for _, addr := range []string{"localhost", "127.1", "0.0.0.0", "::", "[::ffff:127.0.0.1]", "::ffff:127.0.0.1", "fe80::1%eth0", "x.local"} {
		candidates = append(candidates, "candidate:7 1 udp 2130706431 "+addr+" 50003 typ host")
	}
	candidates = append(candidates, "candidate:8 1 udp 2130706431\t10.89.0.2 127.0.0.1 50004 typ host")
	for i, sdp := range sdpSeeds() {
		f.Add(sdp, candidates[i%len(candidates)], uint8(i), uint16(4000))
	}
	for i, c := range candidates {
		f.Add(answer(fpA, c), c, uint8(i), uint16(4000))
	}
	f.Fuzz(func(t *testing.T, sdp, candidate string, transport uint8, port uint16) {
		p := Policy{Transport: []Transport{Auto, Direct, Tunnel}[transport%3], TunnelPort: int(port)}
		r := New(p)
		out, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: sdp})
		if err == nil && strings.Contains(strings.ReplaceAll(sdp, "\r\n", "\n"), "\r") {
			t.Fatalf("passed an answer with a CR inside a line: %q", sdp)
		}
		if err != nil {
			if r.Pinned() != "" {
				t.Fatalf("a refused answer pinned %q", r.Pinned())
			}
		} else {
			pinned := r.Pinned()
			if pinned == "" {
				t.Fatal("an answer passed without pinning a fingerprint")
			}
			checkAnswer(t, r, sdp, out)
			// The same answer again passes, and keeps the pin.
			if _, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: sdp}); err != nil || r.Pinned() != pinned {
				t.Fatalf("the same answer again: %v, pinned %q then %q", err, pinned, r.Pinned())
			}
		}
		got, err := r.FromAgent(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{Candidate: candidate}})
		if err != nil {
			t.Fatalf("a candidate failed the relay: %v", err)
		}
		for _, m := range got {
			checkCandidate(t, p, m.Candidate.Candidate)
		}
	})
}

func checkAnswer(t *testing.T, r *Relay, sdp string, out []protocol.Message) {
	t.Helper()
	if len(out) == 0 || out[0].Type != protocol.TypeAnswer {
		t.Fatalf("an answer became %+v", out)
	}
	filtered := out[0].SDP
	if again := r.filterSDP(filtered); again != filtered {
		t.Fatalf("filtering again changed the answer:\n%q\n%q", filtered, again)
	}
	kept := strings.SplitAfter(filtered, "\n")
	in := strings.SplitAfter(sdp, "\n")
	// What is left is the answer's lines in order, less some candidates.
	j := 0
	for _, l := range kept {
		for j < len(in) && in[j] != l {
			if !strings.HasPrefix(in[j], "a=candidate:") {
				t.Fatalf("dropped the line %q", in[j])
			}
			j++
		}
		if j == len(in) {
			t.Fatalf("the line %q is not the answer's", l)
		}
		j++
	}
	for _, l := range in[j:] {
		if !strings.HasPrefix(l, "a=candidate:") {
			t.Fatalf("dropped the line %q", l)
		}
	}
	for _, l := range kept {
		if c, ok := strings.CutPrefix(strings.TrimRight(l, "\r\n"), "a="); ok && strings.HasPrefix(c, "candidate:") {
			checkCandidate(t, r.policy, c)
		}
	}
	for _, m := range out[1:] {
		if m.Type != protocol.TypeCandidate || m.Candidate.Candidate != TunnelCandidate(r.policy.TunnelPort) || !r.tunnelEnabled() {
			t.Fatalf("the answer brought %+v", m)
		}
	}
}

// checkCandidate fails unless c may reach the browser under p. It parses
// on its own rather than with the relay's helpers, so that it does not
// agree with them by construction.
func checkCandidate(t *testing.T, p Policy, c string) {
	t.Helper()
	if c == "" {
		return
	}
	f := strings.Split(c, " ")
	if p.Transport == Tunnel || slices.Contains(f, "") || strings.ContainsFunc(c, func(r rune) bool { return r != ' ' && unicode.IsSpace(r) }) ||
		len(f) < 8 || !strings.EqualFold(f[2], "udp") {
		t.Fatalf("the browser got %q under %+v", c, p)
	}
	addr, err := netip.ParseAddr(f[4])
	if err != nil || addr.Zone() != "" || addr.Unmap().IsLoopback() || addr.Unmap().IsUnspecified() {
		t.Fatalf("the browser got %q, whose address is not one it may send checks to", c)
	}
}

// FuzzAnswerCredentials feeds the answers the tunnel learns the agent's
// ICE credentials from. An answer whose a=ice-ufrag lines, or a=ice-pwd
// lines, are missing, disagree or hold an empty value is refused: the
// tunnel would otherwise admit checks signed with a password the agent
// does not use. Any other answer gives its one pair.
func FuzzAnswerCredentials(f *testing.F) {
	for _, sdp := range sdpSeeds() {
		f.Add(sdp)
	}
	f.Add("a=ice-ufrag:u\na=ice-pwd:p\na=ice-pwd:q\n")
	f.Add("a=ice-ufrag:u\na=ice-ufrag:v\na=ice-pwd:p\n")
	f.Add("a=ice-ufrag:\na=ice-ufrag:u\na=ice-pwd:p\n")
	f.Add("a=ice-ufrag:u\r\na=ice-ufrag:u\r\na=ice-pwd:p\r\na=ice-ufrag:\r\n")
	f.Fuzz(func(t *testing.T, sdp string) {
		ufrags, pwds := map[string]bool{}, map[string]bool{}
		for l := range strings.SplitSeq(sdp, "\n") {
			l = strings.TrimRight(l, "\r")
			if v, ok := strings.CutPrefix(l, "a=ice-ufrag:"); ok {
				ufrags[v] = true
			} else if v, ok := strings.CutPrefix(l, "a=ice-pwd:"); ok {
				pwds[v] = true
			}
		}
		c, err := AnswerCredentials(sdp)
		if len(ufrags) != 1 || len(pwds) != 1 || ufrags[""] || pwds[""] {
			if err == nil || c != (Credentials{}) {
				t.Fatalf("took %+v from the ufrags %q and passwords %q", c, slices.Collect(maps.Keys(ufrags)), slices.Collect(maps.Keys(pwds)))
			}
			return
		}
		if err != nil || !ufrags[c.Ufrag] || !pwds[c.Pwd] {
			t.Fatalf("credentials %+v (%v) are not the answer's one pair", c, err)
		}
	})
}

// FuzzAdmits builds connectivity checks as a browser would, signed with
// the agent's password or another, then flips a byte and appends a tail.
// The tunnel admits a check only when it is a Binding request for the
// agent's ufrag whose every signed byte is as the password's holder wrote
// it, and admits every such check.
func FuzzAdmits(f *testing.F) {
	f.Add("brws", true, uint16(0), byte(0), []byte{})
	f.Add("brws", false, uint16(0), byte(0), []byte{})
	f.Add("brws", true, uint16(31), byte(1), []byte{})
	f.Add("brws", true, uint16(0), byte(0), []byte{0x80, 0x28, 0, 4, 1, 2, 3, 4})
	f.Add("", true, uint16(3), byte(4), []byte{0, 6, 0, 4, 'x', ':', 'y', 'z'})
	f.Add(strings.Repeat("u", 600), true, uint16(0), byte(0), []byte{})
	c := Credentials{Ufrag: agentUfrag, Pwd: agentPwd}
	f.Fuzz(func(t *testing.T, peer string, signed bool, at uint16, flip byte, tail []byte) {
		pwd := agentPwd
		if !signed {
			pwd = "guessedpasswordguessedpa"
		}
		m, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername(agentUfrag+":"+peer),
			stun.NewShortTermIntegrity(pwd))
		if err != nil {
			return
		}
		raw := m.Raw
		msg := append([]byte(nil), raw...)
		if flip != 0 {
			msg[int(at)%len(msg)] ^= flip
		}
		if len(msg)+len(tail)-stunHeader > 0xffff {
			return
		}
		msg = append(msg, tail...)
		binary.BigEndian.PutUint16(msg[2:4], uint16(len(msg)-stunHeader))
		// Only the header's length may differ in the signed bytes, and it
		// is checked against the message's.
		same := bytes.Equal(msg[:2], raw[:2]) && bytes.Equal(msg[4:len(raw)], raw[4:])
		got := c.Admits(msg)
		if got && !(signed && same) {
			t.Fatalf("admitted a check signed %v whose signed bytes changed %v: % x", signed, !same, msg)
		}
		if signed && same && len(tail) == 0 && !got {
			t.Fatalf("refused the agent's own check: % x", msg)
		}
		if (Credentials{}).Admits(msg) || (Credentials{Ufrag: agentUfrag}).Admits(msg) {
			t.Fatal("credentials without a password admitted a check")
		}
	})
}

// FuzzFromBrowser passes an offer and a candidate from the page through the
// relay under every policy. Through the tunnel alone, no offer that passes
// names a candidate as Pion reads it, and only the end of the candidates is
// trickled; every other transport passes both unchanged.
func FuzzFromBrowser(f *testing.F) {
	for i, sdp := range sdpSeeds() {
		f.Add(sdp, udpHost, uint8(i))
	}
	f.Add(strings.Replace(answer(fpA), "a=mid:0\r\n", "a=mid:0\n\ra="+udpHost+"\r\n", 1), "", uint8(2))
	f.Fuzz(func(t *testing.T, offer, candidate string, transport uint8) {
		tr := []Transport{Auto, Direct, Tunnel}[transport%3]
		r := New(Policy{Transport: tr, TunnelPort: 4000})
		m, ok := r.FromBrowser(protocol.Message{Type: protocol.TypeOffer, SDP: offer})
		c, cok := r.FromBrowser(cand(candidate))
		if tr != Tunnel {
			if !ok || m.SDP != offer || !cok || c.Candidate.Candidate != candidate {
				t.Fatalf("%s changed the page's offer or candidate", tr)
			}
			return
		}
		if cok && candidate != "" {
			t.Fatalf("the tunnel passed the page's candidate %q", candidate)
		}
		if !ok {
			return
		}
		var d sdp.SessionDescription
		if d.UnmarshalString(m.SDP) != nil {
			return
		}
		attrs := slices.Clone(d.Attributes)
		for _, md := range d.MediaDescriptions {
			attrs = append(attrs, md.Attributes...)
		}
		for _, a := range attrs {
			if a.IsICECandidate() {
				t.Fatalf("the tunnel passed an offer that names %q", a.Value)
			}
		}
	})
}
