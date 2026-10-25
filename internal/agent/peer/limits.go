package peer

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// maxRemoteCandidates bounds the distinct candidates the browser may name
// in one ICE generation, in its offer and trickled after it. A browser
// gathers one per address, transport and server: a few, or a few dozen on a
// machine with container bridges and IPv6 addresses (the test machine's
// stand-in browser gathers about 20). Each the agent takes has it check a
// pair against every candidate of its own for as long as the peer lives.
const maxRemoteCandidates = 256

// maxWaitingBulk bounds the bulk channels the browser has opened and sent no
// header on yet. The page sends a header as soon as a channel opens and
// runs at most four uploads and two clipboard transfers at once; past the
// header, those features' own limits hold.
const maxWaitingBulk = 8

// bulkHeaderWait bounds how long a bulk channel may stay open without a
// header, the page's first frame, which it sends the moment the channel
// opens; it is long only for a link that stalls.
const bulkHeaderWait = 10 * time.Second

// errBulkWaiting refuses a bulk channel while maxWaitingBulk others wait
// for their header.
var errBulkWaiting = fmt.Errorf("peer: %d transfers are already waiting for their header", maxWaitingBulk)

// takeBulk takes a bulk channel the browser opened, counting it as waiting
// until its first frame, and refuses it when its header is late or too
// many others wait.
func (p *Peer) takeBulk(dc *webrtc.DataChannel) {
	if p.waitingBulk.Add(1) > maxWaitingBulk {
		p.waitingBulk.Add(-1)
		refuseBulk(dc, errBulkWaiting)
		return
	}
	receiveBulk(dc, p.h.Bulk, p.log).awaitHeader(p.headerWait, func() { p.waitingBulk.Add(-1) })
}

// takeMedia picks the lines of an applied offer the agent takes media on:
// the first audio line when Config.Mic asks for the microphone, the first
// video line when Config.Camera asks for the camera. It stops every other
// line the page added to send on, so the answer marks it inactive and the
// page sends nothing there. The agent's own lines, which carry its video
// and sound, cannot be stopped without their sending side; media the page
// sends on one is read and dropped.
func (p *Peer) takeMedia() {
	var mic, camera *webrtc.RTPReceiver
	for _, t := range p.pc.GetTransceivers() {
		if d := t.Direction(); d != webrtc.RTPTransceiverDirectionRecvonly && d != webrtc.RTPTransceiverDirectionSendrecv {
			continue
		}
		switch {
		case t.Kind() == webrtc.RTPCodecTypeAudio && p.mic && mic == nil:
			mic = t.Receiver()
		case t.Kind() == webrtc.RTPCodecTypeVideo && p.camera && camera == nil:
			camera = t.Receiver()
		case t.Sender() == nil:
			if err := t.Stop(); err != nil {
				p.log.Warn("stop a media line the agent does not take", "err", err)
			}
		}
	}
	p.mu.Lock()
	p.taken = [2]*webrtc.RTPReceiver{mic, camera}
	p.mu.Unlock()
}

// wanted reports whether r is a line takeMedia took.
func (p *Peer) wanted(r *webrtc.RTPReceiver) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return r != nil && (r == p.taken[0] || r == p.taken[1])
}

// drop reads tr until its receiver stops, so what the page sends on a line
// the agent cannot refuse does not pile up unread.
func drop(tr *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := tr.Read(buf); err != nil {
			return
		}
	}
}

// ErrTooManyCandidates means the browser named more than
// maxRemoteCandidates candidates since its last ICE restart; the candidate
// was dropped.
var ErrTooManyCandidates = errors.New("peer: too many remote candidates")

// remoteCandidates counts the candidates of the browser's current ICE
// generation, which starts with each offer under a new ufrag: Pion forgets
// the previous generation's candidates then too.
type remoteCandidates struct {
	mu     sync.Mutex
	ufrag  string
	seen   map[string]bool
	warned bool
}

