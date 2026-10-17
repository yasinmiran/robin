package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"

	"github.com/yasinmiran/remotejump/internal/protocol"
	"github.com/yasinmiran/remotejump/internal/relay"
	"github.com/yasinmiran/remotejump/internal/web"
)

// signaling is what the bridge needs from the agent's stdio connection.
type signaling interface {
	Send(protocol.Message) error
	Recv(context.Context) (protocol.Message, error)
}

// browsers yields authenticated page connections, one at a time.
type browsers interface {
	Accept(ctx context.Context) (*web.Browser, error)
}

// bridge relays signaling between whichever page is connected and the agent.
// A page that goes away (reload, closed tab) costs the agent its peer
// connection but not its display.
type bridge struct {
	agent  signaling
	relay  *relay.Relay
	config protocol.Config
	log    *slog.Logger

	mu  sync.Mutex
	cur *web.Browser
}

// run relays until ctx ends, SSH drops, the agent goes away or the page
// server stops. serveErr carries the page server's result.
func (b *bridge) run(ctx context.Context, srv browsers, sshDone <-chan struct{}, serveErr <-chan error) error {
	// Ending run must also end the accept loop below, whatever the reason.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	agentErr := make(chan error, 1)
	go func() { agentErr <- b.fromAgent(ctx) }()

	accepted := make(chan *web.Browser)
	go func() {
		for {
			br, err := srv.Accept(ctx)
			if err != nil {
				return
			}
			select {
			case accepted <- br:
			case <-ctx.Done():
				br.Close()
				return
			}
		}
	}()

	var prevDone chan struct{}
	for {
		select {
		case <-ctx.Done():
			b.closeCurrent()
			return ctx.Err()
		case <-sshDone:
			b.closeCurrent()
			return errors.New("SSH connection to the host was lost")
		case err := <-serveErr:
			b.closeCurrent()
			return fmt.Errorf("page server stopped: %w", err)
		case err := <-agentErr:
			b.closeCurrent()
			if errors.Is(err, io.EOF) {
				return errors.New("agent exited")
			}
			return err
		case br := <-accepted:
			// The previous page's hangup must reach the agent before this
			// page's offer, or the offer would land on the old peer.
			if prevDone != nil {
				<-prevDone
			}
			done := make(chan struct{})
			prevDone = done
			b.mu.Lock()
			b.cur = br
			b.mu.Unlock()
			if err := br.Send(protocol.Message{Type: protocol.TypeConfig, Config: &b.config}); err != nil {
				b.log.Warn("send config", "err", err)
			}
			go b.fromBrowser(ctx, br, done)
		}
	}
}

func (b *bridge) fromAgent(ctx context.Context) error {
	for {
		m, err := b.agent.Recv(ctx)
		if err != nil {
			return err
		}
		if m.Type == protocol.TypeError {
			b.log.Warn("agent reported", "err", m.Error)
		}
		out, err := b.relay.FromAgent(m)
		if err != nil {
			// A fingerprint change means something other than our agent is
			// answering. End the session rather than connect the page to it.
			return err
		}
		b.mu.Lock()
		br := b.cur
		b.mu.Unlock()
		if br == nil {
			continue
		}
		for _, o := range out {
			if err := br.Send(o); err != nil {
				b.log.Debug("send to page", "err", err)
			}
		}
	}
}

func (b *bridge) fromBrowser(ctx context.Context, br *web.Browser, done chan<- struct{}) {
	defer close(done)
	for {
		m, err := br.Recv(ctx)
		if err != nil {
			break
		}
		if err := b.agent.Send(m); err != nil {
			b.log.Warn("send to agent", "err", err)
			break
		}
	}
	b.mu.Lock()
	if b.cur == br {
		b.cur = nil
	}
	b.mu.Unlock()
	br.Close()
	b.log.Info("page disconnected")
	if err := b.agent.Send(protocol.Message{Type: protocol.TypeHangup}); err != nil {
		b.log.Debug("send hangup", "err", err)
	}
}

func (b *bridge) closeCurrent() {
	b.mu.Lock()
	br := b.cur
	b.mu.Unlock()
	if br != nil {
		br.Close()
	}
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the opener when it exits; it would otherwise stay a zombie for the
	// whole session.
	go cmd.Wait()
	return nil
}

// prefixWriter prefixes each line written to w.
func prefixWriter(w io.Writer, prefix string) io.Writer {
	return &linePrefixer{w: w, prefix: []byte(prefix)}
}

type linePrefixer struct {
	mu      sync.Mutex
	w       io.Writer
	prefix  []byte
	partial []byte
}

func (l *linePrefixer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		if _, err := fmt.Fprintf(l.w, "%s%s", l.prefix, l.partial[:i+1]); err != nil {
			return 0, err
		}
		l.partial = l.partial[i+1:]
	}
}
