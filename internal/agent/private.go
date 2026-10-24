//go:build linux

package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agent/keeper"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Hold hands a keeper what undoes a change, for the undo of kind, before
// the change is made; an error means the change must not be made.
type Hold func(kind string, undo any) error

// leaker is a switch of a private session that can fail to hide all it
// should while it is on.
type leaker interface {
	// Leak says what the room can still see or hear, or nil.
	Leak() error
	// Changed is signalled whenever Leak changes.
	Changed() <-chan struct{}
}

// curtain blacks out a desktop's screens; see gamma.Curtain.
type curtain interface {
	Draw() error
	Lift() error
	Close() error
	leaker
}

// Room keeps the room's speakers quiet while the session still hears them;
// see audio.Room.
type Room interface {
	// Monitor is the source the session records while the room is silent.
	Monitor() string
	// Gone is closed when the room is no longer silent, on its own; Err,
	// which is not nil once Gone is closed, then says why.
	Gone() <-chan struct{}
	Err() error
	Close() error
	leaker
}

// SilenceFunc makes the attached desktop's room silent. ctx bounds the
// start only.
type SilenceFunc func(ctx context.Context, hold Hold, log *slog.Logger) (Room, error)

// follower is an AudioSource that can record another source than the one it
// started on: the silent room's monitor, or "" for its own again.
type follower interface {
	Follow(source string) error
}

// Keeper step IDs: one change of each kind at a time.
const (
	curtainStep = "curtain"
	silentStep  = "silent"
)

// roomTimeout bounds making the room silent.
const roomTimeout = 10 * time.Second

