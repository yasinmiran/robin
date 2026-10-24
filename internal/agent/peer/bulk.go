package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// A bulk sender keeps between bulkLowWater and bulkHighWater bytes queued
// on its channel: enough to keep a fast path busy between wakeups, little
// enough that cancelling drops at most a megabyte.
const (
	bulkHighWater = 1 << 20
	bulkLowWater  = 256 << 10
)

// closeGrace is how long a failed send waits for the channel to report its
// close before giving the send error as the reason.
const closeGrace = 5 * time.Second

// maxBulkReason bounds the reason a receiver sends back, so the verdict
// frame stays within protocol.BulkChunkSize after JSON escaping.
const maxBulkReason = 1 << 10

// ErrBulkCancelled means the other side closed a bulk channel before the
// transfer finished.
var ErrBulkCancelled = errors.New("peer: bulk transfer cancelled")

// BulkRefused is what SendBulk returns when the browser refused a transfer
// or could not keep it. Reason is the browser's own account.
type BulkRefused struct{ Reason string }

func (e *BulkRefused) Error() string { return "peer: bulk transfer refused: " + e.Reason }

// BulkSink takes the payload of one transfer from the browser. Write gets
// the chunks in order; it may block, which holds the browser back. Exactly
// one of Commit and Abort follows: Commit once every byte has arrived and
// the SHA-256 matched, its error refusing the transfer after all, or Abort
// with the reason the transfer failed or was cancelled. A Write error fails
// the transfer, and Abort follows it.
type BulkSink interface {
	io.Writer
	Commit() error
	Abort(err error)
}

// SendBulk sends r as one transfer described by h, on a data channel of its
// own, and returns once the browser has kept it. h.T is set for the caller;
// h.Size must be exactly what r holds. Cancelling ctx cancels the transfer.
// It returns a *BulkRefused when the browser refuses, and ErrBulkCancelled
// when the browser closes the channel first.
func (p *Peer) SendBulk(ctx context.Context, h protocol.Bulk, r io.Reader) error {
	h.T = protocol.BulkHeader
	if err := h.CheckHeader(); err != nil {
		return err
	}
	ordered := true
	dc, err := p.pc.CreateDataChannel(protocol.BulkChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return fmt.Errorf("peer: open bulk channel: %w", err)
	}
	return newBulkSender(dc).send(ctx, h, r)
}

// bulkSender is the sending end of one transfer. Its channels carry what
// Pion's callbacks report.
type bulkSender struct {
	dc      *webrtc.DataChannel
	opened  chan struct{}
	closed  chan struct{}
	low     chan struct{}
	verdict chan protocol.Bulk
}

// newBulkSender takes over dc, which nothing else may use. It must be called
// as soon as dc is created: Pion drops messages that arrive before there is
// a handler, and the receiver may refuse at once.
func newBulkSender(dc *webrtc.DataChannel) *bulkSender {
	s := &bulkSender{
		dc: dc, opened: make(chan struct{}), closed: make(chan struct{}),
		low: make(chan struct{}, 1), verdict: make(chan protocol.Bulk, 1),
	}
	var opened, closed sync.Once
	dc.OnOpen(func() { opened.Do(func() { close(s.opened) }) })
	dc.OnClose(func() { closed.Do(func() { close(s.closed) }) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		v, err := protocol.ParseBulk(m.Data)
		if !m.IsString || err != nil || (v.T != protocol.BulkDone && v.T != protocol.BulkError) {
			v = protocol.Bulk{T: protocol.BulkError, Error: "malformed verdict"}
		}
		select {
		case s.verdict <- v:
		default: // only the first verdict counts
		}
	})
	dc.SetBufferedAmountLowThreshold(bulkLowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case s.low <- struct{}{}:
		default:
		}
	})
	return s
}

