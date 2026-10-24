//go:build linux

package agent

import (
	"context"
	"sync"
	"unicode/utf8"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/protocol"
)

func init() {
	handleControl(protocol.ControlText, (*session).applyText)
}

// maxLane bounds the input waiting behind text. A user cannot get that far
// ahead of a desktop that keeps up; past it, runs of pointer moves merge
// into their last, and then input is dropped.
const maxLane = 256

func (s *session) applyText(m protocol.Control) {
	if len(m.Text) > protocol.MaxTextBytes || !utf8.ValidString(m.Text) {
		s.log.Warn("text from browser refused", "bytes", len(m.Text), "utf8", utf8.ValidString(m.Text))
		return
	}
	t, ok := s.desk.(desktop.Typist)
	if !ok {
		s.log.Warn("this desktop cannot type text")
		return
	}
	queued := s.typing.queue(func(ctx context.Context) {
		if err := t.Type(ctx, m.Text); err != nil {
			s.log.Debug("type text", "err", err)
		}
	})
	if !queued {
		s.log.Warn("text dropped: typing has fallen behind")
	}
}

// lane applies the browser's input in the order it came. Input normally
// applies at once, on the peer's read loop. Text is the exception: typing it
// can wait on the desktop (X11 lets a spare keycode settle before binding it
// again, Wayland waits for a paste to be read), and while the read loop
// waits every data channel stalls. So text runs on a goroutine of its own,
// and so does whatever input arrives before that goroutine has caught up,
// which would otherwise overtake the text. The zero lane is ready to use.
type lane struct {
	mu     sync.Mutex
	steps  []step
	cancel context.CancelFunc
	idle   chan struct{} // closed when the goroutine stops; nil when there is none
}

// step is one thing the lane applies in turn: text to type, or input of
// type t.
type step struct {
	t  protocol.ControlType
	do func(context.Context)
}

// queue queues f, which types text, for the lane's goroutine. It reports
// false when the lane is full and f was dropped.
func (l *lane) queue(f func(context.Context)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.push(step{protocol.ControlText, f})
}

// run applies input of type t: at once when no text is being typed, and
// otherwise queued behind it. It reports false when the lane is full and f
// was dropped. A release, which lets go of every key and button, is never
// dropped: what a dropped key-up would have let go of must not stay held.
func (l *lane) run(t protocol.ControlType, f func(context.Context)) bool {
	l.mu.Lock()
	if l.idle == nil {
		l.mu.Unlock()
		f(context.Background())
		return true
	}
	defer l.mu.Unlock()
	return l.push(step{t, f})
}

// push queues st, starting the lane's goroutine when there is none. l.mu
// must be held.
func (l *lane) push(st step) bool {
	release := st.t == protocol.ControlRelease
	if release && len(l.steps) > 0 && l.steps[len(l.steps)-1].t == protocol.ControlRelease {
		return true // one release lets go of everything already
	}
	if len(l.steps) >= maxLane && !release {
		l.mergeMoves()
		if len(l.steps) >= maxLane {
			return false
		}
	}
	l.steps = append(l.steps, st)
	if l.idle == nil {
		var ctx context.Context
		ctx, l.cancel = context.WithCancel(context.Background())
		l.idle = make(chan struct{})
		go l.drain(ctx, l.idle)
	}
	return true
}

// mergeMoves keeps only the last of each run of queued pointer moves:
// once the lane has fallen this far behind, where the pointer ends up is
// what counts.
func (l *lane) mergeMoves() {
	kept := l.steps[:0]
	for i, st := range l.steps {
		if st.t == protocol.ControlMove && i+1 < len(l.steps) && l.steps[i+1].t == protocol.ControlMove {
			continue
		}
		kept = append(kept, st)
	}
	clear(l.steps[len(kept):])
	l.steps = kept
}

func (l *lane) drain(ctx context.Context, idle chan struct{}) {
	defer close(idle)
	for {
		l.mu.Lock()
		if ctx.Err() != nil {
			// discard has let this goroutine go.
			l.mu.Unlock()
			return
		}
		if len(l.steps) == 0 {
			l.cancel()
			l.idle = nil
			l.mu.Unlock()
			return
		}
		st := l.steps[0]
		l.steps = l.steps[1:]
		l.mu.Unlock()
		st.do(ctx)
	}
}

// discard drops what is queued, stops the text being typed, and returns
// once the lane's goroutine has stopped: what a browser that left still had
// queued must not type after it.
func (l *lane) discard() {
	l.mu.Lock()
	idle := l.idle
	if idle != nil {
		l.cancel()
	}
	l.steps, l.idle = nil, nil
	l.mu.Unlock()
	if idle != nil {
		<-idle
	}
}
