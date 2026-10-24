//go:build linux

// Package keeper undoes what a session changed on an attached desktop, even
// when the agent is killed. The agent runs the keeper as a child and, before
// each change it makes, tells it how to undo that change. The keeper undoes
// whatever it still holds when its stdin closes or a signal ends it, the
// SIGTERM the kernel sends when the agent dies among them.
//
// Steps go to the keeper's stdin as JSON lines. The keeper writes a byte to
// its stdout once it is ready and one for each line it takes, so the agent
// makes no change before a live keeper holds what undoes it.
package keeper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Step is one change to undo. A Step with no Kind ends the one with its ID:
// the agent undid that change itself.
type Step struct {
	ID   string          `json:"id"`
	Kind string          `json:"kind,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Undo maps a step's Kind to what undoes it.
type Undo map[string]func(ctx context.Context, data json.RawMessage) error

// maxStep bounds one line on the keeper's stdin. Gamma ramps for a few
// monitors take tens of KiB.
const maxStep = 1 << 20

var (
	// undoTimeout bounds each undo, so an X or sound server that stopped
	// answering cannot keep the keeper alive.
	undoTimeout = 5 * time.Second
	// drain is how long the keeper goes on reading after a signal, for the
	// steps an agent wrote just before it died.
	drain = 250 * time.Millisecond
)

var ack = []byte{'\n'}

// Main runs this process as a keeper on its stdin and stdout until stdin
// closes or SIGINT, SIGTERM or SIGHUP arrives, and then undoes what it
// holds. It says it is ready only once those signals no longer kill it, so
// the agent's death cannot kill a keeper that holds anything.
func Main(undo Undo, log *slog.Logger) {
	// The agent's SSH channel may be gone by the time there is anything to say.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	acks := os.Stdout
	os.Stdout = os.Stderr // stdout carries the acks and nothing else
	Serve(ctx, os.Stdin, acks, undo, log)
}

// Serve reads steps from r until it ends or ctx does, then undoes every step
// still held, the newest first. It writes a byte to w first, saying it is
// ready, and one for every line it takes. A step that cannot be undone is
// logged and the rest still run.
//
// Once ctx is done Serve returns after a short drain, even while r stays
// open. Its reader then lives on until r ends, so a caller that goes on
// running closes r.
func Serve(ctx context.Context, r io.Reader, w io.Writer, undo Undo, log *slog.Logger) {
	h := &held{steps: map[string]Step{}}
	w.Write(ack)
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), maxStep)
		for sc.Scan() {
			err := h.apply(sc.Bytes())
			if errors.Is(err, errUndoing) {
				continue // no ack: the agent hands the step to a keeper that takes it
			}
			if err != nil {
				log.Warn("keeper: bad step", "err", err)
			}
			w.Write(ack)
		}
		if err := sc.Err(); err != nil {
			log.Warn("keeper: reading steps", "err", err)
		}
	}()
	select {
	case <-eof:
	case <-ctx.Done():
		select {
		case <-eof:
		case <-time.After(drain):
		}
	}
	for _, s := range h.close() {
		f := undo[s.Kind]
		if f == nil {
			log.Warn("keeper: no undo for step", "id", s.ID, "kind", s.Kind)
			continue
		}
		uctx, cancel := context.WithTimeout(context.Background(), undoTimeout)
		err := f(uctx, s.Data)
		cancel()
		if err != nil {
			log.Warn("keeper: undo failed", "id", s.ID, "kind", s.Kind, "err", err)
			continue
		}
		log.Info("keeper: undone", "id", s.ID, "kind", s.Kind)
	}
}

var errUndoing = errors.New("keeper: undoing already")

// held is what the keeper would undo now, in the order the changes were
// first made.
type held struct {
	mu     sync.Mutex
	steps  map[string]Step
	order  []string
	closed bool
}

func (h *held) apply(line []byte) error {
	var s Step
	if err := json.Unmarshal(line, &s); err != nil {
		return err
	}
	if s.ID == "" {
		return errors.New("step without an id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errUndoing
	}
	if s.Kind == "" {
		delete(h.steps, s.ID)
		h.order = slices.DeleteFunc(h.order, func(id string) bool { return id == s.ID })
		return nil
	}
	if _, ok := h.steps[s.ID]; !ok {
		h.order = append(h.order, s.ID)
	}
	h.steps[s.ID] = s
	return nil
}

// close stops taking steps and returns those held, the newest first.
func (h *held) close() []Step {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	var out []Step
	for _, id := range slices.Backward(h.order) {
		out = append(out, h.steps[id])
	}
	return out
}

// closeWait bounds how long Close waits for the keeper to undo what it holds.
const closeWait = 15 * time.Second

// reviveTries is how many times a keeper that died is started again before
// the client gives up and says so.
const reviveTries = 5

const (
	// ackWait bounds how long the client waits for a keeper to say it is
	// ready, or that it took a step.
	ackWait = 5 * time.Second
	// reviveWait is the pause before starting a keeper again after one died,
	// doubled after each start that fails.
	reviveWait = 100 * time.Millisecond
)

// self runs the agent's own executable, which a deploy of another version
// may have removed from its directory since the agent started.
const self = "/proc/self/exe"

// Client is the agent's end of a keeper. It starts the keeper with the
// first Hold, so a session that changes nothing runs none, and starts it
// again, handing it everything still held, whenever one dies. The zero
// Client has no keeper to start and refuses every Hold.
type Client struct {
	argv []string
	env  []string
	// Lost, if set, is told when a keeper that died could not be started
	// again, so nothing would undo what it held should the agent die too.
	// Set it before the first Hold.
	Lost func(error)

	// The constants of the same names, which tests shorten.
	ackWait, reviveWait time.Duration

	mu     sync.Mutex
	p      *proc
	lines  map[string][]byte // what the keeper holds, as sent
	order  []string
	closed bool
}

// New returns a client for a keeper that is the agent's own executable run
// again with argv: argv[0] names the process, and the rest of argv makes it a
// keeper. env is added to the agent's environment.
func New(argv, env []string) *Client {
	return &Client{argv: argv, env: env, ackWait: ackWait, reviveWait: reviveWait}
}

// Hold tells the keeper how to undo the change id: data, for the undo of
// kind. It replaces whatever the keeper held for id, and returns once a live
// keeper holds it. An error means nothing would undo the change, so the
// caller must not make it.
func (c *Client) Hold(id, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	line, err := json.Marshal(Step{ID: id, Kind: kind, Data: raw})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("keeper: closed")
	}
	if c.lines == nil {
		c.lines = map[string][]byte{}
	}
	before, had := c.lines[id]
	if !had {
		c.order = append(c.order, id)
	}
	c.lines[id] = line
	if err := c.send(line); err != nil {
		if had {
			c.lines[id] = before // that change was made, and still needs its undo
		} else {
			delete(c.lines, id)
			c.order = slices.DeleteFunc(c.order, func(o string) bool { return o == id })
		}
		return err
	}
	return nil
}

// Drop tells the keeper the change id has been undone. A keeper that does
// not take it is ended, and the one started after it holds only what is
// left.
func (c *Client) Drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.lines[id]; !ok {
		return
	}
	delete(c.lines, id)
	c.order = slices.DeleteFunc(c.order, func(o string) bool { return o == id })
	if c.p == nil {
		return
	}
	line, _ := json.Marshal(Step{ID: id})
	if c.p.take(line) != nil {
		c.p.kill()
	}
}

// Close ends the keeper, which first undoes anything still held, and waits
// for it to exit. A keeper that died holding steps, before Close or while it
// undid them, is started again to undo them.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.p == nil {
		return nil
	}
	for range 2 {
		if c.p.dead() && len(c.order) > 0 {
			if err := c.restart(); err != nil {
				return err
			}
		}
		if err := c.p.finish(); err != nil {
			return err
		}
		if c.p.cmd.ProcessState.Success() || len(c.order) == 0 {
			return nil
		}
	}
	return errors.New("keeper: killed before it could undo")
}

// send hands line to the keeper, or, when there is none or it does not
// take it, to a new one along with everything else held. c.mu must be held.
func (c *Client) send(line []byte) error {
	if c.p != nil && c.p.take(line) == nil {
		return nil
	}
	return c.restart()
}

// restart replaces the keeper with a new one that holds every step held.
// c.mu must be held.
func (c *Client) restart() error {
	if c.p != nil {
		c.p.kill() // what is left of one that no longer answers
	}
	p, err := c.start()
	if err != nil {
		return err
	}
	c.p = p
	for _, id := range c.order {
		if err := p.take(c.lines[id]); err != nil {
			return err
		}
	}
	return nil
}

// revive starts a keeper again once dead, the one that held c's steps, has
// exited, and tells Lost if it cannot.
func (c *Client) revive(dead *proc) {
	wait := c.reviveWait
	for try := 1; ; try++ {
		time.Sleep(wait)
		c.mu.Lock()
		if c.closed || c.p != dead || len(c.order) == 0 {
			c.mu.Unlock()
			return
		}
		err := c.restart()
		lost := c.Lost
		c.mu.Unlock()
		if err == nil {
			return
		}
		if try == reviveTries {
			if lost != nil {
				lost(err)
			}
			return
		}
		wait *= 2
	}
}

// start runs a keeper in a process group of its own, so nothing aimed at
// the agent's group reaches it, with SIGTERM as its parent-death signal, and
// waits for it to say it is ready. c.mu must be held.
func (c *Client) start() (*proc, error) {
	if len(c.argv) == 0 {
		return nil, errors.New("keeper: this agent has no keeper to undo its changes")
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("keeper: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("keeper: %w", err)
	}
	cmd := &exec.Cmd{
		Path: self, Args: c.argv, Env: append(os.Environ(), c.env...),
		Stdin: inR, Stdout: outW, Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM},
	}
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, fmt.Errorf("keeper: %w", err)
	}
	acks, exited := make(chan struct{}), make(chan struct{})
	p := &proc{cmd: cmd, stdin: inW, acks: acks, exited: exited, wait: c.ackWait}
	go func() {
		defer close(acks)
		defer outR.Close()
		buf := make([]byte, 64)
		for {
			n, err := outR.Read(buf)
			for range n {
				select {
				case acks <- struct{}{}:
				case <-exited:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		cmd.Wait()
		close(exited)
		inW.Close()
		c.revive(p)
	}()
	if err := p.ack(); err != nil {
		p.kill()
		return nil, fmt.Errorf("keeper: it never said it was ready: %w", err)
	}
	return p, nil
}

// proc is one keeper process.
type proc struct {
	cmd    *exec.Cmd
	stdin  *os.File
	acks   <-chan struct{} // one for each byte the keeper writes; closed once it can write no more
	exited <-chan struct{}
	wait   time.Duration // for each ack
}

// take writes line to the keeper and waits for it to say it has it.
func (p *proc) take(line []byte) error {
	p.stdin.SetWriteDeadline(time.Now().Add(p.wait))
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("keeper: %w", err)
	}
	if err := p.ack(); err != nil {
		return fmt.Errorf("keeper: %w", err)
	}
	return nil
}

// ack waits for the keeper's next byte.
func (p *proc) ack() error {
	select {
	case _, ok := <-p.acks:
		if !ok {
			return errors.New("it exited")
		}
		return nil
	case <-time.After(p.wait):
		return errors.New("it stopped answering")
	}
}

func (p *proc) dead() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

func (p *proc) kill() { p.cmd.Process.Kill() }

// finish closes the keeper's stdin, which has it undo what it holds, and
// waits for it to exit.
func (p *proc) finish() error {
	p.stdin.Close()
	select {
	case <-p.exited:
		return nil
	case <-time.After(closeWait):
		p.kill()
		<-p.exited
		return errors.New("keeper: did not finish undoing in time")
	}
}
