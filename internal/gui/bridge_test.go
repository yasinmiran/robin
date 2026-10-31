package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/web"
)

// fakeAgent is the bridge's view of an agent connection: what the test
// feeds it comes out of Recv, what the bridge sends lands in sent.
type fakeAgent struct {
	in   chan protocol.Message
	sent chan protocol.Message
	err  error // Recv's error once in is closed
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{in: make(chan protocol.Message, 16), sent: make(chan protocol.Message, 64), err: io.EOF}
}

func (a *fakeAgent) Send(m protocol.Message) error { a.sent <- m; return nil }

func (a *fakeAgent) Recv(ctx context.Context) (protocol.Message, error) {
	select {
	case m, ok := <-a.in:
		if !ok {
			return protocol.Message{}, a.err
		}
		return m, nil
	case <-ctx.Done():
		return protocol.Message{}, ctx.Err()
	}
}

// page is a browser tab on the websocket.
type page struct {
	t *testing.T
	c *websocket.Conn
}

func testServer(t *testing.T) (*web.Server, context.Context) {
	t.Helper()
	s, err := web.Listen(web.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	return s, ctx
}

func openPage(t *testing.T, s *web.Server) *page {
	t.Helper()
	pg, err := dialPage(t, s)
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

// reopenPage opens a page once the server has let the previous one go,
// which follows its close by a moment.
func reopenPage(t *testing.T, s *web.Server) *page {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pg, err := dialPage(t, s)
		if err == nil {
			return pg
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dialPage(t *testing.T, s *web.Server) (*page, error) {
	url := s.URL()
	host := strings.TrimPrefix(url[:strings.Index(url, "/#")], "http://")
	token := url[strings.Index(url, "#")+1:]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+host+"/ws", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": {"http://" + host}},
		Subprotocols: []string{"robin", "token." + token},
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { c.CloseNow() })
	return &page{t: t, c: c}, nil
}

func (p *page) send(m protocol.Message) {
	p.t.Helper()
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.c.Write(ctx, websocket.MessageText, b); err != nil {
		p.t.Fatal(err)
	}
}

func (p *page) recv() protocol.Message {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := p.c.Read(ctx)
	if err != nil {
		p.t.Fatalf("page read: %v", err)
	}
	var m protocol.Message
	if err := json.Unmarshal(b, &m); err != nil {
		p.t.Fatal(err)
	}
	return m
}

func (p *page) closed() {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := p.c.Read(ctx); err == nil {
		p.t.Fatal("the page's websocket is still open")
	}
}

// testBridge serves pages to a bridge built with first as its agent
// connection, as Run builds it, so a page that connects before run begins
// has an agent to signal.
func testBridge(t *testing.T, first signaling) (*bridge, *web.Server, context.Context) {
	t.Helper()
	s, ctx := testServer(t)
	b := newBridge(first, relay.New(relay.Policy{Transport: relay.Direct}),
		protocol.Config{ICEServers: []protocol.ICEServer{}, Keep: 60}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go b.accept(ctx, s)
	t.Cleanup(b.close)
	return b, s, ctx
}

func runBridge(ctx context.Context, b *bridge, agent signaling) (<-chan error, chan<- struct{}) {
	result := make(chan error, 1)
	sshDone := make(chan struct{})
	go func() { result <- b.run(ctx, agent, sshDone, make(chan error)) }()
	return result, sshDone
}

// sent is the next message the bridge sent to a.
func sent(t *testing.T, a *fakeAgent) protocol.Message {
	t.Helper()
	select {
	case m := <-a.sent:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the agent")
		return protocol.Message{}
	}
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
		return nil
	}
}

// TestBridgeTellsTheTunnelEachAnswerFirst: the tunnel learns an answer's
// ICE credentials before the page sees the answer, since the browser checks
// through the tunnel with them as soon as it has it; an answer the relay
// rejects teaches it nothing.
func TestBridgeTellsTheTunnelEachAnswerFirst(t *testing.T) {
	agent := newFakeAgent()
	b, s, ctx := testBridge(t, agent)
	answers := make(chan string, 4)
	b.answered = func(sdp string) { answers <- sdp }
	result, _ := runBridge(ctx, b, agent)
	pg := openPage(t, s)
	pg.recv()

	sdp := iceAnswer("agentufrag", "agentpasswordagentpasswo")
	agent.in <- protocol.Message{Type: protocol.TypeAnswer, SDP: sdp}
	if m := pg.recv(); m.Type != protocol.TypeAnswer {
		t.Fatalf("page got %+v, want the answer", m)
	}
	select {
	case got := <-answers:
		if got != sdp {
			t.Fatalf("the tunnel got %q", got)
		}
	default:
		t.Fatal("the page had the answer before the tunnel")
	}

	agent.in <- protocol.Message{Type: protocol.TypeAnswer, SDP: strings.Replace(sdp, "AB:CD", "EF:01", 1)}
	if err := waitResult(t, result); !errors.Is(err, relay.ErrFingerprintChanged) {
		t.Fatalf("run: %v, want the fingerprint change", err)
	}
	if len(answers) > 0 {
		t.Fatal("the tunnel took the credentials of an answer the relay refused")
	}
}

func TestBridgeRelaysAndSurvivesAnAgentConnection(t *testing.T) {
	agent := newFakeAgent()
	b, s, ctx := testBridge(t, agent)
	result, _ := runBridge(ctx, b, agent)

	pg := openPage(t, s)
	if m := pg.recv(); m.Type != protocol.TypeConfig || m.Config.Keep != 60 {
		t.Fatalf("first message to the page %+v, want config with keep", m)
	}
	pg.send(protocol.Message{Type: protocol.TypeOffer, SDP: "v=0"})
	if m := sent(t, agent); m.Type != protocol.TypeOffer || m.SDP != "v=0" {
		t.Fatalf("agent got %+v", m)
	}
	agent.in <- protocol.Message{Type: protocol.TypeError, Error: "offer rejected"}
	if m := pg.recv(); m.Type != protocol.TypeError {
		t.Fatalf("page got %+v, want the agent's error relayed", m)
	}
	agent.in <- protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}}
	if m := pg.recv(); m.Type != protocol.TypeCandidate {
		t.Fatalf("page got %+v, want the end-of-candidates marker", m)
	}

	// The agent's channel closes without a word (the rejected offer was
	// not its last): the connection, not the session, is what ended. The
	// page stays and is told.
	close(agent.in)
	if err := waitResult(t, result); !errors.Is(err, errAgentGone) {
		t.Fatalf("run: %v, want errAgentGone", err)
	}
	b.reconnecting()
	if m := pg.recv(); m.Type != protocol.TypeReconnecting {
		t.Fatalf("page got %+v, want reconnecting", m)
	}
	// What the page sends meanwhile has nowhere to go; it is dropped, or
	// reaches the next agent if that comes first, never an old one.
	pg.send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}})

	// The next agent connection: the page gets a fresh Config and builds
	// its peer connection again.
	agent2 := newFakeAgent()
	result, sshDone := runBridge(ctx, b, agent2)
	if m := pg.recv(); m.Type != protocol.TypeConfig {
		t.Fatalf("page got %+v after the resume, want config", m)
	}
	pg.send(protocol.Message{Type: protocol.TypeOffer, SDP: "v=1"})
	for m := sent(t, agent2); m.SDP != "v=1"; m = sent(t, agent2) {
		if m.Type != protocol.TypeCandidate {
			t.Fatalf("second agent got %+v", m)
		}
	}
	if len(agent.sent) != 0 {
		t.Fatalf("the old agent got %+v", <-agent.sent)
	}
	close(sshDone)
	if err := waitResult(t, result); !errors.Is(err, errSSHLost) {
		t.Fatalf("run: %v, want errSSHLost", err)
	}
	// A page arriving while the CLI redials waits for the next Config.
	b.reconnecting()
	pg.c.CloseNow()
	late := reopenPage(t, s)
	if m := late.recv(); m.Type != protocol.TypeReconnecting {
		t.Fatalf("late page got %+v, want reconnecting", m)
	}
	// The old page's hangup has no agent to go to.
	agent3 := newFakeAgent()
	result, _ = runBridge(ctx, b, agent3)
	if m := late.recv(); m.Type != protocol.TypeConfig {
		t.Fatalf("late page got %+v, want config", m)
	}
	// The host ends the session on purpose: no redial.
	agent3.in <- protocol.Message{Type: protocol.TypeEnded, Error: "the keep window ran out"}
	if err := waitResult(t, result); !errors.Is(err, errEnded) || !strings.Contains(err.Error(), "keep window") {
		t.Fatalf("run: %v, want errEnded", err)
	}
	b.close()
	late.closed()
}

