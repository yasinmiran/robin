// Package relay applies the CLI's transport and trust policy to the signaling
// between the agent and the browser.
//
// Transport: the agent's direct UDP candidates pass through, any TCP candidate
// of its own is dropped, and one synthesized passive TCP candidate on the
// client's loopback leads into the SSH forward. ICE priority then prefers UDP
// whenever it works, so TCP media can only ever take the SSH path. Through
// the tunnel alone the browser's candidates are kept from the agent too: the
// tunnel's connection reaches the agent peer-reflexive and needs none.
//
// Trust: the agent's DTLS fingerprint is pinned from the first answer, which
// arrived over SSH; a later answer with a different fingerprint is rejected.
package relay

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/yasinmiran/robin/internal/protocol"
)

// Transport selects which candidates the browser gets.
type Transport string

const (
	// Auto offers direct UDP and the SSH tunnel together; ICE picks.
	Auto Transport = "auto"
	// Direct offers only the agent's UDP candidates.
	Direct Transport = "direct"
	// Tunnel offers only the SSH tunnel.
	Tunnel Transport = "tunnel"
)

// ParseTransport validates a --transport flag value.
func ParseTransport(s string) (Transport, error) {
	switch t := Transport(s); t {
	case Auto, Direct, Tunnel:
		return t, nil
	}
	return "", fmt.Errorf("unknown transport %q (want auto, direct or tunnel)", s)
}

// Policy says which candidates the browser gets and how to reach the tunnel.
// It is fixed for a session.
type Policy struct {
	Transport Transport
	// TunnelPort is the client loopback port that leads into the SSH forward
	// to the agent's ICE-TCP listener. Zero means no tunnel is available.
	TunnelPort int
}

// TunnelPriority is the ICE priority of the synthesized tunnel candidate:
// type preference 0, the value RFC 8445 gives relayed candidates. ICE pair
// priority uses the lower of the two candidates' priorities, so every pair
// through the tunnel ranks below every direct UDP pair whatever the browser
// assigns to its own candidates.
const TunnelPriority = 0<<24 | 0xffff<<8 | (256 - 1)

// ErrFingerprintChanged means an answer carried a DTLS fingerprint other than
// the one pinned for this session.
var ErrFingerprintChanged = errors.New("relay: agent DTLS fingerprint changed mid-session")

// ErrBareCR means an answer had a CR that does not end a line. The relay
// reads lines as ending at LF, and a parser that also ends them at CR would
// find lines the relay never saw, a candidate among them.
var ErrBareCR = errors.New("relay: agent answer has a CR inside a line")

// Relay holds the per-session state: the policy and the pinned fingerprint.
// It is not safe for concurrent use; the bridge feeds it from one goroutine.
type Relay struct {
	policy Policy
	pinned string
}

// New returns a relay for one session.
func New(p Policy) *Relay {
	if p.Transport == "" {
		p.Transport = Auto
	}
	return &Relay{policy: p}
}

// Pinned returns the pinned agent fingerprint, or "" before the first answer.
func (r *Relay) Pinned() string { return r.pinned }

// FromAgent turns one agent message into what the browser should receive.
func (r *Relay) FromAgent(m protocol.Message) ([]protocol.Message, error) {
	switch m.Type {
	case protocol.TypeAnswer:
		if bareCR(m.SDP) {
			return nil, ErrBareCR
		}
		fp, err := Fingerprint(m.SDP)
		if err != nil {
			return nil, err
		}
		if r.pinned == "" {
			r.pinned = fp
		} else if fp != r.pinned {
			return nil, ErrFingerprintChanged
		}
		m.SDP = r.filterSDP(m.SDP)
		out := []protocol.Message{m}
		if c, ok := r.tunnelCandidate(m.SDP); ok {
			out = append(out, protocol.Message{Type: protocol.TypeCandidate, Candidate: &c})
		}
		return out, nil
	case protocol.TypeCandidate:
		if m.Candidate == nil || !r.keep(m.Candidate.Candidate) {
			return nil, nil
		}
		return []protocol.Message{m}, nil
	case protocol.TypeError:
		return []protocol.Message{m}, nil
	}
	return nil, nil
}

// FromBrowser returns what of one page message the agent should receive,
// and false when nothing should. Through the tunnel alone the agent gets
// none of the browser's candidates, in the offer or trickled after it, so
// it has no address of the browser's to check over UDP; the end of the
// candidates still passes. An offer with a CR that does not end a line is
// dropped then: Pion skips a CR at the start of a line, so it would read a
// candidate there that the filter does not see. Under any other transport
// the message passes as it is. FromBrowser reads only the policy, so it is
// safe beside FromAgent.
func (r *Relay) FromBrowser(m protocol.Message) (protocol.Message, bool) {
	if r.policy.Transport != Tunnel {
		return m, true
	}
	switch m.Type {
	case protocol.TypeOffer:
		if bareCR(m.SDP) {
			return protocol.Message{}, false
		}
		// Every attribute Pion takes for a candidate, a malformed one too.
		m.SDP = dropAttributes(m.SDP, func(attr string) bool {
			key, _, _ := strings.Cut(attr, ":")
			return key == "candidate"
		})
	case protocol.TypeCandidate:
		if m.Candidate == nil || m.Candidate.Candidate != "" {
			return protocol.Message{}, false
		}
	}
	return m, true
}

