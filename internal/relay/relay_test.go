package relay

import (
	"errors"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/protocol"
)

const (
	udpHost  = "candidate:1 1 udp 2130706431 10.89.0.2 50000 typ host"
	udpSrflx = "candidate:2 1 udp 1694498815 203.0.113.7 50000 typ srflx raddr 10.89.0.2 rport 50000"
	udpLoop  = "candidate:5 1 udp 2130706431 127.0.0.1 50001 typ host"
	udpLoop6 = "candidate:6 1 udp 2130706431 ::1 50002 typ host"
	tcpHost  = "candidate:3 1 tcp 1671430143 10.89.0.2 41000 typ host tcptype passive"
	tcpLoop  = "candidate:4 1 TCP 1671430143 127.0.0.1 41000 typ host tcptype passive"
	fpA      = "sha-256 AB:CD:EF"
)

func answer(fp string, candidates ...string) string {
	var b strings.Builder
	b.WriteString("v=0\r\no=- 1 2 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\na=group:BUNDLE 0 1\r\n")
	if fp != "" {
		b.WriteString("a=fingerprint:" + fp + "\r\n")
	}
	b.WriteString("m=video 9 UDP/TLS/RTP/SAVPF 102\r\nc=IN IP4 0.0.0.0\r\na=mid:0\r\n")
	for _, c := range candidates {
		b.WriteString("a=" + c + "\r\n")
	}
	b.WriteString("m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:1\r\n")
	return b.String()
}

func cand(c string) protocol.Message {
	return protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{Candidate: c}}
}

func TestCandidateSelection(t *testing.T) {
	all := []string{udpHost, udpSrflx, udpLoop, udpLoop6, tcpHost, tcpLoop, ""}
	for _, tc := range []struct {
		name string
		p    Policy
		want []string
	}{
		{"auto keeps udp, drops agent tcp", Policy{Transport: Auto, TunnelPort: 4000}, []string{udpHost, udpSrflx, ""}},
		{"direct keeps udp", Policy{Transport: Direct, TunnelPort: 4000}, []string{udpHost, udpSrflx, ""}},
		{"tunnel drops all agent candidates", Policy{Transport: Tunnel, TunnelPort: 4000}, []string{""}},
		{"no tunnel port still never passes tcp", Policy{Transport: Auto}, []string{udpHost, udpSrflx, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(tc.p)
			var got []string
			for _, c := range all {
				out, err := r.FromAgent(cand(c))
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range out {
					got = append(got, m.Candidate.Candidate)
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestAnswerGetsTunnelCandidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		p          Policy
		wantTunnel bool
	}{
		{"auto", Policy{Transport: Auto, TunnelPort: 4000}, true},
		{"tunnel", Policy{Transport: Tunnel, TunnelPort: 4000}, true},
		{"direct", Policy{Transport: Direct, TunnelPort: 4000}, false},
		{"no forward", Policy{Transport: Auto}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := New(tc.p).FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: answer(fpA, udpHost, tcpHost)})
			if err != nil {
				t.Fatal(err)
			}
			if out[0].Type != protocol.TypeAnswer {
				t.Fatalf("first message %q, want the answer", out[0].Type)
			}
			if strings.Contains(out[0].SDP, "tcptype") {
				t.Fatal("agent tcp candidate leaked through the SDP")
			}
			if got := strings.Contains(out[0].SDP, udpHost); got == (tc.p.Transport == Tunnel) {
				t.Fatalf("udp candidate present=%v under %s", got, tc.p.Transport)
			}
			if !tc.wantTunnel {
				if len(out) != 1 {
					t.Fatalf("unexpected extra messages: %+v", out[1:])
				}
				return
			}
			if len(out) != 2 || out[1].Candidate == nil {
				t.Fatalf("want answer + tunnel candidate, got %+v", out)
			}
			c := out[1].Candidate
			if c.Candidate != "candidate:robintunnel 1 tcp 16777215 127.0.0.1 4000 typ host tcptype passive" {
				t.Fatalf("tunnel candidate %q", c.Candidate)
			}
			if *c.SDPMid != "0" || *c.SDPMLineIndex != 0 {
				t.Fatalf("tunnel candidate attached to mid %q index %d", *c.SDPMid, *c.SDPMLineIndex)
			}
		})
	}
}

func TestTunnelRanksBelowAnyUDPPair(t *testing.T) {
	// RFC 8445 pair priority with the browser as controlling agent G and the
	// agent's candidate as D. Even if the browser ranks its own TCP candidate
	// above its UDP one, the tunnel pair must lose to a direct UDP pair.
	pair := func(g, d uint64) uint64 {
		lo, hi := min(g, d), max(g, d)
		p := lo<<32 + 2*hi
		if g > d {
			p++
		}
		return p
	}
	const browserUDP, browserTCP, agentUDPWorst = 2122260223, 2130706431, 1 << 24
	if pair(browserTCP, TunnelPriority) >= pair(browserUDP, agentUDPWorst) {
		t.Fatal("tunnel pair outranks a direct UDP pair")
	}
}

func TestFingerprintPinning(t *testing.T) {
	r := New(Policy{Transport: Auto, TunnelPort: 4000})
	if _, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: answer("SHA-256 ab:cd:ef")}); err != nil {
		t.Fatal(err)
	}
	if r.Pinned() != fpA {
		t.Fatalf("pinned %q", r.Pinned())
	}
	// An ICE restart answer from the same agent passes; the same text in other
	// case is the same fingerprint.
	if _, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: answer(fpA)}); err != nil {
		t.Fatalf("restart answer rejected: %v", err)
	}
	_, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: answer("sha-256 11:22:33")})
	if !errors.Is(err, ErrFingerprintChanged) {
		t.Fatalf("want ErrFingerprintChanged, got %v", err)
	}
}

