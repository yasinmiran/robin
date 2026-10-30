package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/protocol"
)

// testKind is the one bulk kind these tests send.
const (
	testKind  = "test"
	testLimit = 16 << 20
)

func init() { protocol.RegisterBulkKind(testKind, testLimit) }

// memSink keeps a transfer in memory and reports how it ended on result:
// nil once committed, the reason once aborted.
type memSink struct {
	h         protocol.Bulk
	buf       bytes.Buffer
	block     chan struct{} // when set, Write waits until it is closed
	slowAbort chan struct{} // when set, Abort waits until it is closed
	writeErr  error
	commitErr error
	writes    atomic.Int32 // Write calls begun
	ends      atomic.Int32
	result    chan error
}

func (m *memSink) Write(p []byte) (int, error) {
	m.writes.Add(1)
	if m.block != nil {
		<-m.block
	}
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.buf.Write(p)
}

func (m *memSink) Commit() error {
	m.ends.Add(1)
	m.result <- nil
	return m.commitErr
}

func (m *memSink) Abort(err error) {
	if m.slowAbort != nil {
		<-m.slowAbort
	}
	m.ends.Add(1)
	m.result <- err
}

// end waits for Commit or Abort and returns what result carried.
func (m *memSink) end(t *testing.T) error {
	t.Helper()
	select {
	case err := <-m.result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("transfer %q never ended", m.h.ID)
		return nil
	}
}

// onlyEnd is end for a transfer that is over: a second Commit or Abort soon
// after the first fails the test.
func (m *memSink) onlyEnd(t *testing.T) error {
	t.Helper()
	err := m.end(t)
	select {
	case again := <-m.result:
		t.Errorf("transfer %q ended twice: with %v, then %v", m.h.ID, err, again)
	case <-time.After(200 * time.Millisecond):
	}
	return err
}

// sinks hands out a memSink per header, shaped by its ID, and reports each
// one on the channel.
func sinks(out chan<- *memSink) func(protocol.Bulk) (BulkSink, error) {
	return func(h protocol.Bulk) (BulkSink, error) {
		s := &memSink{h: h, result: make(chan error, 2)}
		switch h.ID {
		case "refuse":
			return nil, errors.New("not today")
		case "fail-write":
			s.writeErr = errors.New("disk on fire")
		case "fail-commit":
			s.commitErr = errors.New("rename failed")
		case "block":
			s.block = make(chan struct{})
		case "slow-abort":
			s.slowAbort = make(chan struct{})
		}
		out <- s
		return s, nil
	}
}

// bulkPair is an agent peer connected to a stand-in browser that takes
// bulk transfers the same way.
type bulkPair struct {
	agent   *Peer
	browser *browser
	// atAgent and atBrowser report each sink handed out on either side;
	// browserChannels each bulk channel the agent opened.
	atAgent, atBrowser chan *memSink
	browserChannels    chan *webrtc.DataChannel
	// raw leaves the browser's end of those channels to the test.
	raw atomic.Bool
}

func newBulkPair(t *testing.T, cfg Config, h Handlers) *bulkPair {
	t.Helper()
	bp := &bulkPair{
		atAgent: make(chan *memSink, 16), atBrowser: make(chan *memSink, 16),
		browserChannels: make(chan *webrtc.DataChannel, 16),
	}
	cands := make(chan protocol.Candidate, 32)
	connected := make(chan struct{})
	var once atomic.Bool
	h.Candidate = func(c protocol.Candidate) { cands <- c }
	h.State = func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected && once.CompareAndSwap(false, true) {
			close(connected)
		}
	}
	if h.Bulk == nil {
		h.Bulk = sinks(bp.atAgent)
	}
	// No video streams here, so the target stays where it starts, and
	// bulk from the agent is paced under it.
	cfg.MinBitrate, cfg.StartBitrate, cfg.MaxBitrate, cfg.IncludeLoopback = 100_000, 200_000_000, 200_000_000, true
	p, err := New(cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	bp.agent = p

	b := newBrowser(t)
	bp.browser = b
	browserAccept := sinks(bp.atBrowser)
	b.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != protocol.BulkChannel {
			return
		}
		select {
		case bp.browserChannels <- dc:
		default:
		}
		if !bp.raw.Load() {
			receiveBulk(dc, browserAccept, slog.Default())
		}
	})
	connect(t, b, p, cands)
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer never connected")
	}
	waitOpen(t, b.control)
	return bp
}

