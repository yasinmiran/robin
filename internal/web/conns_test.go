package web

import (
	"net"
	"net/http"
	"testing"
)

// fakeConn is a connection that only records being closed.
type fakeConn struct {
	net.Conn
	closed bool
}

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

// TestConnLimitClosesTheOldestWaiting: past the limit the oldest connection
// waiting for a request goes, then the oldest inside one, and never the one
// that just arrived or one handed to a websocket.
func TestConnLimitClosesTheOldestWaiting(t *testing.T) {
	l := newConnLimit(3)
	ws, busy1, busy2, idle := &fakeConn{}, &fakeConn{}, &fakeConn{}, &fakeConn{}
	l.track(ws, http.StateNew)
	l.track(ws, http.StateActive)
	l.track(ws, http.StateHijacked)
	l.track(busy1, http.StateNew)
	l.track(busy1, http.StateActive)
	l.track(busy2, http.StateNew)
	l.track(busy2, http.StateActive)
	l.track(idle, http.StateNew)
	l.track(idle, http.StateActive)
	l.track(idle, http.StateIdle)

	fresh := &fakeConn{}
	l.track(fresh, http.StateNew)
	if !idle.closed || busy1.closed || busy2.closed || fresh.closed || ws.closed {
		t.Fatal("the idle connection should have gone first, and only it")
	}
	// Reported after it was closed, as the server does once it notices.
	l.track(idle, http.StateClosed)
	l.track(idle, http.StateIdle)

	l.track(fresh, http.StateActive)
	next := &fakeConn{}
	l.track(next, http.StateNew)
	if !busy1.closed || busy2.closed || fresh.closed || next.closed || ws.closed {
		t.Fatal("with nothing waiting, the oldest busy connection should have gone, and only it")
	}
	if l.Open() != 3 || l.Peak() != 3 {
		t.Fatalf("open %d, peak %d; want 3 and 3", l.Open(), l.Peak())
	}
}
