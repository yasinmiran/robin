package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// MaxLineBytes bounds one JSON line. SDP is a few kilobytes; anything near
// this limit is a broken or hostile peer.
const MaxLineBytes = 1 << 20

// Heartbeat defaults for the agent's stdio. A dropped network can leave the SSH
// channel open for minutes before TCP gives up; the timeout is what makes the
// agent tear down its display promptly instead.
const (
	DefaultPingInterval = 5 * time.Second
	DefaultIdleTimeout  = 20 * time.Second
)

// ErrIdle is returned by Recv when nothing arrived within the idle timeout.
var ErrIdle = errors.New("protocol: peer silent past idle timeout")

// ErrLineTooLong is returned by Send for a message whose line would be over
// MaxLineBytes, which the peer would take for a broken stream.
var ErrLineTooLong = fmt.Errorf("protocol: message longer than a line of %d bytes", MaxLineBytes)

// Conn carries Messages as JSON lines over a byte stream. It sends a ping every
// interval and fails with ErrIdle when the peer sends nothing for timeout.
// Pings are consumed here and never returned by Recv.
type Conn struct {
	w  io.Writer
	wm sync.Mutex

	in       chan Message
	done     chan struct{}
	closeOne sync.Once
	err      atomic.Pointer[error]
	lastSeen atomic.Int64
}

// NewConn starts reading r and pinging w. A zero interval or timeout disables
// that half of the heartbeat.
func NewConn(r io.Reader, w io.Writer, interval, timeout time.Duration) *Conn {
	c := &Conn{w: w, in: make(chan Message), done: make(chan struct{})}
	c.lastSeen.Store(time.Now().UnixNano())
	go c.readLoop(r)
	if interval > 0 {
		go c.pingLoop(interval)
	}
	if timeout > 0 {
		go c.watchdog(timeout)
	}
	return c
}

// Send writes one message. It is safe for concurrent use. Send returns
// ErrLineTooLong, writing nothing, for a message whose line would be over
// MaxLineBytes; the connection stays up.
func (c *Conn) Send(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > MaxLineBytes {
		return ErrLineTooLong
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	_, err = c.w.Write(b)
	return err
}

// Recv returns the next non-ping message. After the stream ends or fails it
// returns the cause: io.EOF for a clean end, ErrIdle, or a framing error.
// When ctx ends first it returns context.Cause(ctx), and the next message
// waits for a later Recv.
func (c *Conn) Recv(ctx context.Context) (Message, error) {
	select {
	case m := <-c.in:
		return m, nil
	case <-c.done:
		return Message{}, c.Err()
	case <-ctx.Done():
		return Message{}, context.Cause(ctx)
	}
}

// Done is closed once the connection has failed or been closed.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is live.
func (c *Conn) Err() error {
	if p := c.err.Load(); p != nil {
		return *p
	}
	return nil
}

// Close stops the connection. It does not close the underlying stream.
func (c *Conn) Close() error {
	c.fail(io.ErrClosedPipe)
	return nil
}

func (c *Conn) fail(err error) {
	c.closeOne.Do(func() {
		c.err.Store(&err)
		close(c.done)
	})
}

func (c *Conn) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	for sc.Scan() {
		c.lastSeen.Store(time.Now().UnixNano())
		m, err := decodeLine(sc.Bytes())
		if err != nil {
			c.fail(err)
			return
		}
		if m.Type == TypePing {
			continue
		}
		select {
		case c.in <- m:
		case <-c.done:
			return
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	} else if errors.Is(err, bufio.ErrTooLong) {
		err = fmt.Errorf("protocol: line longer than %d bytes", MaxLineBytes)
	}
	c.fail(err)
}

// decodeLine rejects anything that is not a JSON object with a type. A stray
// line on stdout means something other than the agent is writing to the
// signaling channel, and continuing would only hide that.
func decodeLine(b []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, fmt.Errorf("protocol: malformed line %q: %w", truncate(b, 80), err)
	}
	if m.Type == "" {
		return Message{}, fmt.Errorf("protocol: line without type: %q", truncate(b, 80))
	}
	return m, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

func (c *Conn) pingLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := c.Send(Message{Type: TypePing}); err != nil {
				c.fail(err)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) watchdog(timeout time.Duration) {
	t := time.NewTicker(timeout / 4)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if time.Since(time.Unix(0, c.lastSeen.Load())) > timeout {
				c.fail(ErrIdle)
				return
			}
		case <-c.done:
			return
		}
	}
}
