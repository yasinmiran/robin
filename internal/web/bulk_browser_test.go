package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// testKind is the one bulk kind TestBrowserBulk sends.
const testKind = "test"

func init() { protocol.RegisterBulkKind(testKind, 16<<20) }

// TestBrowserBulk sends bulk transfers both ways between the page in system
// Chrome and the agent's own peer: an upload the agent sends back, a
// refusal each way, a cancel from the page and one over the kind's limit.
// Like TestBrowserSmoke it needs `npm ci` in testdata/browser.
func TestBrowserBulk(t *testing.T) {
	dir := filepath.Join("testdata", "browser")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "playwright-core")); err != nil {
		t.Skip("browser bulk test needs `npm ci` in internal/web/testdata/browser")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("browser bulk test needs node on PATH")
	}
	s, _ := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a := &bulkAgent{got: map[string]*heldSink{}, sent: make(chan struct{})}
	served := make(chan error, 1)
	go func() {
		b, err := s.Accept(ctx)
		if err != nil {
			served <- err
			return
		}
		served <- a.play(ctx, b)
	}()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, node, "bulk.mjs", s.URL())
	cmd.Dir, cmd.Stderr = dir, &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bulk.mjs: %v\n%s%s", err, out, stderr.String())
	}
	var r struct {
		Upload, UpSHA256           string
		UploadMs, Size             int
		Refused, Aborted, TooLarge string
		Down                       struct {
			Header protocol.Bulk
			SHA256 string
			Size   int
		}
		Errors []string
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("bulk.mjs printed %q: %v", out, err)
	}
	select {
	case <-a.sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's transfers to the page never finished")
	}
	// The cancelled upload's sink learns of it once its stalled write ends.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		a.mu.Lock()
		c := a.got["cancel"]
		done := c != nil && c.abort != nil
		a.mu.Unlock()
		if done {
			break
		}
	}
	cancel()
	<-served
	t.Logf("page to agent: %d bytes in %d ms; refused %q, aborted %q, over the limit %q", r.Size, r.UploadMs, r.Refused, r.Aborted, r.TooLarge)

	a.mu.Lock()
	defer a.mu.Unlock()
	up := a.got["up"]
	if r.Upload != "ok" || up == nil {
		t.Fatalf("upload: %s", r.Upload)
	}
	want := protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: "up", Name: "noise.bin", MIME: "application/octet-stream", Size: int64(r.Size)}
	if up.h != want {
		t.Errorf("agent got header %+v, want %+v", up.h, want)
	}
	if got := sha256.Sum256(up.buf.Bytes()); hex.EncodeToString(got[:]) != r.UpSHA256 || !up.committed {
		t.Errorf("agent kept %d bytes, committed %v; they do not hash as the page's do", up.buf.Len(), up.committed)
	}
	if a.echoErr != nil || r.Down.SHA256 != r.UpSHA256 || r.Down.Size != r.Size || r.Down.Header.ID != "down" || r.Down.Header.Name != "noise.bin" {
		t.Errorf("agent to page: %v; page got %+v", a.echoErr, r.Down)
	}
	var refused *peer.BulkRefused
	if !errors.As(a.refuseErr, &refused) || refused.Reason != "page says no" {
		t.Errorf("transfer the page refuses: %v", a.refuseErr)
	}
	if !strings.Contains(r.Refused, "refused: not today") {
		t.Errorf("transfer the agent refuses: %s", r.Refused)
	}
	if !strings.HasPrefix(r.Aborted, "AbortError") {
		t.Errorf("aborted upload: %s", r.Aborted)
	}
	if c := a.got["cancel"]; c == nil || !errors.Is(c.abort, peer.ErrBulkCancelled) {
		t.Errorf("aborted upload reached the agent as %+v", c)
	}
	if !strings.Contains(r.TooLarge, "too large") {
		t.Errorf("upload over the limit: %s", r.TooLarge)
	}
	if len(r.Errors) > 0 {
		t.Errorf("page errors: %v", r.Errors)
	}
}

// bulkAgent plays the agent for TestBrowserBulk with the real peer.
type bulkAgent struct {
	mu                 sync.Mutex
	got                map[string]*heldSink // by transfer ID
	echoErr, refuseErr error
	sent               chan struct{}
}

// heldSink keeps an upload. One with ID "cancel" stalls its first write
// long enough for the page to give up.
type heldSink struct {
	a         *bulkAgent
	h         protocol.Bulk
	buf       bytes.Buffer
	committed bool
	abort     error
	stalled   bool
	onCommit  func([]byte)
}

func (s *heldSink) Write(p []byte) (int, error) {
	if s.h.ID == "cancel" && !s.stalled {
		s.stalled = true
		time.Sleep(2 * time.Second)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	return s.buf.Write(p)
}

func (s *heldSink) Commit() error {
	s.a.mu.Lock()
	s.committed = true
	data := bytes.Clone(s.buf.Bytes())
	s.a.mu.Unlock()
	if s.onCommit != nil {
		go s.onCommit(data)
	}
	return nil
}

func (s *heldSink) Abort(err error) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	s.abort = err
}

func (a *bulkAgent) play(ctx context.Context, b *Browser) error {
	defer b.Close()
	if err := b.Send(protocol.Message{Type: protocol.TypeConfig, Config: &protocol.Config{ICEServers: []protocol.ICEServer{}}}); err != nil {
		return err
	}
	var p *peer.Peer
	p, err := peer.New(peer.Config{IncludeLoopback: true, MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000}, peer.Handlers{
		Candidate: func(c protocol.Candidate) { b.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &c}) },
		// Heartbeat like the real agent, or the page treats the link as stalled.
		ControlOpen: func() {
			go func() {
				for ctx.Err() == nil && p.SendControl(protocol.Control{T: protocol.ControlHeartbeat}) == nil {
					time.Sleep(time.Second)
				}
			}()
		},
		Bulk: func(h protocol.Bulk) (peer.BulkSink, error) {
			if h.ID == "refuse" {
				return nil, errors.New("not today")
			}
			s := &heldSink{a: a, h: h}
			if h.ID == "up" {
				s.onCommit = func(data []byte) { a.sendBack(ctx, p, h, data) }
			}
			a.mu.Lock()
			a.got[h.ID] = s
			a.mu.Unlock()
			return s, nil
		},
	})
	if err != nil {
		return err
	}
	defer p.Close()
	for {
		m, err := b.Recv(ctx)
		if err != nil {
			return nil // the page went away
		}
		switch m.Type {
		case protocol.TypeOffer:
			answer, err := p.Answer(m.SDP)
			if err != nil {
				return err
			}
			if err := b.Send(protocol.Message{Type: protocol.TypeAnswer, SDP: answer}); err != nil {
				return err
			}
		case protocol.TypeCandidate:
			if m.Candidate != nil {
				p.AddCandidate(*m.Candidate)
			}
		}
	}
}

// sendBack returns an upload to the page, then offers it a transfer it
// refuses.
func (a *bulkAgent) sendBack(ctx context.Context, p *peer.Peer, h protocol.Bulk, data []byte) {
	echo := p.SendBulk(ctx, protocol.Bulk{Kind: testKind, ID: "down", Name: h.Name, MIME: h.MIME, Size: int64(len(data))}, bytes.NewReader(data))
	refuse := p.SendBulk(ctx, protocol.Bulk{Kind: testKind, ID: "refuse", Size: 1}, strings.NewReader("x"))
	a.mu.Lock()
	a.echoErr, a.refuseErr = echo, refuse
	a.mu.Unlock()
	close(a.sent)
}
