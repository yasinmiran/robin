package web

import (
	"net"
	"net/http"
	"sync"
)

// connLimit holds the page server to max open connections by closing one
// whenever a new one would make more: the oldest that has sent no request
// or sits idle, else the oldest still inside a request, never the newcomer.
// Waiting for a free slot instead would queue the browser's connection
// behind whoever holds them all, its tab loading for as long as they keep
// them. A websocket leaves the count once hijacked, so it is never the one
// closed; the one-browser rule bounds those.
type connLimit struct {
	max int

	mu   sync.Mutex
	open map[net.Conn]*tracked
	seq  uint64
	peak int
}

// tracked is what connLimit knows of one connection: when it arrived, and
// its state as http.Server last reported it.
type tracked struct {
	seq   uint64
	state http.ConnState
}

func newConnLimit(max int) *connLimit {
	return &connLimit{max: max, open: make(map[net.Conn]*tracked)}
}

// track is the http.Server's ConnState hook. The server reports StateNew
// on its accept loop, before it reads from the connection.
func (l *connLimit) track(c net.Conn, state http.ConnState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch state {
	case http.StateNew:
		l.seq++
		l.open[c] = &tracked{seq: l.seq, state: state}
		if len(l.open) > l.max {
			victim := l.oldest(c)
			delete(l.open, victim)
			victim.Close()
		}
		l.peak = max(l.peak, len(l.open))
	case http.StateActive, http.StateIdle:
		if t, ok := l.open[c]; ok {
			t.state = state
		}
	case http.StateHijacked, http.StateClosed:
		delete(l.open, c)
	}
}

// oldest picks the connection to close for newcomer: the oldest of those
// waiting for a request, or failing that the oldest of the rest.
func (l *connLimit) oldest(newcomer net.Conn) net.Conn {
	var waiting, busy net.Conn
	for c, t := range l.open {
		if c == newcomer {
			continue
		}
		pick := &busy
		if t.state == http.StateNew || t.state == http.StateIdle {
			pick = &waiting
		}
		if *pick == nil || t.seq < l.open[*pick].seq {
			*pick = c
		}
	}
	if waiting != nil {
		return waiting
	}
	return busy
}

// Open is how many connections are open now.
func (l *connLimit) Open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.open)
}

// Peak is the most connections that were open at once.
func (l *connLimit) Peak() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peak
}