// upload sends payload from the browser to the agent with the real sender.
func (bp *bulkPair) upload(ctx context.Context, t *testing.T, h protocol.Bulk, r io.Reader) error {
	t.Helper()
	dc, err := bp.browser.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.T = protocol.BulkHeader
	return newBulkSender(dc).send(ctx, h, r)
}

func payload(n int) []byte {
	rng := rand.New(rand.NewPCG(uint64(n), 7))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func receive(t *testing.T, from <-chan *memSink) *memSink {
	t.Helper()
	select {
	case s := <-from:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no transfer arrived")
		return nil
	}
}

// receiveID skips transfers other than id, which earlier cases may leave.
func receiveID(t *testing.T, from <-chan *memSink, id string) *memSink {
	t.Helper()
	for {
		if s := receive(t, from); s.h.ID == id {
			return s
		}
	}
}

func TestBulkBothWays(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	ctx := context.Background()

	for _, size := range []int{0, 1, protocol.BulkChunkSize, 8<<20 + 123} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := payload(size)
			up, down := fmt.Sprintf("up-%d", size), fmt.Sprintf("down-%d", size)
			h := protocol.Bulk{Kind: testKind, ID: up, Name: "notes.txt", MIME: "text/plain", Size: int64(size)}
			start := time.Now()
			if err := bp.upload(ctx, t, h, bytes.NewReader(data)); err != nil {
				t.Fatalf("upload: %v", err)
			}
			if size > 1<<20 {
				t.Logf("browser to agent: %d bytes in %v", size, time.Since(start))
			}
			s := receiveID(t, bp.atAgent, up)
			h.T = protocol.BulkHeader
			if s.h != h {
				t.Errorf("agent got header %+v, want %+v", s.h, h)
			}
			if err := s.end(t); err != nil {
				t.Fatalf("upload aborted: %v", err)
			}
			if !bytes.Equal(s.buf.Bytes(), data) {
				t.Fatalf("upload arrived as %d different bytes", s.buf.Len())
			}

			start = time.Now()
			if err := bp.agent.SendBulk(ctx, protocol.Bulk{Kind: testKind, ID: down, Size: int64(size)}, bytes.NewReader(data)); err != nil {
				t.Fatalf("download: %v", err)
			}
			if size > 1<<20 {
				t.Logf("agent to browser: %d bytes in %v", size, time.Since(start))
			}
			s = receiveID(t, bp.atBrowser, down)
			if err := s.end(t); err != nil || !bytes.Equal(s.buf.Bytes(), data) {
				t.Fatalf("download: %v, %d bytes", err, s.buf.Len())
			}
			if n := s.ends.Load(); n != 1 {
				t.Errorf("sink ended %d times", n)
			}
		})
	}

	// One channel per transfer: several run at once.
	t.Run("several at once", func(t *testing.T) {
		errs := make(chan error, 3)
		for i := range 3 {
			go func() {
				errs <- bp.agent.SendBulk(ctx, protocol.Bulk{Kind: testKind, ID: "many", Size: 200_000}, bytes.NewReader(payload(200_000 + i)[:200_000]))
			}()
		}
		for range 3 {
			if err := <-errs; err != nil {
				t.Fatalf("concurrent download: %v", err)
			}
			if err := receiveID(t, bp.atBrowser, "many").end(t); err != nil {
				t.Fatalf("concurrent download aborted: %v", err)
			}
		}
	})
}