// send runs one transfer and closes the channel.
func (s *bulkSender) send(ctx context.Context, h protocol.Bulk, r io.Reader) error {
	dc := s.dc
	defer dc.Close()
	if err := s.wait(ctx, s.opened); err != nil {
		return err
	}
	if err := s.sendText(ctx, h); err != nil {
		return err
	}
	sum := protocol.NewBulkSum(h.Size)
	// One byte past the declared size is enough to tell r holds too much.
	src := io.LimitReader(r, h.Size+1)
	buf := make([]byte, protocol.BulkChunkSize)
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			if err := sum.Add(buf[:n]); err != nil {
				return err
			}
			for dc.BufferedAmount() > bulkHighWater {
				if err := s.wait(ctx, s.low); err != nil {
					return err
				}
			}
			if err := s.check(ctx); err != nil {
				return err
			}
			if err := dc.Send(buf[:n]); err != nil {
				return s.sendErr(ctx, err)
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("peer: read bulk payload: %w", err)
		}
	}
	end, err := sum.End()
	if err != nil {
		return err
	}
	if err := s.sendText(ctx, end); err != nil {
		return err
	}
	select {
	case v := <-s.verdict:
		return outcome(v)
	case <-s.closed:
		return s.afterClose()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait blocks until ready fires, or returns why the transfer cannot go on.
// Before the end frame any verdict is a refusal, a done included.
func (s *bulkSender) wait(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
		return nil
	case v := <-s.verdict:
		return refusal(v)
	case <-s.closed:
		return s.afterClose()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// check returns why the transfer cannot go on, without waiting.
func (s *bulkSender) check(ctx context.Context) error {
	select {
	case v := <-s.verdict:
		return refusal(v)
	case <-s.closed:
		return s.afterClose()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *bulkSender) sendText(ctx context.Context, b protocol.Bulk) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := s.dc.SendText(string(data)); err != nil {
		return s.sendErr(ctx, err)
	}
	return nil
}

// sendErr explains a failed send, which usually means the receiver refused
// or left. Pion fails sends as soon as the receiver resets the stream, which
// can be before its read loop has delivered the receiver's last frame, so
// this waits a moment for the close that follows.
func (s *bulkSender) sendErr(ctx context.Context, err error) error {
	select {
	case v := <-s.verdict:
		return refusal(v)
	case <-s.closed:
		return s.afterClose()
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(closeGrace):
		return fmt.Errorf("peer: bulk send: %w", err)
	}
}

// afterClose is the outcome once the channel has closed. Pion delivers a
// channel's messages before it reports the close, so a verdict sent just
// before closing is already waiting.
func (s *bulkSender) afterClose() error {
	select {
	case v := <-s.verdict:
		return refusal(v)
	default:
		return ErrBulkCancelled
	}
}

func refusal(v protocol.Bulk) error {
	if v.T == protocol.BulkDone {
		return &BulkRefused{Reason: "receiver finished before the end"}
	}
	return &BulkRefused{Reason: v.Error}
}

func outcome(v protocol.Bulk) error {
	if v.T == protocol.BulkDone {
		return nil
	}
	return refusal(v)
}

// bulkReceiver is the receiving end of one transfer. Pion calls message
// from the channel's read loop and closed after it, never at once; mu makes
// that plain to the race detector.
type bulkReceiver struct {
	dc     *webrtc.DataChannel
	accept func(protocol.Bulk) (BulkSink, error)
	log    *slog.Logger

	mu   sync.Mutex
	sink BulkSink
	sum  *protocol.BulkSum
	over bool // a verdict went out or the channel closed
}

// receiveBulk takes one transfer the remote side opened on dc. accept
// decides on its header; nil refuses everything.
func receiveBulk(dc *webrtc.DataChannel, accept func(protocol.Bulk) (BulkSink, error), log *slog.Logger) {
	r := &bulkReceiver{dc: dc, accept: accept, log: log}
	dc.OnMessage(r.message)
	dc.OnClose(r.closed)
}

func (r *bulkReceiver) message(m webrtc.DataChannelMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.over {
		return
	}
	switch {
	case r.sink == nil:
		if err := r.open(m); err != nil {
			r.fail(err)
		}
	case !m.IsString:
		if err := r.sum.Add(m.Data); err != nil {
			r.fail(err)
			return
		}
		if _, err := r.sink.Write(m.Data); err != nil {
			r.fail(err)
		}
	default:
		end, err := protocol.ParseBulk(m.Data)
		if err == nil {
			err = r.sum.Check(end)
		}
		if err != nil {
			r.fail(err)
			return
		}
		if err := r.sink.Commit(); err != nil {
			r.reply(protocol.Bulk{T: protocol.BulkError, Error: reason(err)})
			return
		}
		r.reply(protocol.Bulk{T: protocol.BulkDone})
	}
}

// open reads the header and asks accept for a sink.
func (r *bulkReceiver) open(m webrtc.DataChannelMessage) error {
	if !m.IsString {
		return fmt.Errorf("%w: data before the header", protocol.ErrBulkFrame)
	}
	h, err := protocol.ParseBulk(m.Data)
	if err != nil {
		return err
	}
	if err := h.CheckHeader(); err != nil {
		return err
	}
	if r.accept == nil {
		return fmt.Errorf("%w %q: nothing here takes bulk transfers", protocol.ErrBulkKind, h.Kind)
	}
	sink, err := r.accept(h)
	if err != nil {
		return err
	}
	r.sink, r.sum = sink, protocol.NewBulkSum(h.Size)
	return nil
}

// fail aborts the transfer and tells the sender why.
func (r *bulkReceiver) fail(err error) {
	r.log.Info("bulk transfer failed", "err", err)
	if r.sink != nil {
		r.sink.Abort(err)
	}
	r.reply(protocol.Bulk{T: protocol.BulkError, Error: reason(err)})
}

// reply sends the verdict and closes the channel, which ends the transfer.
func (r *bulkReceiver) reply(v protocol.Bulk) {
	r.over = true
	if b, err := json.Marshal(v); err == nil {
		r.dc.SendText(string(b))
	}
	r.dc.Close()
}

func (r *bulkReceiver) closed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.over {
		return
	}
	r.over = true
	if r.sink != nil {
		r.sink.Abort(ErrBulkCancelled)
	}
}

func reason(err error) string {
	s := err.Error()
	if len(s) > maxBulkReason {
		s = s[:maxBulkReason]
	}
	return s
}
