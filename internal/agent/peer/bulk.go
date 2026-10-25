package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// closeGrace is how long a failed send waits for the channel to report its
// close before giving the send error as the reason.
const closeGrace = 5 * time.Second

// maxBulkReason bounds the reason a receiver sends back, so the verdict
// frame stays within protocol.BulkChunkSize after JSON escaping, and the
// reason the agent takes from the browser's refusals, which reaches
// robin-send's terminal and the log.
const maxBulkReason = 1 << 10

// ErrBulkCancelled means the other side closed a bulk channel before the
// transfer finished.
var ErrBulkCancelled = errors.New("peer: bulk transfer cancelled")

// BulkRefusedError is what SendBulk returns when the browser refused a
// transfer or could not keep it. Reason is the browser's own account.
type BulkRefusedError struct{ Reason string }

func (e *BulkRefusedError) Error() string { return "peer: bulk transfer refused: " + e.Reason }

// BulkSink takes the payload of one transfer from the browser. Write gets
// the chunks in order. Exactly one of Commit and Abort follows: Commit once
// every byte has arrived and the SHA-256 matched, its error refusing the
// transfer after all, or Abort with the reason the transfer failed or was
// cancelled. A Write error fails the transfer, and Abort follows it.
//
// Write, Commit and Abort all run on the channel's read loop, and any of
// them that blocks holds the browser back on every channel at once, control
// included, since SCTP has one receive window for them all. A sink that can
// stall for long must buffer or fail instead.
type BulkSink interface {
	io.Writer
	Commit() error
	Abort(err error)
}

// SendBulk sends r as one transfer described by h, on a data channel of its
// own, and returns once the browser has kept it. h.T is set for the caller;
// h.Size must be exactly what r holds. Cancelling ctx cancels the transfer.
// It returns a *BulkRefusedError when the browser refuses, and
// ErrBulkCancelled when the browser closes the channel first.
//
// The transfer goes no faster than the browser's credit allows, and,
// since GCC sees none of it, under the congestion target: transfers at
// once share what the video leaves of the target, and never less than a
// quarter of it between them.
func (p *Peer) SendBulk(ctx context.Context, h protocol.Bulk, r io.Reader) error {
	h.T = protocol.BulkHeader
	if err := h.CheckHeader(); err != nil {
		return err
	}
	dc, err := p.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		return fmt.Errorf("peer: open bulk channel: %w", err)
	}
	s := newBulkSender(dc)
	if !p.unpaced {
		s.pace = p.bwe
	}
	return s.send(ctx, h, r)
}

// bulkPace is what paces a bulk sender: how much to send at once, and how
// long to wait before sending it.
type bulkPace interface {
	bulkChunk() int
	reserveBulk(n int) time.Duration
}

// bulkSender is the sending end of one transfer. Its channels carry what
// Pion's callbacks report.
type bulkSender struct {
	dc      *webrtc.DataChannel
	pace    bulkPace // nil sends as fast as the credit allows
	opened  chan struct{}
	closed  chan struct{}
	verdict chan protocol.Bulk
	// credit is how many payload bytes the receiver lets the sender have
	// sent; more ticks when it grows.
	credit atomic.Int64
	more   chan struct{}
}