// private is an attached desktop's private session, which hides the desktop
// from the room it sits in: the curtain blacks out its screens and the
// silent room keeps its speakers quiet, while the stream still shows and
// plays both. Every change goes through a keeper first, so an agent that is
// killed still gives the room its screens and speakers back.
type private struct {
	log     *slog.Logger
	keeper  *keeper.Client
	curtain curtain // nil when the desktop cannot have one
	silence SilenceFunc
	capture follower // nil when the session records no sound
	send    func()   // tells the page the state

	mu                      sync.Mutex
	state                   protocol.Private
	wantCurtain, wantSilent bool // as the page asked last
	room                    Room

	kick chan struct{}
	// settled hands run a channel to close once it has settled the
	// switches.
	settled chan chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

// openPrivate readies the private session of an attached desktop, with the
// switches Start turns on already on; it returns nil for a spawned one. send
// tells the page the state, from the session's own goroutines.
func openPrivate(d *openedDesktop, start protocol.Start, o Options, audio AudioSource, send func(), log *slog.Logger) (*private, error) {
	if d.mode != protocol.ModeAttach {
		return nil, nil
	}
	p := &private{
		log: log, silence: o.Silence, send: send,
		kick: make(chan struct{}, 1), settled: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	p.capture, _ = audio.(follower)
	if len(o.Keeper) == 0 {
		why := "this agent has no keeper to undo the change if it dies"
		p.state.Curtain.Unsupported, p.state.Silent.Unsupported = why, why
	} else {
		p.keeper = keeper.New(o.Keeper, nil)
		p.keeper.Lost = p.keeperLost
		if d.openCurtain == nil {
			p.state.Curtain.Unsupported = "this desktop cannot be darkened"
		} else if c, err := d.openCurtain(p.hold(curtainStep)); err != nil {
			p.state.Curtain.Unsupported = err.Error()
			log.Info("no curtain", "why", err)
		} else {
			p.curtain = c
		}
		if p.silence == nil {
			p.state.Silent.Unsupported = "this agent was built without sound"
		}
	}
	p.wantCurtain, p.wantSilent = start.Curtain, start.Silent
	p.settle()
	go p.run()
	for _, s := range []struct {
		flag string
		on   bool
		sw   protocol.PrivateSwitch
	}{{"--curtain", start.Curtain, p.state.Curtain}, {"--silent", start.Silent, p.state.Silent}} {
		if s.on && !s.sw.On {
			p.close()
			return nil, fmt.Errorf("%s: %s", s.flag, cmp.Or(s.sw.Unsupported, s.sw.Error))
		}
	}
	return p, nil
}

// hold is the Hold for the change id.
func (p *private) hold(id string) Hold {
	return func(kind string, undo any) error { return p.keeper.Hold(id, kind, undo) }
}

// snapshot is the state for Ready and the page; nil for a spawned desktop.
func (p *private) snapshot() *protocol.Private {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state
	return &st
}

// set asks for a switch from the page. The change happens on the private
// session's own goroutine, since the control handlers run on the peer's
// read loop, which must not wait on an X or sound server.
func (p *private) set(t protocol.ControlType, on bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	switch t {
	case protocol.ControlCurtain:
		p.wantCurtain = on
	case protocol.ControlSilent:
		p.wantSilent = on
	}
	p.mu.Unlock()
	p.wake()
}

// require turns on the switches a resuming CLI asks for, as Start's would
// be, and waits until they are. A switch it cannot turn on refuses the
// resume: the error says why, and both switches go back to what the page
// last asked. A spawned desktop, which has no private session, refuses any.
func (p *private) require(curtain, silent bool) error {
	if !curtain && !silent {
		return nil
	}
	if p == nil {
		return errors.New("--curtain and --silent are for a desktop you attach to; nobody sits in front of one robin starts")
	}
	p.mu.Lock()
	wasCurtain, wasSilent := p.wantCurtain, p.wantSilent
	p.wantCurtain, p.wantSilent = wasCurtain || curtain, wasSilent || silent
	p.mu.Unlock()
	p.settleNow()
	st := p.snapshot()
	var errs []error
	for _, s := range []struct {
		flag string
		on   bool
		sw   protocol.PrivateSwitch
	}{{"--curtain", curtain, st.Curtain}, {"--silent", silent, st.Silent}} {
		if s.on && !s.sw.On {
			errs = append(errs, fmt.Errorf("%s: %s", s.flag, cmp.Or(s.sw.Unsupported, s.sw.Error, "the session ended")))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	p.mu.Lock()
	p.wantCurtain, p.wantSilent = wasCurtain, wasSilent
	p.mu.Unlock()
	p.settleNow()
	return errors.Join(errs...)
}

// settleNow has run settle the switches, and waits until it has or the
// private session closes.
func (p *private) settleNow() {
	done := make(chan struct{})
	select {
	case p.settled <- done:
	case <-p.stop:
		return
	}
	select {
	case <-done:
	case <-p.stop:
	}
}

func (p *private) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *private) run() {
	defer close(p.done)
	var curtainLeak <-chan struct{}
	if p.curtain != nil {
		curtainLeak = p.curtain.Changed()
	}
	for {
		var gone, roomLeak <-chan struct{}
		p.mu.Lock()
		room := p.room
		p.mu.Unlock()
		if room != nil {
			gone, roomLeak = room.Gone(), room.Changed()
		}
		select {
		case <-p.stop:
			return
		case <-p.kick:
			p.settle()
		case done := <-p.settled:
			p.settle()
			close(done)
		case <-gone:
			p.roomGone()
		case <-curtainLeak:
			p.leaked(&p.state.Curtain, p.curtain)
		case <-roomLeak:
			p.leaked(&p.state.Silent, room)
		}
		p.send()
	}
}

// settle turns each switch the way the page last asked, as far as it can.
func (p *private) settle() {
	p.mu.Lock()
	wantCurtain, wantSilent, st := p.wantCurtain, p.wantSilent, p.state
	p.mu.Unlock()
	if wantCurtain != st.Curtain.On && st.Curtain.Unsupported == "" {
		p.switchCurtain(wantCurtain)
	}
	if wantSilent != st.Silent.On && st.Silent.Unsupported == "" {
		p.switchSilent(wantSilent)
	}
}

// leaked tells the page, on sw, what the switch l fails to hide while on.
func (p *private) leaked(sw *protocol.PrivateSwitch, l leaker) {
	err := l.Leak()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !sw.On {
		return
	}
	sw.Error = ""
	if err != nil {
		sw.Error = err.Error()
	}
}

// keeperLost tells the page that nothing would undo what is switched on,
// should the agent die, until the switch is turned again.
func (p *private) keeperLost(err error) {
	p.log.Warn("keeper lost", "err", err)
	why := "nothing would give the room this back if the agent died: " + err.Error()
	p.mu.Lock()
	for _, sw := range []*protocol.PrivateSwitch{&p.state.Curtain, &p.state.Silent} {
		if sw.On {
			sw.Error = why
		}
	}
	p.mu.Unlock()
	p.wake()
}

func (p *private) switchCurtain(on bool) {
	var err error
	if on {
		if err = p.curtain.Draw(); err != nil {
			// Draw put back what it could.
			p.keeper.Drop(curtainStep)
		}
	} else if err = p.curtain.Lift(); err == nil {
		p.keeper.Drop(curtainStep)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Curtain.Error = ""
	if err != nil {
		p.state.Curtain.Error = err.Error()
		p.log.Warn("curtain", "on", on, "err", err)
		return
	}
	p.state.Curtain.On = on
	p.log.Info("curtain", "on", on)
}

func (p *private) switchSilent(on bool) {
	var err error
	if on {
		ctx, cancel := context.WithTimeout(context.Background(), roomTimeout)
		var r Room
		r, err = p.silence(ctx, p.hold(silentStep), p.log)
		cancel()
		if err != nil {
			p.keeper.Drop(silentStep)
		} else {
			p.follow(r.Monitor())
			p.mu.Lock()
			p.room = r
			p.mu.Unlock()
		}
	} else {
		err = p.closeRoom()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Silent.Error = ""
	if err != nil {
		p.state.Silent.Error = err.Error()
		p.log.Warn("silent room", "on", on, "err", err)
		return
	}
	p.state.Silent.On = on
	p.log.Info("silent room", "on", on)
}

// follow points the session's capture at source; a session that cannot
// follow carries on without the room's sound, which the log says.
func (p *private) follow(source string) {
	if p.capture == nil {
		return
	}
	if err := p.capture.Follow(source); err != nil {
		p.log.Warn("silent room: the session cannot record the room's sound", "err", err)
	}
}

// closeRoom gives the room its speakers back, the capture first, since
// closing removes the source it records.
func (p *private) closeRoom() error {
	p.mu.Lock()
	r := p.room
	p.mu.Unlock()
	if r == nil {
		return nil
	}
	p.follow("")
	if err := r.Close(); err != nil {
		return err
	}
	p.keeper.Drop(silentStep)
	p.mu.Lock()
	p.room = nil
	p.mu.Unlock()
	return nil
}

// roomGone takes in a silent room that ended underneath the session.
func (p *private) roomGone() {
	p.mu.Lock()
	r := p.room
	p.mu.Unlock()
	why := r.Err()
	err := p.closeRoom()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Silent.On, p.wantSilent = false, false
	p.state.Silent.Error = "the room is no longer silent"
	if e := errors.Join(why, err); e != nil {
		p.state.Silent.Error = e.Error()
	}
}

// close gives the room back its screens and speakers and ends the keeper,
// which undoes whatever could not be.
func (p *private) close() {
	if p == nil {
		return
	}
	select {
	case <-p.stop:
		return
	default:
	}
	close(p.stop)
	select {
	case <-p.done:
	case <-time.After(roomTimeout + time.Second):
		// A switch still waits on a server; the keeper undoes what it held.
	}
	if p.curtain != nil {
		if err := p.curtain.Close(); err != nil {
			p.log.Warn("curtain: lift", "err", err)
		} else {
			p.keeper.Drop(curtainStep)
		}
	}
	if err := p.closeRoom(); err != nil {
		p.log.Warn("silent room: give the speakers back", "err", err)
	}
	if p.keeper != nil {
		if err := p.keeper.Close(); err != nil {
			p.log.Warn("keeper", "err", err)
		}
	}
}

func init() {
	for _, t := range []protocol.ControlType{protocol.ControlCurtain, protocol.ControlSilent} {
		handleControl(t, func(s *session, m protocol.Control) { s.private.set(m.T, m.On) })
	}
}

// sendPrivate tells the page the private session's state.
func (s *session) sendPrivate() {
	st := s.private.snapshot()
	p := s.currentOrNil()
	if st == nil || p == nil {
		return
	}
	if err := p.SendControl(protocol.Control{T: protocol.ControlPrivate, Private: st}); err != nil && !errors.Is(err, peer.ErrNoControl) {
		s.log.Debug("send private", "err", err)
	}
}