// TestBridgeGivesAnEarlyPageOneConfig: a page that connects before run
// begins gets its Config on arrival, and run sends it no second one, which
// would land in the middle of the page's signaling.
func TestBridgeGivesAnEarlyPageOneConfig(t *testing.T) {
	agent := newFakeAgent()
	b, s, ctx := testBridge(t, agent)
	pg := openPage(t, s)
	if m := pg.recv(); m.Type != protocol.TypeConfig {
		t.Fatalf("first message to the page %+v, want config", m)
	}
	runBridge(ctx, b, agent)
	agent.in <- protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}}
	if m := pg.recv(); m.Type != protocol.TypeCandidate {
		t.Fatalf("page got %+v once run began, want the agent's candidate", m)
	}
}

func TestBridgeTakesThePagesDecisions(t *testing.T) {
	agent := newFakeAgent()
	b, s, ctx := testBridge(t, agent)
	result, _ := runBridge(ctx, b, agent)
	pg := openPage(t, s)
	pg.recv() // config
	pg.send(protocol.Message{Type: protocol.TypeDetach})
	if err := waitResult(t, result); !errors.Is(err, errDetached) {
		t.Fatalf("run: %v, want errDetached", err)
	}
	if len(agent.sent) != 0 {
		t.Fatalf("detach reached the agent: %+v", <-agent.sent)
	}

	// End session: the bridge reports the wish and forwards nothing; the
	// CLI sends the bye itself and waits for the host's answer, so the
	// page's socket stays until the host confirmed.
	agent2 := newFakeAgent()
	agent2.sent = make(chan protocol.Message) // unbuffered: a forwarded bye would block
	b.reconnecting()
	pg.recv() // reconnecting
	result, _ = runBridge(ctx, b, agent2)
	pg.recv() // config again
	pg.send(protocol.Message{Type: protocol.TypeBye})
	if err := waitResult(t, result); !errors.Is(err, errPageEnded) {
		t.Fatalf("run: %v, want errPageEnded", err)
	}
	select {
	case m := <-agent2.sent:
		t.Fatalf("the bridge forwarded %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	b.ended("the CLI ended the session")
	if m := pg.recv(); m.Type != protocol.TypeEnded {
		t.Fatalf("page got %+v, want ended", m)
	}
}

func TestBridgeRunOwnsTheAgentReader(t *testing.T) {
	agent := newFakeAgent()
	b, s, ctx := testBridge(t, agent)
	result, _ := runBridge(ctx, b, agent)
	pg := openPage(t, s)
	pg.recv() // config
	pg.send(protocol.Message{Type: protocol.TypeDetach})
	if err := waitResult(t, result); !errors.Is(err, errDetached) {
		t.Fatalf("run: %v, want errDetached", err)
	}
	// Once run returned, what the agent says next is the caller's to read.
	agent.in <- protocol.Message{Type: protocol.TypeEnded, Error: "the CLI ended the session"}
	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if m, err := agent.Recv(rctx); err != nil || m.Type != protocol.TypeEnded {
		t.Fatalf("the bridge took the agent's message after run returned: %+v, %v", m, err)
	}
	// Decisions that pile up with no run reading them never block the page.
	for i := 0; i < 5; i++ {
		pg.send(protocol.Message{Type: protocol.TypeDetach})
	}
	pg.send(protocol.Message{Type: protocol.TypeOffer, SDP: "v=0"})
	pg.c.CloseNow()
	next := reopenPage(t, s)
	if m := next.recv(); m.Type != protocol.TypeConfig {
		t.Fatalf("next page got %+v, want config", m)
	}
}

func TestBridgeReportsTheAgentsLastWord(t *testing.T) {
	agent := newFakeAgent()
	b, _, ctx := testBridge(t, agent)
	result, _ := runBridge(ctx, b, agent)
	agent.in <- protocol.Message{Type: protocol.TypeError, Error: "desktop went away: Xvfb exited"}
	close(agent.in)
	err := waitResult(t, result)
	var ae *agentError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "Xvfb exited") || resumable(err) {
		t.Fatalf("run: %v, want the agent's error, not resumable", err)
	}
	if !resumable(errSSHLost) || !resumable(errAgentGone) || resumable(errEnded) || resumable(errDetached) {
		t.Error("resumable picks the wrong errors")
	}
}

func TestBridgeHangsUpWhenThePageLeaves(t *testing.T) {
	agent := newFakeAgent()
	b, s, ctx := testBridge(t, agent)
	runBridge(ctx, b, agent)
	pg := openPage(t, s)
	pg.recv()
	pg.c.Close(websocket.StatusNormalClosure, "")
	select {
	case m := <-agent.sent:
		if m.Type != protocol.TypeHangup {
			t.Fatalf("agent got %+v, want hangup", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no hangup reached the agent")
	}
	// The next page's config comes after the hangup.
	next := openPage(t, s)
	if m := next.recv(); m.Type != protocol.TypeConfig {
		t.Fatalf("next page got %+v", m)
	}
}

// TestBridgeDropsAnOfferTooLongForTheAgent: a page frame within the
// websocket's limit can grow sixfold when the bridge encodes it again, every
// '<' becoming `\u003c`, six bytes for one, past the line the agent reads.
// The bridge drops it rather than write a line that would end the agent's
// connection.
func TestBridgeDropsAnOfferTooLongForTheAgent(t *testing.T) {
	s, ctx := testServer(t)
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	cli := protocol.NewConn(fromAgentR, toAgentW, 0, 0)
	agent := protocol.NewConn(toAgentR, fromAgentW, 0, 0)
	t.Cleanup(func() {
		cli.Close()
		agent.Close()
		toAgentR.Close()
		fromAgentR.Close()
	})
	b := newBridge(cli, relay.New(relay.Policy{Transport: relay.Direct}),
		protocol.Config{ICEServers: []protocol.ICEServer{}}, nil, slog.New(slog.DiscardHandler))
	go b.accept(ctx, s)
	t.Cleanup(b.close)

	got := make(chan protocol.Message, 4)
	failed := make(chan error, 1)
	go func() {
		for {
			m, err := agent.Recv(context.Background())
			if err != nil {
				failed <- err
				return
			}
			got <- m
		}
	}()

	pg := openPage(t, s)
	pg.recv()
	// Written by hand: json.Marshal would escape the '<'s already.
	frame := `{"type":"offer","sdp":"` + strings.Repeat("<", 900<<10) + `"}`
	if err := pg.c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	pg.send(protocol.Message{Type: protocol.TypeOffer, SDP: "v=0"})
	select {
	case m := <-got:
		if m.Type != protocol.TypeOffer || m.SDP != "v=0" {
			t.Fatalf("the agent got %s of %d bytes, want the small offer", m.Type, len(m.SDP))
		}
	case err := <-failed:
		t.Fatalf("the agent's connection failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("nothing reached the agent")
	}
}

// TestBridgeKeepsTheBrowsersCandidatesFromTheAgentUnderTunnel: with
// --transport tunnel the page hears none of the agent's UDP candidates, but
// the agent still gathers UDP and checks every address the page names, and
// a browser that exposes its host addresses (Chrome once a page has the
// microphone or the camera, or with mDNS obfuscation off) then connects
// over UDP, which outranks the tunnel pair. The tunnel's connection reaches
// the agent peer-reflexive and needs no candidate of the browser's, so
// under Tunnel none may reach the agent: not in the offer, not trickled.
func TestBridgeKeepsTheBrowsersCandidatesFromTheAgentUnderTunnel(t *testing.T) {
	agent := newFakeAgent()
	s, ctx := testServer(t)
	b := newBridge(agent, relay.New(relay.Policy{Transport: relay.Tunnel, TunnelPort: 4242}),
		protocol.Config{ICEServers: []protocol.ICEServer{}, Keep: 60}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go b.accept(ctx, s)
	t.Cleanup(b.close)
	result, _ := runBridge(ctx, b, agent)

	pg := openPage(t, s)
	if m := pg.recv(); m.Type != protocol.TypeConfig {
		t.Fatalf("first message to the page %+v, want config", m)
	}
	pg.send(protocol.Message{Type: protocol.TypeOffer, SDP: "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=mid:0\r\n" +
		"a=candidate:1 1 udp 2130706431 192.0.2.10 5000 typ host\r\na=end-of-candidates\r\n"})
	pg.send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{Candidate: "candidate:2 1 udp 2130706431 192.0.2.10 5001 typ host"}})
	pg.send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}})
	// The page's messages reach the agent in order; bye is the last of
	// them, and run returns once it has been read.
	pg.send(protocol.Message{Type: protocol.TypeBye})
	if err := waitResult(t, result); !errors.Is(err, errPageEnded) {
		t.Fatalf("run: %v, want errPageEnded", err)
	}
	for len(agent.sent) > 0 {
		m := <-agent.sent
		switch {
		case m.Type == protocol.TypeOffer && strings.Contains(m.SDP, "a=candidate:"):
			t.Errorf("the agent got the page's offer with its candidates:\n%s", m.SDP)
		case m.Type == protocol.TypeCandidate && m.Candidate != nil && m.Candidate.Candidate != "":
			t.Errorf("the agent got the page's candidate %q", m.Candidate.Candidate)
		}
	}
}