// rawBulk opens a bulk channel from the browser for hand-made frames, and
// reports the agent's frames and the channel's close.
func (bp *bulkPair) rawBulk(t *testing.T) (dc *webrtc.DataChannel, frames <-chan protocol.Bulk, closed <-chan struct{}) {
	t.Helper()
	dc, err := bp.browser.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan protocol.Bulk, 4)
	done := make(chan struct{})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if b, err := protocol.ParseBulk(m.Data); m.IsString && err == nil {
			got <- b
		}
	})
	dc.OnClose(func() { close(done) })
	waitOpen(t, dc)
	return dc, got, done
}

func sendJSON(t *testing.T, dc *webrtc.DataChannel, b protocol.Bulk) {
	t.Helper()
	data, _ := json.Marshal(b)
	if err := dc.SendText(string(data)); err != nil {
		t.Fatal(err)
	}
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestBulkReceiveRefuses(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	header := func(id string, size int64) protocol.Bulk {
		return protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: id, Size: size}
	}
	hundred := payload(100)

	for _, tc := range []struct {
		name string
		// frames sends what the browser sends; the agent's verdict should
		// read verdict, and the sink, when there is one, end with abort.
		frames  func(t *testing.T, dc *webrtc.DataChannel)
		sink    bool
		verdict string
		abort   error
	}{
		{name: "too large", frames: func(t *testing.T, dc *webrtc.DataChannel) { sendJSON(t, dc, header("big", testLimit+1)) },
			verdict: "too large"},
		{name: "unknown kind", frames: func(t *testing.T, dc *webrtc.DataChannel) {
			sendJSON(t, dc, protocol.Bulk{T: protocol.BulkHeader, Kind: "nobody", Size: 1})
		}, verdict: "unknown bulk kind"},
		{name: "refused by the handler", frames: func(t *testing.T, dc *webrtc.DataChannel) { sendJSON(t, dc, header("refuse", 1)) },
			verdict: "not today"},
		{name: "data before the header", frames: func(t *testing.T, dc *webrtc.DataChannel) { dc.Send([]byte{1, 2, 3}) },
			verdict: "data before the header"},
		{name: "junk header", frames: func(t *testing.T, dc *webrtc.DataChannel) { dc.SendText("hello") },
			verdict: "malformed"},
		{name: "end where the header belongs", frames: func(t *testing.T, dc *webrtc.DataChannel) {
			sendJSON(t, dc, protocol.Bulk{T: protocol.BulkEnd, SHA256: sumOf(nil)})
		}, verdict: "malformed"},
		{name: "wrong hash", sink: true, abort: protocol.ErrBulkHash, verdict: "SHA-256 mismatch",
			frames: func(t *testing.T, dc *webrtc.DataChannel) {
				sendJSON(t, dc, header("hash", 100))
				dc.Send(hundred)
				sendJSON(t, dc, protocol.Bulk{T: protocol.BulkEnd, SHA256: sumOf(hundred[1:])})
			}},
		{name: "truncated", sink: true, abort: protocol.ErrBulkTruncated, verdict: "ended early",
			frames: func(t *testing.T, dc *webrtc.DataChannel) {
				sendJSON(t, dc, header("short", 100))
				dc.Send(hundred[:50])
				sendJSON(t, dc, protocol.Bulk{T: protocol.BulkEnd, SHA256: sumOf(hundred[:50])})
			}},
		{name: "past the declared size", sink: true, abort: protocol.ErrBulkOverrun, verdict: "past the declared size",
			frames: func(t *testing.T, dc *webrtc.DataChannel) {
				sendJSON(t, dc, header("long", 10))
				dc.Send(hundred)
			}},
		{name: "oversized chunk", sink: true, abort: protocol.ErrBulkFrame, verdict: "chunk of",
			frames: func(t *testing.T, dc *webrtc.DataChannel) {
				sendJSON(t, dc, header("chunk", testLimit))
				dc.Send(make([]byte, protocol.BulkChunkSize+1))
			}},
		{name: "junk end", sink: true, abort: protocol.ErrBulkFrame, verdict: "malformed",
			frames: func(t *testing.T, dc *webrtc.DataChannel) {
				sendJSON(t, dc, header("junk-end", 0))
				dc.SendText("{")
			}},
		{name: "write fails", sink: true, verdict: "disk on fire",
			frames: func(t *testing.T, dc *webrtc.DataChannel) {
				sendJSON(t, dc, header("fail-write", 100))
				dc.Send(hundred)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc, frames, closed := bp.rawBulk(t)
			tc.frames(t, dc)
			select {
			case v := <-frames:
				if v.T != protocol.BulkError || !strings.Contains(v.Error, tc.verdict) {
					t.Errorf("verdict %+v, want an error with %q", v, tc.verdict)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no verdict")
			}
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
				t.Fatal("the agent left the channel open after its verdict")
			}
			if !tc.sink {
				select {
				case s := <-bp.atAgent:
					t.Fatalf("a sink was opened for %+v", s.h)
				default:
				}
				return
			}
			s := receive(t, bp.atAgent)
			err := s.onlyEnd(t)
			if err == nil || (tc.abort != nil && !errors.Is(err, tc.abort)) {
				t.Errorf("sink ended with %v, want an abort with %v", err, tc.abort)
			}
		})
	}

	t.Run("commit fails", func(t *testing.T) {
		err := bp.upload(context.Background(), t, header("fail-commit", 100), bytes.NewReader(hundred))
		var refused *BulkRefusedError
		if !errors.As(err, &refused) || refused.Reason != "rename failed" {
			t.Fatalf("upload: %v, want refused with the commit error", err)
		}
		if err := receive(t, bp.atAgent).onlyEnd(t); err != nil {
			t.Fatalf("sink: %v, want a commit", err)
		}
	})

	// The verdict goes out before the sink's abort, which may take a while.
	t.Run("verdict before a slow abort", func(t *testing.T) {
		dc, frames, _ := bp.rawBulk(t)
		sendJSON(t, dc, header("slow-abort", 100))
		dc.Send(hundred)
		sendJSON(t, dc, protocol.Bulk{T: protocol.BulkEnd, SHA256: sumOf(hundred[1:])})
		s := receive(t, bp.atAgent)
		release := sync.OnceFunc(func() { close(s.slowAbort) })
		defer release()
		select {
		case v := <-frames:
			if v.T != protocol.BulkError || !strings.Contains(v.Error, "SHA-256 mismatch") {
				t.Errorf("verdict %+v, want the hash mismatch", v)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no verdict while the sink was still aborting")
		}
		if n := s.ends.Load(); n != 0 {
			t.Fatalf("sink ended %d times before its abort was let go", n)
		}
		release()
		if err := s.onlyEnd(t); !errors.Is(err, protocol.ErrBulkHash) {
			t.Fatalf("sink ended with %v, want ErrBulkHash", err)
		}
	})

	t.Run("cancelled by the browser", func(t *testing.T) {
		dc, _, _ := bp.rawBulk(t)
		sendJSON(t, dc, header("cancel", 100))
		dc.Send(hundred[:50])
		s := receive(t, bp.atAgent)
		dc.Close()
		if err := s.onlyEnd(t); !errors.Is(err, ErrBulkCancelled) {
			t.Fatalf("sink ended with %v, want ErrBulkCancelled", err)
		}
	})

	t.Run("nothing takes bulk", func(t *testing.T) {
		r := &bulkReceiver{accept: nil, log: slog.Default()}
		if err := r.open(webrtc.DataChannelMessage{IsString: true, Data: []byte(`{"t":"header","kind":"test"}`)}); !errors.Is(err, protocol.ErrBulkKind) {
			t.Fatalf("open without a handler: %v, want ErrBulkKind", err)
		}
	})
}

// cancelAfter is a reader of n bytes of zeros that cancels a context once
// only left bytes remain.
type cancelAfter struct {
	n, left int
	cancel  context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, io.EOF
	}
	k := min(len(p), c.n)
	clear(p[:k])
	c.n -= k
	if c.n <= c.left {
		c.cancel()
	}
	return k, nil
}

