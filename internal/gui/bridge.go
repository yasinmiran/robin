package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/yasinmiran/robin/internal/macros"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/web"
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

// Why one agent connection's relay ended.
var (
	// errSSHLost: the master went away.
	errSSHLost = errors.New("SSH connection to the host was lost")
	// errAgentGone: the agent's channel closed without a word, as it does
	// when the proxy dies with its connection.
	errAgentGone = errors.New("agent exited")
	// errDetached: the page asked to leave the desktop running.
	errDetached = errors.New("the page detached, keeping the desktop")
	// errEnded: the host ended the session on purpose: bye, the keep
	// window ran out, or robin end.
	errEnded = errors.New("session ended")
	// errPageEnded: the page asked to end the session; the CLI has yet to
	// make the host do it.
	errPageEnded = errors.New("the page ended the session")
	// errNoMacros: the page asked to change macros in a session that keeps
	// none.
	errNoMacros = errors.New("this session keeps no macros")
)

// bridge relays signaling between whichever page is connected and the agent.
// A page that goes away (reload, closed tab) costs the agent its peer
// connection but not its display. The page outlives the agent connection
// too: while the CLI redials, the page is told so and waits for the next
// Config, which makes it build a fresh peer connection.
type bridge struct {
	relay  *relay.Relay
	config protocol.Config
	log    *slog.Logger
	// macros hands every Config the macros as they are then and makes the
	// page's changes to them; nil when this session keeps none.
	macros *macros.Store

	mu    sync.Mutex
	cur   *web.Browser
	agent signaling // nil while no agent connection is up
	// redialing marks the gap between agent connections, so a page that
	// arrives then is told to wait.
	redialing bool
	// events carries what the page decides: errDetached or errPageEnded.
	events chan error
}

// newBridge makes a bridge whose first agent connection is agent, so a page
// that connects before run begins is not left without one.
func newBridge(agent signaling, r *relay.Relay, config protocol.Config, m *macros.Store, log *slog.Logger) *bridge {
	return &bridge{agent: agent, relay: r, config: config, macros: m, log: log, events: make(chan error, 2)}
}

// accept takes pages as they connect, one at a time, for the life of ctx.
func (b *bridge) accept(ctx context.Context, srv browsers) {
	var prevDone chan struct{}
	for {
		br, err := srv.Accept(ctx)
		if err != nil {
			return
		}
		// The previous page's hangup must reach the agent before this
		// page's offer, or the offer would land on the old peer.
		if prevDone != nil {
			<-prevDone
		}
		done := make(chan struct{})
		prevDone = done
		b.mu.Lock()
		b.cur = br
		redialing := b.redialing
		b.mu.Unlock()
		if redialing {
			if err := br.Send(protocol.Message{Type: protocol.TypeReconnecting}); err != nil {
				b.log.Warn("send reconnecting", "err", err)
			}
		} else {
			b.sendConfig(br)
		}
		go b.fromBrowser(ctx, br, done)
	}
}

func (b *bridge) sendConfig(br *web.Browser) {
	config := b.config
	if b.macros != nil {
		config.Macros = b.macroView(b.macros.Load())
	}
	if err := br.Send(protocol.Message{Type: protocol.TypeConfig, Config: &config}); err != nil {
		b.log.Warn("send config", "err", err)
	}
}

// changeMacros makes the page's change to the macros and tells the page
// what it shows now. The page names no file and asks for one change; the
// keeper checks it before anything is written.
func (b *bridge) changeMacros(br *web.Browser, c *protocol.MacroChange) {
	list, err := []protocol.Macro(nil), errNoMacros
	if b.macros != nil {
		list, err = b.macros.Change(c)
	}
	if err != nil {
		b.log.Warn("macros not changed", "err", err)
	}
	if err := br.Send(protocol.Message{Type: protocol.TypeMacros, Macros: b.macroView(list, err)}); err != nil {
		b.log.Debug("send macros", "err", err)
	}
}

// macroView is what the page shows of the macros, and why the last read or
// change failed.
func (b *bridge) macroView(list []protocol.Macro, err error) *protocol.Macros {
	v := &protocol.Macros{List: list}
	if b.macros != nil {
		v.Host = b.macros.Host()
	}
	if v.List == nil {
		v.List = []protocol.Macro{}
	}
	if err != nil {
		v.Error = err.Error()
		v.ReadOnly = errors.Is(err, macros.ErrUnreadable)
	}
	return v
}

