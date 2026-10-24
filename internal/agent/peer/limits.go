package peer

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
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
// opens; it is long only for a link that stalls. A var so tests need not
// wait it out.
var bulkHeaderWait = 10 * time.Second

// errBulkWaiting refuses a bulk channel while maxWaitingBulk others wait
// for their header.
var errBulkWaiting = fmt.Errorf("peer: %d transfers are already waiting for their header", maxWaitingBulk)

// takeBulk hands a bulk channel the browser opened to receiveBulk's
// receiver, counting it as waiting until its first frame, and closes it
// with a refusal when its header is late or too many others wait.
func (p *Peer) takeBulk(dc *webrtc.DataChannel) {
	if p.waitingBulk.Add(1) > maxWaitingBulk {
		p.waitingBulk.Add(-1)
		dc.OnOpen(func() {
			(&bulkReceiver{dc: dc}).reply(protocol.Bulk{T: protocol.BulkError, Error: reason(errBulkWaiting)})
		})
		return
	}
	r := &bulkReceiver{dc: dc, accept: p.h.Bulk, log: p.log}
	var once sync.Once
	settled := func() { once.Do(func() { p.waitingBulk.Add(-1) }) }
	wait := bulkHeaderWait
	late := time.AfterFunc(wait, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sink == nil && !r.over {
			r.fail(fmt.Errorf("peer: no header within %s", wait))
		}
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		late.Stop()
		settled()
		r.message(m)
	})
	dc.OnClose(func() {
		late.Stop()
		settled()
		r.closed()
	})
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

// offer checks the candidates offerSDP names. The count it returns applies
// them once the offer has been applied, so an offer Pion refuses changes
// nothing.
func (rc *remoteCandidates) offer(offerSDP string) (apply func(), err error) {
	ufrag, named := "", map[string]bool{}
	for line := range strings.Lines(offerSDP) {
		line = strings.TrimRight(line, "\r\n")
		if v, ok := strings.CutPrefix(line, "a=ice-ufrag:"); ok && ufrag == "" {
			ufrag = v
		}
		if v, ok := strings.CutPrefix(line, "a="); ok && strings.HasPrefix(v, "candidate:") {
			named[v] = true
		}
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
		return nil, fmt.Errorf("peer: the offer brings the browser's candidates to %d, more than %d", n, maxRemoteCandidates)
	}
	return func() {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if restart {
			rc.ufrag, rc.seen, rc.warned = ufrag, map[string]bool{}, false
		}
		for c := range named {
			rc.seen[c] = true
		}
	}, nil
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