var errRead = errors.New("read failed")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errRead }

func TestBulkSendFails(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	ctx := context.Background()
	send := func(ctx context.Context, id string, size int64, r io.Reader) error {
		return bp.agent.SendBulk(ctx, protocol.Bulk{Kind: testKind, ID: id, Size: size}, r)
	}

	if err := bp.agent.SendBulk(ctx, protocol.Bulk{Kind: "nobody"}, bytes.NewReader(nil)); !errors.Is(err, protocol.ErrBulkKind) {
		t.Errorf("unknown kind: %v, want ErrBulkKind", err)
	}
	if err := send(ctx, "big", testLimit+1, bytes.NewReader(nil)); !errors.Is(err, protocol.ErrBulkTooLarge) {
		t.Errorf("too large: %v, want ErrBulkTooLarge", err)
	}

	var refused *BulkRefusedError
	if err := send(ctx, "refuse", 10, bytes.NewReader(make([]byte, 10))); !errors.As(err, &refused) || refused.Reason != "not today" {
		t.Errorf("refused by the browser: %v", err)
	}

	for _, tc := range []struct {
		name string
		size int64
		r    io.Reader
		want error
	}{
		{"short reader", 1000, bytes.NewReader(make([]byte, 999)), protocol.ErrBulkTruncated},
		{"long reader", 1000, bytes.NewReader(make([]byte, 1001)), protocol.ErrBulkOverrun},
	} {
		if err := send(ctx, tc.name, tc.size, tc.r); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
		if err := receiveID(t, bp.atBrowser, tc.name).end(t); !errors.Is(err, ErrBulkCancelled) {
			t.Errorf("%s: browser sink ended with %v, want ErrBulkCancelled", tc.name, err)
		}
	}

	if err := send(ctx, "broken", 10, failingReader{}); !errors.Is(err, errRead) {
		t.Errorf("failing reader: %v, want its read error", err)
	}
	if err := receiveID(t, bp.atBrowser, "broken").end(t); !errors.Is(err, ErrBulkCancelled) {
		t.Errorf("failing reader: browser sink ended with %v, want ErrBulkCancelled", err)
	}

	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := send(gone, "early", 10, bytes.NewReader(make([]byte, 10))); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled before the start: %v, want context.Canceled", err)
	}

	mid, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := send(mid, "mid", 4<<20, &cancelAfter{n: 4 << 20, left: 2 << 20, cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled mid-transfer: %v, want context.Canceled", err)
	}
	if err := receiveID(t, bp.atBrowser, "mid").end(t); !errors.Is(err, ErrBulkCancelled) {
		t.Errorf("cancelled mid-transfer: browser sink ended with %v, want ErrBulkCancelled", err)
	}
}

