package peer

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// maxRemoteCandidates bounds the distinct candidates the browser may name
// in one ICE generation, in its offer and trickled after it. A browser
// gathers one per address, transport and server: a few, or a few dozen on a
// machine with container bridges and IPv6 addresses (the test machine's
// stand-in browser gathers about 20). Each the agent takes has it check a
// pair against every candidate of its own for as long as the peer lives.
const maxRemoteCandidates = 256

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
		return nil, fmt.Errorf("peer: the offer names %d candidates, more than %d", n, maxRemoteCandidates)
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