// run relays one agent connection until it ends: the agent or SSH goes
// away, the page server stops, the page detaches or ends the session, or
// ctx ends. serveErr carries the page server's result. When run returns,
// nothing reads the agent connection any more, so the caller may.
func (b *bridge) run(ctx context.Context, agent signaling, sshDone <-chan struct{}, serveErr <-chan error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.mu.Lock()
	b.agent = agent
	b.redialing = false
	br := b.cur
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.agent = nil
		b.mu.Unlock()
	}()
	if br != nil {
		// A page that waited through a redial builds its peer connection
		// anew from this; the first page got its Config on arrival and
		// ignores a second.
		b.sendConfig(br)
	}
	agentErr := make(chan error, 1)
	go func() { agentErr <- b.fromAgent(ctx, agent) }()
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-sshDone:
		err = errSSHLost
	case e := <-serveErr:
		err = fmt.Errorf("page server stopped: %w", e)
	case err = <-agentErr:
		return err
	case err = <-b.events:
	}
	cancel()
	<-agentErr
	return err
}

// decided records what the page asked for. Only the first decision
// matters; one that finds the channel full is dropped.
func (b *bridge) decided(err error) {
	select {
	case b.events <- err:
	default:
	}
}

// ended tells the page the session is over, once the host confirmed it.
func (b *bridge) ended(why string) {
	b.mu.Lock()
	br := b.cur
	b.mu.Unlock()
	if br != nil {
		if err := br.Send(protocol.Message{Type: protocol.TypeEnded, Error: why}); err != nil {
			b.log.Debug("send ended", "err", err)
		}
	}
}

// reconnecting tells the current page the host connection is being redialed,
// and pages that arrive meanwhile the same.
func (b *bridge) reconnecting() {
	b.mu.Lock()
	b.redialing = true
	br := b.cur
	b.mu.Unlock()
	if br != nil {
		if err := br.Send(protocol.Message{Type: protocol.TypeReconnecting}); err != nil {
			b.log.Debug("send reconnecting", "err", err)
		}
	}
}

// fromAgent relays the agent's messages to the page until the connection
// ends. An error message the agent sent as its last word is the reason it
// left; one it went on talking after was about an offer, not the session.
// Without any it went silently.
func (b *bridge) fromAgent(ctx context.Context, agent signaling) error {
	var last string
	for {
		m, err := agent.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if last != "" {
				return &agentError{text: last}
			}
			return errAgentGone
		}
		switch m.Type {
		case protocol.TypeEnded:
			return fmt.Errorf("%w: %s", errEnded, m.Error)
		case protocol.TypeError:
			b.log.Warn("agent reported", "err", m.Error)
			last = m.Error
		default:
			last = ""
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

// currentAgent is the agent connection to send to, or nil between them.
func (b *bridge) currentAgent() signaling {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.agent
}

func (b *bridge) fromBrowser(ctx context.Context, br *web.Browser, done chan<- struct{}) {
	defer close(done)
	for {
		m, err := br.Recv(ctx)
		if err != nil {
			break
		}
		switch m.Type {
		case protocol.TypeDetach:
			b.decided(errDetached)
			continue
		case protocol.TypeBye:
			// The CLI ends the session, and waits for the host to say so.
			b.decided(errPageEnded)
			continue
		case protocol.TypeMacros:
			b.changeMacros(br, m.MacroChange)
			continue
		}
		agent := b.currentAgent()
		if agent == nil {
			b.log.Debug("dropped while reconnecting", "type", m.Type)
			continue
		}
		if err := agent.Send(m); err != nil {
			b.log.Warn("send to agent", "err", err)
		}
	}
	b.mu.Lock()
	if b.cur == br {
		b.cur = nil
	}
	b.mu.Unlock()
	br.Close()
	b.log.Info("page disconnected")
	if agent := b.currentAgent(); agent != nil {
		if err := agent.Send(protocol.Message{Type: protocol.TypeHangup}); err != nil {
			b.log.Debug("send hangup", "err", err)
		}
	}
}

// close drops the current page once the session is over.
func (b *bridge) close() {
	b.mu.Lock()
	br := b.cur
	b.mu.Unlock()
	if br != nil {
		br.Close()
	}
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