// newBulkSender takes over dc, which nothing else may use. It must be called
// as soon as dc is created: Pion drops messages that arrive before there is
// a handler, and the receiver may refuse at once.
func newBulkSender(dc *webrtc.DataChannel) *bulkSender {
	s := &bulkSender{
		dc: dc, opened: make(chan struct{}), closed: make(chan struct{}),
		verdict: make(chan protocol.Bulk, 1), more: make(chan struct{}, 1),
	}
	s.credit.Store(protocol.BulkWindow)
	var opened, closed sync.Once
	dc.OnOpen(func() { opened.Do(func() { close(s.opened) }) })
	dc.OnClose(func() { closed.Do(func() { close(s.closed) }) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		v, err := protocol.ParseBulk(m.Data)
		if m.IsString && err == nil && v.T == protocol.BulkCredit {
			if v.Credit > s.credit.Load() {
				s.credit.Store(v.Credit)
				select {
				case s.more <- struct{}{}:
				default:
				}
			}
			return
		}
		if !m.IsString || err != nil || (v.T != protocol.BulkDone && v.T != protocol.BulkError) {
			v = protocol.Bulk{T: protocol.BulkError, Error: "malformed verdict"}
		}
		select {
		case s.verdict <- v:
		default: // only the first verdict counts
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
	var sent int64
	for {
		size := protocol.BulkChunkSize
		if s.pace != nil {
			size = s.pace.bulkChunk()
		}
		n, err := io.ReadFull(src, buf[:size])
		if n > 0 {
			if err := sum.Add(buf[:n]); err != nil {
				return err
			}
			// The credit bounds what waits in the channel's queue too,
			// so cancelling drops at most a window.
			for sent+int64(n) > s.credit.Load() {
				if err := s.wait(ctx, s.more); err != nil {
					return err
				}
			}
			if s.pace != nil {
				if err := s.sleep(ctx, s.pace.reserveBulk(n)); err != nil {
					return err
				}
			}
			if err := s.check(ctx); err != nil {
				return err
			}
			if err := dc.Send(buf[:n]); err != nil {
				return s.sendErr(ctx, err)
			}
			sent += int64(n)
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

// sleep waits d, or returns why the transfer cannot go on.
func (s *bulkSender) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return s.check(ctx)
	}
	due := make(chan struct{})
	t := time.AfterFunc(d, func() { close(due) })
	defer t.Stop()
	return s.wait(ctx, due)
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
		return &BulkRefusedError{Reason: "receiver finished before the end"}
	}
	return &BulkRefusedError{Reason: bounded(v.Error)}
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

	mu    sync.Mutex
	sink  BulkSink
	sum   *protocol.BulkSum
	grant protocol.BulkGrant
	over  bool // a verdict went out or the channel closed
	// While the header is awaited, late refuses the transfer and settled
	// is called once the first frame or the close comes.
	late    *time.Timer
	settled func()
}

// receiveBulk takes one transfer the remote side opened on dc. accept
// decides on its header; nil refuses everything.
func receiveBulk(dc *webrtc.DataChannel, accept func(protocol.Bulk) (BulkSink, error), log *slog.Logger) *bulkReceiver {
	r := &bulkReceiver{dc: dc, accept: accept, log: log}
	dc.OnMessage(r.message)
	dc.OnClose(r.closed)
	return r
}

// awaitHeader refuses the transfer unless its first frame comes within
// wait, and calls settled once that frame or the close has come. It must
// be called from the handler that took the channel, before Pion delivers
// anything on it.
func (r *bulkReceiver) awaitHeader(wait time.Duration, settled func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settled = settled
	r.late = time.AfterFunc(wait, func() { r.expire(wait) })
}

// expire refuses a transfer that has sent nothing within wait.
func (r *bulkReceiver) expire(wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sink == nil && !r.over {
		r.fail(fmt.Errorf("peer: no header within %s", wait))
	}
}

// settle ends the wait for the header.
func (r *bulkReceiver) settle() {
	if r.late != nil {
		r.late.Stop()
	}
	if r.settled != nil {
		r.settled()
		r.settled = nil
	}
}

func (r *bulkReceiver) message(m webrtc.DataChannelMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settle()
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
			return
		}
		if credit, ok := r.grant.Take(len(m.Data)); ok {
			sendFrame(r.dc, credit)
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

// fail sends the verdict before it aborts the sink, so the sender learns
// it was refused without waiting out the sink's cleanup.
func (r *bulkReceiver) fail(err error) {
	r.log.Info("bulk transfer failed", "err", err)
	r.reply(protocol.Bulk{T: protocol.BulkError, Error: reason(err)})
	if r.sink != nil {
		r.sink.Abort(err)
	}
}

func (r *bulkReceiver) reply(v protocol.Bulk) {
	r.over = true
	sendVerdict(r.dc, v)
}

// sendVerdict sends v and closes dc, which ends the transfer. A send fails
// only once the channel is closing, and the sender learns from the close
// instead.
func sendVerdict(dc *webrtc.DataChannel, v protocol.Bulk) {
	sendFrame(dc, v)
	dc.Close()
}

// sendFrame sends f on dc. It fails only once the channel is closing.
func sendFrame(dc *webrtc.DataChannel, f protocol.Bulk) {
	if b, err := json.Marshal(f); err == nil {
		_ = dc.SendText(string(b))
	}
}

// refuseBulk turns down the transfer on dc, which the remote side opened,
// as soon as the channel is open.
func refuseBulk(dc *webrtc.DataChannel, err error) {
	dc.OnOpen(func() { sendVerdict(dc, protocol.Bulk{T: protocol.BulkError, Error: reason(err)}) })
}

func (r *bulkReceiver) closed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settle()
	if r.over {
		return
	}
	r.over = true
	if r.sink != nil {
		r.sink.Abort(ErrBulkCancelled)
	}
}

func reason(err error) string { return bounded(err.Error()) }

func bounded(s string) string {
	if len(s) > maxBulkReason {
		s = s[:maxBulkReason]
	}
	return s
}