func (r *Relay) tunnelEnabled() bool {
	return r.policy.Transport != Direct && r.policy.TunnelPort > 0
}

// keep decides whether an agent candidate reaches the browser. The empty
// end-of-candidates marker always does.
func (r *Relay) keep(candidate string) bool {
	if candidate == "" {
		return true
	}
	// A browser splits a candidate at each space, the relay at any run of
	// white space: only a candidate both read alike may pass. The agent
	// gathers loopback because its tunnel listener lives there, and a
	// loopback UDP candidate would point the browser at its own machine.
	if strings.Join(strings.Fields(candidate), " ") != candidate || transportOf(candidate) != "udp" || !routable(candidate) {
		return false
	}
	return r.policy.Transport != Tunnel
}

// routable reports whether a candidate's address is one the browser may
// send checks to: an IP literal with no zone that is not loopback,
// unspecified or multicast. The agent gathers with mDNS off, so its own
// candidates are never names, and a name could resolve to anything, the
// browser's own machine included.
func routable(candidate string) bool {
	f := strings.Fields(candidate)
	if len(f) < 5 {
		return false
	}
	a, err := netip.ParseAddr(f[4])
	if err != nil || a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	return !a.IsLoopback() && !a.IsUnspecified() && !a.IsMulticast()
}

func bareCR(sdp string) bool {
	for i := range len(sdp) {
		if sdp[i] == '\r' && (i+1 == len(sdp) || sdp[i+1] != '\n') {
			return true
		}
	}
	return false
}

func (r *Relay) filterSDP(sdp string) string {
	return dropAttributes(sdp, func(attr string) bool {
		return strings.HasPrefix(attr, "candidate:") && !r.keep(attr)
	})
}

// dropAttributes removes the a= lines of sdp whose attribute, the text
// after a=, drop picks.
func dropAttributes(sdp string, drop func(attr string) bool) string {
	lines := strings.SplitAfter(sdp, "\n")
	out := lines[:0]
	for _, l := range lines {
		if attr, ok := strings.CutPrefix(strings.TrimRight(l, "\r\n"), "a="); ok && drop(attr) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "")
}

// tunnelCandidate builds the passive TCP candidate for the client side of the
// SSH forward, attached to the answer's first media section (the session is
// bundled, so one transport carries everything).
func (r *Relay) tunnelCandidate(sdp string) (protocol.Candidate, bool) {
	if !r.tunnelEnabled() {
		return protocol.Candidate{}, false
	}
	mid, ok := firstMid(sdp)
	if !ok {
		return protocol.Candidate{}, false
	}
	idx := uint16(0)
	return protocol.Candidate{
		Candidate:     TunnelCandidate(r.policy.TunnelPort),
		SDPMid:        &mid,
		SDPMLineIndex: &idx,
	}, true
}

// TunnelCandidate is the candidate line for a tunnel entry on port.
func TunnelCandidate(port int) string {
	return "candidate:robintunnel 1 tcp " + strconv.Itoa(TunnelPriority) + " 127.0.0.1 " + strconv.Itoa(port) + " typ host tcptype passive"
}

// transportOf returns the lowercased transport field of a candidate line, or
// "" when the line does not parse.
func transportOf(candidate string) string {
	f := strings.Fields(candidate)
	if len(f) < 8 || !strings.HasPrefix(f[0], "candidate:") {
		return ""
	}
	return strings.ToLower(f[2])
}

func firstMid(sdp string) (string, bool) {
	inMedia := false
	for _, l := range strings.Split(sdp, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "m=") {
			if inMedia {
				return "", false
			}
			inMedia = true
			continue
		}
		if mid, ok := strings.CutPrefix(l, "a=mid:"); ok && inMedia {
			return mid, true
		}
	}
	return "", false
}

// Fingerprint extracts the DTLS fingerprint from an SDP. Every a=fingerprint
// line must agree; zero lines is an error, since an answer without one could
// not be authenticated at all.
func Fingerprint(sdp string) (string, error) {
	var fp string
	for _, l := range strings.Split(sdp, "\n") {
		v, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), "a=fingerprint:")
		if !ok {
			continue
		}
		algo, hash, ok := strings.Cut(strings.TrimSpace(v), " ")
		if !ok || hash == "" {
			return "", fmt.Errorf("relay: malformed fingerprint line %q", l)
		}
		norm := strings.ToLower(algo) + " " + strings.ToUpper(strings.TrimSpace(hash))
		if fp != "" && fp != norm {
			return "", errors.New("relay: SDP carries conflicting DTLS fingerprints")
		}
		fp = norm
	}
	if fp == "" {
		return "", errors.New("relay: SDP has no DTLS fingerprint")
	}
	return fp, nil
}