// offer checks offerSDP's ICE credentials and counts the candidates it
// names. It returns the offer to apply, without the candidates the agent
// never checks (see outOfReach), and commit, which records the count once
// the offer has applied, so an offer Pion refuses changes nothing.
//
// Pion takes a bundle's credentials from one section and starts a new
// generation only when that section's ufrag changes; it checks no other
// section against it. An offer whose sections disagree is refused, so the
// ufrag counted is the one Pion holds.
func (rc *remoteCandidates) offer(offerSDP string) (sdp string, commit func(), err error) {
	var ufrag, pwd string
	named := map[string]bool{}
	var kept strings.Builder
	for line := range strings.Lines(offerSDP) {
		text := strings.TrimRight(line, "\r\n")
		if v, ok := strings.CutPrefix(text, "a=ice-ufrag:"); ok {
			if err := sameCredential(&ufrag, v, minUfrag); err != nil {
				return "", nil, fmt.Errorf("peer: the offer's ice-ufrag %w", err)
			}
		}
		if v, ok := strings.CutPrefix(text, "a=ice-pwd:"); ok {
			if err := sameCredential(&pwd, v, minPwd); err != nil {
				return "", nil, fmt.Errorf("peer: the offer's ice-pwd %w", err)
			}
		}
		if c, ok := strings.CutPrefix(text, "a="); ok && strings.HasPrefix(c, "candidate:") {
			named[c] = true
			if outOfReach(c) {
				continue
			}
		}
		kept.WriteString(line)
	}
	if ufrag == "" || pwd == "" {
		return "", nil, errors.New("peer: the offer names no ICE credentials")
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	restart := ufrag != rc.ufrag
	n := len(named)
	if !restart {
		for c := range named {
			if rc.seen[c] {
				n--
			}
		}
		n += len(rc.seen)
	}
	if n > maxRemoteCandidates {
		return "", nil, fmt.Errorf("peer: the offer brings the browser's candidates to %d, more than %d", n, maxRemoteCandidates)
	}
	return kept.String(), func() {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		// A ufrag is never empty, so the first offer always restarts.
		if restart {
			rc.ufrag, rc.seen, rc.warned = ufrag, map[string]bool{}, false
		}
		for c := range named {
			rc.seen[c] = true
		}
	}, nil
}

// RFC 8839's bounds on ice-ufrag and ice-pwd, in ice-char.
const (
	minUfrag, minPwd = 4, 22
	maxCredential    = 256
)

// sameCredential checks v, one ice-ufrag or ice-pwd line of an offer:
// min to maxCredential of RFC 8839's ice-char, and the same as *seen, what
// earlier lines gave, if any. Every check the agent sends carries the
// browser's ufrag, so nothing else goes there.
func sameCredential(seen *string, v string, min int) error {
	odd := func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '+' || r == '/')
	}
	switch {
	case len(v) < min || len(v) > maxCredential || strings.ContainsFunc(v, odd):
		return fmt.Errorf("is not %d to %d letters, digits, + and /", min, maxCredential)
	case *seen != "" && v != *seen:
		return errors.New("differs between sections")
	}
	*seen = v
	return nil
}

// outOfReach reports whether candidate c names an address the browser is
// never at: the host's own loopback, a link-local address, which the
// agent's checks would reach only on its own link, or no unicast address
// at all. The browser on another machine cannot be there, and what listens
// on the host's loopback trusts it; the tunnel comes in peer-reflexive, not
// named. A name, which Pion does not resolve, is left to it.
func outOfReach(c string) bool {
	f := strings.Fields(c)
	if len(f) < 5 {
		return false
	}
	ip, err := netip.ParseAddr(f[4])
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified()
}

// add counts one trickled candidate, or refuses it once the generation has
// its fill. warn reports the first refusal.
func (rc *remoteCandidates) add(c string) (warn bool, err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.seen == nil {
		rc.seen = map[string]bool{}
	}
	if rc.seen[c] {
		return false, nil
	}
	if len(rc.seen) >= maxRemoteCandidates {
		warn, rc.warned = !rc.warned, true
		return warn, ErrTooManyCandidates
	}
	rc.seen[c] = true
	return false, nil
}