// TestBulkCancelWhileBlocked closes the browser's end while the agent waits
// for credit from a sink that stopped taking chunks, a wait no credit ever
// ends.
func TestBulkCancelWhileBlocked(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	dc, err := bp.agent.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	sender := newBulkSender(dc)
	go func() {
		result <- sender.send(context.Background(), protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: "block", Size: testLimit}, bytes.NewReader(make([]byte, testLimit)))
	}()
	s := receive(t, bp.atBrowser)
	defer close(s.block)
	waitStalled(t, dc, s)
	(<-bp.browserChannels).Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrBulkCancelled) {
			t.Fatalf("blocked sender: %v, want ErrBulkCancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked sender never noticed the close")
	}
}

// TestBulkSenderIgnoresCreditPastWhatWasSent: a receiver that grants far
// more than it could have taken, then stops reading, holds the agent to a
// window past what it sent, not the whole transfer in the SCTP queue.
func TestBulkSenderIgnoresCreditPastWhatWasSent(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	bp.raw.Store(true)
	dc, err := bp.agent.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := newBulkSender(dc)
	go sender.send(context.Background(), protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: "greedy", Size: testLimit}, bytes.NewReader(make([]byte, testLimit)))
	var far *webrtc.DataChannel
	select {
	case far = <-bp.browserChannels:
	case <-time.After(10 * time.Second):
		t.Fatal("the channel never reached the browser")
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	var once sync.Once
	far.OnMessage(func(webrtc.DataChannelMessage) {
		once.Do(func() { sendJSON(t, far, protocol.Bulk{T: protocol.BulkCredit, Credit: 1 << 40}) })
		<-stop // the browser reads nothing more on this channel
	})
	deadline := time.Now().Add(10 * time.Second)
	queued := dc.BufferedAmount()
	for still := 0; still < 10; {
		if time.Now().After(deadline) {
			t.Fatalf("the queue never stopped growing: %d bytes", dc.BufferedAmount())
		}
		time.Sleep(20 * time.Millisecond)
		now := dc.BufferedAmount()
		if now == queued {
			still++
		} else {
			still = 0
		}
		queued = now
	}
	if limit := uint64(2 * protocol.BulkWindow); queued > limit {
		t.Fatalf("%d bytes queued for a receiver that stopped reading, want at most %d", queued, limit)
	}
}

