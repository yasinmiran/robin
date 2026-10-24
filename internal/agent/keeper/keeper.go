//go:build linux

// Package keeper undoes what a session changed on an attached desktop, even
// when the agent is killed. The agent runs the keeper as a child and, before
// each change it makes, tells it how to undo that change. The keeper undoes
// whatever it still holds when its stdin closes or a signal ends it, the
// SIGTERM the kernel sends when the agent dies among them.
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

// Serve reads steps from r until it ends or ctx does, then undoes every step
// still held, the newest first. A step that cannot be undone is logged and
// the rest still run.
func Serve(ctx context.Context, r io.Reader, undo Undo, log *slog.Logger) {
	h := &held{steps: map[string]Step{}}
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), maxStep)
		for sc.Scan() {
			if err := h.apply(sc.Bytes()); err != nil {
				log.Warn("keeper: bad step", "err", err)
			}
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
		return nil
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

// Client is the agent's end of a keeper. It starts the keeper with the
// first Hold, so a session that changes nothing runs none, and starts it
// again, handing it everything still held, should it have died.
type Client struct {
	argv []string
	env  []string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
	lines  map[string][]byte // what the keeper holds, as sent
	order  []string
	closed bool
}

// New returns a client for the keeper that argv starts, with env added to
// the agent's environment.
func New(argv, env []string) *Client {
	return &Client{argv: argv, env: env, lines: map[string][]byte{}}
}

// Hold tells the keeper how to undo the change id: data, for the undo of
// kind. It replaces whatever the keeper held for id. An error means nothing
// would undo the change, so the caller must not make it.
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
	if _, ok := c.lines[id]; !ok {
		c.order = append(c.order, id)
	}
	c.lines[id] = line
	if err := c.send(line); err != nil {
		delete(c.lines, id)
		c.order = slices.DeleteFunc(c.order, func(o string) bool { return o == id })
		return err
	}
	return nil
}

// Drop tells the keeper the change id has been undone.
func (c *Client) Drop(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.lines[id]; !ok {
		return nil
	}
	delete(c.lines, id)
	c.order = slices.DeleteFunc(c.order, func(o string) bool { return o == id })
	if c.cmd == nil || c.dead() {
		return nil
	}
	line, _ := json.Marshal(Step{ID: id})
	return c.write(line)
}

// Close ends the keeper, which first undoes anything still held, and waits
// for it to exit.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	cmd, exited := c.cmd, c.exited
	if cmd != nil {
		c.stdin.Close()
	}
	c.mu.Unlock()
	if cmd == nil {
		return nil
	}
	select {
	case <-exited:
		return nil
	case <-time.After(closeWait):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		return errors.New("keeper: did not finish undoing in time")
	}
}

// send writes line to a running keeper, starting one, and handing it
// every other step held, when there is none. c.mu must be held.
func (c *Client) send(line []byte) error {
	if c.cmd != nil && !c.dead() {
		return c.write(line)
	}
	if err := c.start(); err != nil {
		return err
	}
	for _, id := range c.order {
		if err := c.write(c.lines[id]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) dead() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

func (c *Client) write(line []byte) error {
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("keeper: %w", err)
	}
	return nil
}

// start runs the keeper in a process group of its own, so nothing aimed at
// the agent's group reaches it, with SIGTERM as its parent-death signal.
// c.mu must be held.
func (c *Client) start() error {
	if len(c.argv) == 0 {
		return errors.New("keeper: this agent has no keeper to undo its changes")
	}
	cmd := exec.Command(c.argv[0], c.argv[1:]...)
	cmd.Env = append(os.Environ(), c.env...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("keeper: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("keeper: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	c.cmd, c.stdin, c.exited = cmd, stdin, exited
	return nil
}