func TestFingerprintRejectsMissingOrConflicting(t *testing.T) {
	if _, err := Fingerprint(answer("")); err == nil {
		t.Fatal("answer without fingerprint accepted")
	}
	conflicting := answer(fpA) + "a=fingerprint:sha-256 11:22:33\r\n"
	if _, err := Fingerprint(conflicting); err == nil {
		t.Fatal("conflicting fingerprints accepted")
	}
	if _, err := New(Policy{}).FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: answer("")}); err == nil {
		t.Fatal("relay forwarded an answer without fingerprint")
	}
}

func TestParseTransport(t *testing.T) {
	for _, s := range []string{"auto", "direct", "tunnel"} {
		if _, err := ParseTransport(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := ParseTransport("udp"); err == nil {
		t.Error("accepted unknown transport")
	}
}

// A candidate reaches the browser only with an address it can take for
// no other: an IP literal that is not loopback, unspecified or multicast,
// written as the browser splits it. A name could resolve to the browser's
// own machine, as could the many other ways to write loopback.
func TestCandidateNeedsARoutableLiteral(t *testing.T) {
	r := New(Policy{Transport: Auto, TunnelPort: 4000})
	for _, c := range []string{
		"candidate:7 1 udp 2130706431 localhost 50000 typ host",
		"candidate:7 1 udp 2130706431 x.local 50000 typ host",
		"candidate:7 1 udp 2130706431 127.1 50000 typ host",
		"candidate:7 1 udp 2130706431 127.000.0.1 50000 typ host",
		"candidate:7 1 udp 2130706431 0.0.0.0 50000 typ host",
		"candidate:7 1 udp 2130706431 :: 50000 typ host",
		"candidate:7 1 udp 2130706431 ::ffff:127.0.0.1 50000 typ host",
		"candidate:7 1 udp 2130706431 [::ffff:127.0.0.1] 50000 typ host",
		"candidate:7 1 udp 2130706431 [10.89.0.2] 50000 typ host",
		"candidate:7 1 udp 2130706431 fe80::1%eth0 50000 typ host",
		"candidate:7 1 udp 2130706431 224.0.0.251 50000 typ host",
		"candidate:7 1 udp 2130706431\t10.89.0.2 127.0.0.1 50000 typ host",
		"candidate:7 1 udp 2130706431  10.89.0.2 50000 typ host",
		udpHost + "\r\na=" + udpLoop,
		udpHost + " ",
	} {
		if out, err := r.FromAgent(cand(c)); err != nil || len(out) != 0 {
			t.Errorf("%q reached the browser as %+v (%v)", c, out, err)
		}
		out, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: answer(fpA, c, udpHost)})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out[0].SDP, c) || !strings.Contains(out[0].SDP, udpHost) {
			t.Errorf("the answer with %q became %q", c, out[0].SDP)
		}
	}
	for _, c := range []string{udpHost, udpSrflx, "candidate:9 1 udp 2130706431 2001:db8::1 50000 typ host", "candidate:9 1 udp 2130706431 fe80::1 50000 typ host"} {
		if out, _ := r.FromAgent(cand(c)); len(out) != 1 {
			t.Errorf("%q did not reach the browser", c)
		}
	}
}

// An answer with a CR inside a line is refused before anything is pinned:
// a parser that ends lines at CR as well would find a candidate in it the
// relay never filtered.
func TestAnswerWithABareCRIsRefused(t *testing.T) {
	for _, sdp := range []string{
		strings.Replace(answer(fpA, udpHost), "a=mid:0\r\n", "a=mid:0\ra="+udpLoop+"\r\n", 1),
		answer(fpA) + "\r",
	} {
		r := New(Policy{Transport: Auto, TunnelPort: 4000})
		if out, err := r.FromAgent(protocol.Message{Type: protocol.TypeAnswer, SDP: sdp}); !errors.Is(err, ErrBareCR) || out != nil || r.Pinned() != "" {
			t.Errorf("an answer with a bare CR: %+v, %v, pinned %q", out, err, r.Pinned())
		}
	}
}