// waitStalled waits until the sink is in its first Write, which blocks, and
// dc's queue has stopped moving since. With the credit the queue is empty
// then: the sender has sent its credit and waits for more.
func waitStalled(t *testing.T, dc *webrtc.DataChannel, s *memSink) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	queued := dc.BufferedAmount()
	for still := 0; still < 5; {
		if time.Now().After(deadline) {
			t.Fatalf("the transfer never stalled: %d writes begun, %d bytes queued", s.writes.Load(), dc.BufferedAmount())
		}
		time.Sleep(20 * time.Millisecond)
		now := dc.BufferedAmount()
		if s.writes.Load() > 0 && now == queued {
			still++
		} else {
			still = 0
		}
		queued = now
	}
}

// TestBulkLeavesControlLiveBehindAStalledSink: a sink that stops taking
// its transfer holds back that transfer and nothing else. Each way, a
// 16 MiB transfer runs into a sink that has stalled while control
// messages cross the same SCTP association, and each must arrive at once.
// The credit keeps the stalled transfer from filling the association's
// receive buffer, where the messages would wait on the sink.
func TestBulkLeavesControlLiveBehindAStalledSink(t *testing.T) {
	atAgent := make(chan protocol.Control, 64)
	bp := newBulkPair(t, Config{}, Handlers{Control: func(m protocol.Control) { atAgent <- m }})
	// stall starts a transfer from pc to a sink that takes nothing, and
	// returns once it waits for credit. The transfer ends with the subtest.
	stall := func(t *testing.T, pc *webrtc.PeerConnection, pace bulkPace, sinks <-chan *memSink) {
		t.Helper()
		dc, err := pc.CreateDataChannel(protocol.BulkChannel, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		sender := newBulkSender(dc)
		sender.pace = pace
		errc := make(chan error, 1)
		go func() {
			errc <- sender.send(ctx, protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: "block", Size: testLimit}, bytes.NewReader(make([]byte, testLimit)))
		}()
		s := receiveID(t, sinks, "block")
		t.Cleanup(func() {
			cancel()
			close(s.block)
			if err := <-errc; !errors.Is(err, context.Canceled) {
				t.Errorf("the stalled transfer ended with %v, want context.Canceled", err)
			}
			if err := s.end(t); !errors.Is(err, ErrBulkCancelled) {
				t.Errorf("the stalled sink ended with %v, want ErrBulkCancelled", err)
			}
		})
		waitStalled(t, dc, s)
	}
	check := func(t *testing.T, send func(i int) error, got <-chan protocol.Control) {
		t.Helper()
		var worst time.Duration
		for i := range 5 {
			sent := time.Now()
			if err := send(i); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-got:
				lag := time.Since(sent)
				worst = max(worst, lag)
				if lag > 500*time.Millisecond || m.Text != fmt.Sprint(i) {
					t.Errorf("control message %q arrived %v after it was sent", m.Text, lag.Round(time.Millisecond))
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("control message %d held back behind the stalled transfer", i)
			}
		}
		t.Logf("control messages beside a stalled transfer took %v at most", worst.Round(100*time.Microsecond))
	}
	t.Run("browser to agent", func(t *testing.T) {
		stall(t, bp.browser.pc, nil, bp.atAgent)
		check(t, func(i int) error {
			b, _ := json.Marshal(protocol.Control{T: protocol.ControlClipboard, Text: fmt.Sprint(i)})
			return bp.browser.control.SendText(string(b))
		}, atAgent)
	})
	t.Run("agent to browser", func(t *testing.T) {
		stall(t, bp.agent.pc, bp.agent.bwe, bp.atBrowser)
		check(t, func(i int) error {
			return bp.agent.SendControl(protocol.Control{T: protocol.ControlClipboard, Text: fmt.Sprint(i)})
		}, bp.browser.got)
	})
}

