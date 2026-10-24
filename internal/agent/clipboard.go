//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Plain text up to clipboardSize travels as a control message; anything
// else on the clipboard, HTML and images included, as a bulk transfer.
func init() {
	protocol.RegisterBulkKind(protocol.BulkClipboard, protocol.ClipBulkLimit)
	handleBulk(protocol.BulkClipboard, (*session).acceptClip)
	handleControl(protocol.ControlClipboard, (*session).applyClipboard)
}

func (s *session) applyClipboard(m protocol.Control) {
	if len(m.Text) > clipboardSize {
		s.log.Warn("clipboard from browser too large", "bytes", len(m.Text))
		return
	}
	if err := s.setClip(protocol.Clip{Text: m.Text}); err != nil {
		s.log.Warn("set clipboard", "err", err)
	}
}

// setClip puts c on the desktop's clipboard before it returns, on the
// read loop of the channel c came on, so that a paste key the page sends
// after it finds c there. It costs the desktop one round trip, bounded on
// X11 by the wait for a timestamp.
func (s *session) setClip(c protocol.Clip) error {
	if rich, ok := s.desk.(desktop.RichClipboard); ok {
		return rich.SetClipboardContent(c)
	}
	if c.Text == "" {
		return errors.New("this desktop's clipboard takes only text")
	}
	return s.desk.SetClipboard(c.Text)
}

// acceptClip takes clipboard content the browser sends as a bulk
// transfer. It is at most protocol.ClipBulkLimit bytes, kept in memory as
// it arrives rather than reserved for what the header promises.
func (s *session) acceptClip(protocol.Bulk) (peer.BulkSink, error) {
	return &clipSink{s: s, buf: new(bytes.Buffer)}, nil
}

type clipSink struct {
	s   *session
	buf *bytes.Buffer
}

func (c *clipSink) Write(p []byte) (int, error) { return c.buf.Write(p) }

func (c *clipSink) Commit() error {
	clip, err := protocol.DecodeClip(c.buf.Bytes())
	if err != nil {
		return err
	}
	return c.s.setClip(clip)
}

func (c *clipSink) Abort(error) {}

// forwardClipboard sends the browser what programs on the desktop copy.
func (s *session) forwardClipboard(ctx context.Context) {
	if rich, ok := s.desk.(desktop.RichClipboard); ok {
		s.forwardClips(ctx, rich.ClipboardContent())
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case text, ok := <-s.desk.Clipboard():
			if !ok {
				return
			}
			s.sendClip(ctx, protocol.Clip{Text: text})
		}
	}
}

// forwardClips is forwardClipboard for a desktop with a rich clipboard. A
// newer copy cancels the transfer of an older one still under way.
func (s *session) forwardClips(ctx context.Context, clips <-chan protocol.Clip) {
	var wg sync.WaitGroup
	stop := func() {}
	defer func() {
		stop()
		wg.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-clips:
			if !ok {
				return
			}
			stop()
			tctx, cancel := context.WithCancel(ctx)
			stop = cancel
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.sendClip(tctx, c)
			}()
		}
	}
}

// sendClip sends c to the current page: as a control message when it is
// plain text that fits one, else as a bulk transfer.
func (s *session) sendClip(ctx context.Context, c protocol.Clip) {
	p := s.currentOrNil()
	if p == nil {
		return
	}
	if c.HTML == "" && len(c.PNG) == 0 && len(c.Text) <= clipboardSize {
		if err := p.SendControl(protocol.Control{T: protocol.ControlClipboard, Text: c.Text}); err != nil && !errors.Is(err, peer.ErrNoControl) {
			s.log.Warn("send clipboard", "err", err)
		}
		return
	}
	if err := sendClipBulk(ctx, p, c); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Warn("send clipboard", "bytes", c.Size(), "err", err)
	}
}

func sendClipBulk(ctx context.Context, p *peer.Peer, c protocol.Clip) error {
	payload, err := protocol.EncodeClip(c)
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return fmt.Errorf("nothing on the clipboard to send")
	}
	return p.SendBulk(ctx, protocol.Bulk{Kind: protocol.BulkClipboard, Size: int64(len(payload))}, bytes.NewReader(payload))
}
