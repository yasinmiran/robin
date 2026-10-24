package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yasinmiran/robin/internal/protocol"
)

const writeTimeout = 10 * time.Second

// Browser is one authenticated page connected over the websocket.
type Browser struct {
	c *websocket.Conn
	// ctx is the websocket request's context, which derives from Serve's.
	// The handler that made the Browser returns only after Done, so ctx
	// lasts as long as the connection, or until Serve's ctx ends; reads and
	// writes stop then.
	ctx     context.Context
	in      chan protocol.Message
	done    chan struct{}
	once    sync.Once
	err     error
	release func()
}

func newBrowser(ctx context.Context, c *websocket.Conn, release func()) *Browser {
	c.SetReadLimit(protocol.MaxLineBytes)
	b := &Browser{
		c:       c,
		ctx:     ctx,
		in:      make(chan protocol.Message),
		done:    make(chan struct{}),
		release: release,
	}
	go b.readLoop()
	return b
}

// Send writes one message as a JSON text frame. It is safe for concurrent use.
func (b *Browser) Send(m protocol.Message) error {
	p, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(b.ctx, writeTimeout)
	defer cancel()
	return b.c.Write(ctx, websocket.MessageText, p)
}

// Recv returns the next message from the page. The page may only send
// offers, candidates, detach and bye; anything else closes the connection.
func (b *Browser) Recv(ctx context.Context) (protocol.Message, error) {
	select {
	case m := <-b.in:
		return m, nil
	case <-b.done:
		return protocol.Message{}, b.err
	case <-ctx.Done():
		return protocol.Message{}, ctx.Err()
	}
}

// Done is closed once the websocket has closed.
func (b *Browser) Done() <-chan struct{} { return b.done }

// Close closes the websocket. After it returns, another browser may connect.
func (b *Browser) Close() error {
	b.finish(websocket.StatusNormalClosure, errors.New("web: browser closed"))
	return nil
}

func (b *Browser) finish(code websocket.StatusCode, err error) {
	b.once.Do(func() {
		b.err = err
		close(b.done)
		b.release()
		b.c.Close(code, "")
	})
}

func (b *Browser) readLoop() {
	for {
		typ, p, err := b.c.Read(b.ctx)
		if err != nil {
			b.finish(websocket.StatusGoingAway, fmt.Errorf("web: browser: %w", err))
			return
		}
		m, err := decode(typ, p)
		if err != nil {
			b.finish(websocket.StatusPolicyViolation, err)
			return
		}
		select {
		case b.in <- m:
		case <-b.done:
			return
		}
	}
}

func decode(typ websocket.MessageType, p []byte) (protocol.Message, error) {
	if typ != websocket.MessageText {
		return protocol.Message{}, errors.New("web: browser sent a binary frame")
	}
	var m protocol.Message
	if err := json.Unmarshal(p, &m); err != nil {
		return protocol.Message{}, fmt.Errorf("web: browser sent malformed JSON: %w", err)
	}
	switch m.Type {
	case protocol.TypeOffer, protocol.TypeCandidate, protocol.TypeDetach, protocol.TypeBye:
		return m, nil
	}
	return protocol.Message{}, fmt.Errorf("web: browser may not send %q", m.Type)
}