// TestBulkReceiverGrantsCredit: a window of chunks sent by hand earns a
// credit at half of it and another at the whole.
func TestBulkReceiverGrantsCredit(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	dc, frames, _ := bp.rawBulk(t)
	sendJSON(t, dc, protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: "raw", Size: testLimit})
	receiveID(t, bp.atAgent, "raw")
	chunk := make([]byte, protocol.BulkChunkSize)
	for sent := 0; sent < protocol.BulkWindow; sent += len(chunk) {
		if err := dc.Send(chunk); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int64{protocol.BulkWindow + protocol.BulkWindow/2, 2 * protocol.BulkWindow} {
		select {
		case f := <-frames:
			if f != (protocol.Bulk{T: protocol.BulkCredit, Credit: want}) {
				t.Fatalf("frame %+v, want a credit of %d", f, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no credit of %d", want)
		}
	}
}

// TestBulkVerdicts drives the agent's sender against a hand-made receiver
// in the browser.
func TestBulkVerdicts(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	bp.raw.Store(true)
	for _, tc := range []struct {
		name  string
		reply func(t *testing.T, dc *webrtc.DataChannel)
		check func(error) bool
	}{
		{"done before the end", func(t *testing.T, dc *webrtc.DataChannel) { sendJSON(t, dc, protocol.Bulk{T: protocol.BulkDone}) },
			func(err error) bool {
				var r *BulkRefusedError
				return errors.As(err, &r) && strings.Contains(r.Reason, "before the end")
			}},
		{"junk verdict", func(t *testing.T, dc *webrtc.DataChannel) { dc.SendText("junk") },
			func(err error) bool {
				var r *BulkRefusedError
				return errors.As(err, &r) && r.Reason == "malformed verdict"
			}},
		{"closed without a verdict", func(t *testing.T, dc *webrtc.DataChannel) { dc.Close() },
			func(err error) bool { return errors.Is(err, ErrBulkCancelled) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc, err := bp.agent.pc.CreateDataChannel(protocol.BulkChannel, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			sender := newBulkSender(dc)
			go func() {
				result <- sender.send(context.Background(), protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, Size: testLimit}, bytes.NewReader(make([]byte, testLimit)))
			}()
			var far *webrtc.DataChannel
			select {
			case far = <-bp.browserChannels:
			case <-time.After(10 * time.Second):
				t.Fatal("the channel never reached the browser")
			}
			waitOpen(t, far)
			tc.reply(t, far)
			select {
			case err := <-result:
				if !tc.check(err) {
					t.Fatalf("sender: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("sender never finished")
			}
		})
	}
}

func TestPeerRoutesChannelsByLabel(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	dc, err := bp.browser.pc.CreateDataChannel("other", nil)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	dc.OnClose(func() { close(closed) })
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept a channel with an unknown label")
	}
}

func TestPeerHandsOverRemoteTracks(t *testing.T) {
	tracks := make(chan *webrtc.TrackRemote, 1)
	bp := newBulkPair(t, Config{Audio: true, Mic: true}, Handlers{Track: func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { tracks <- tr }})
	mic, err := webrtc.NewTrackLocalStaticSample(opusCapability, "audio", "browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bp.browser.pc.AddTrack(mic); err != nil {
		t.Fatal(err)
	}
	// Adding a track needs a fresh offer, as the page would send.
	offer, err := bp.browser.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bp.browser.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := bp.agent.Answer(offer.SDP)
	if err != nil {
		t.Fatal(err)
	}
	if err := bp.browser.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	// The writer stops before the cleanups close the peers under it.
	stop := make(chan struct{})
	var writer sync.WaitGroup
	defer func() {
		close(stop)
		writer.Wait()
	}()
	writer.Go(func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				mic.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
			}
		}
	})
	select {
	case tr := <-tracks:
		if tr.Kind() != webrtc.RTPCodecTypeAudio || !strings.EqualFold(tr.Codec().MimeType, webrtc.MimeTypeOpus) {
			t.Fatalf("remote track %s %s", tr.Kind(), tr.Codec().MimeType)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the browser's track never reached the handler")
	}
}

// TestBulkSenderOutcomes covers how the sender explains a stop, including
// the races between a verdict, a close and a failed send that a live
// channel produces only now and then.
func TestBulkSenderOutcomes(t *testing.T) {
	ctx := context.Background()
	gone, cancel := context.WithCancel(ctx)
	cancel()
	sendFailed := errors.New("stream closed")
	fresh := func(verdict *protocol.Bulk, closed bool) *bulkSender {
		s := &bulkSender{closed: make(chan struct{}), verdict: make(chan protocol.Bulk, 1)}
		if verdict != nil {
			s.verdict <- *verdict
		}
		if closed {
			close(s.closed)
		}
		return s
	}
	full := &protocol.Bulk{T: protocol.BulkError, Error: "full"}
	refusedFull := func(err error) bool {
		var r *BulkRefusedError
		return errors.As(err, &r) && r.Reason == "full"
	}

	if err := fresh(full, false).sendErr(ctx, sendFailed); !refusedFull(err) {
		t.Errorf("send failed after a refusal: %v", err)
	}
	if err := fresh(full, true).sendErr(ctx, sendFailed); !refusedFull(err) {
		t.Errorf("send failed after a refusal and a close: %v", err)
	}
	if err := fresh(nil, true).sendErr(ctx, sendFailed); !errors.Is(err, ErrBulkCancelled) {
		t.Errorf("send failed after a close: %v, want ErrBulkCancelled", err)
	}
	if err := fresh(nil, false).sendErr(gone, sendFailed); !errors.Is(err, context.Canceled) {
		t.Errorf("send failed after cancelling: %v, want context.Canceled", err)
	}

	if err := fresh(nil, false).check(ctx); err != nil {
		t.Errorf("check with nothing to report: %v", err)
	}
	if err := fresh(nil, false).check(gone); !errors.Is(err, context.Canceled) {
		t.Errorf("check after cancelling: %v", err)
	}
	if err := fresh(nil, true).check(ctx); !errors.Is(err, ErrBulkCancelled) {
		t.Errorf("check after a close: %v", err)
	}
	if err := fresh(nil, true).wait(ctx, nil); !errors.Is(err, ErrBulkCancelled) {
		t.Errorf("wait after a close: %v", err)
	}
	if err := fresh(full, true).afterClose(); !refusedFull(err) {
		t.Errorf("close after a refusal: %v", err)
	}
	if got, want := (&BulkRefusedError{Reason: "full"}).Error(), "peer: bulk transfer refused: full"; got != want {
		t.Errorf("refusal reads %q, want %q", got, want)
	}

	if got := reason(errors.New(strings.Repeat("x", 3*maxBulkReason))); len(got) != maxBulkReason {
		t.Errorf("reason kept %d bytes, want %d", len(got), maxBulkReason)
	}
}
