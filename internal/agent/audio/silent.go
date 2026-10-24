//go:build linux

package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

// RoomStep is the keeper's kind for a RoomUndo.
const RoomStep = "silent-room"

// RoomUndo gives the room its speakers back: each stream moved into the
// silent sink goes back to the sink it came from, and the silent sink goes.
type RoomUndo struct {
	Socket string `json:"socket"`
	Cookie []byte `json:"cookie,omitempty"`
	Sink   string `json:"sink"`
	// Moved holds each stream (sink input) moved into Sink, by index.
	Moved map[uint32]Moved `json:"moved,omitempty"`
}

// Moved is a stream the room moved: the sink it was playing on, and what
// tells it apart from a stream that has taken its index since, as PipeWire
// reuses them.
type Moved struct {
	From   string `json:"from"`
	Client uint32 `json:"client"`
	PID    string `json:"pid,omitempty"`    // application.process.id
	Serial string `json:"serial,omitempty"` // object.serial, which PipeWire sets
}

func movedFrom(in *proto.GetSinkInputInfoReply, sink string) Moved {
	return Moved{From: sink, Client: in.ClientIndex, PID: prop(in, "application.process.id"), Serial: prop(in, "object.serial")}
}

// is reports whether in is the stream m was.
func (m Moved) is(in *proto.GetSinkInputInfoReply) bool {
	return m == movedFrom(in, m.From)
}

func prop(in *proto.GetSinkInputInfoReply, key string) string {
	if v, ok := in.Properties[key]; ok {
		return v.String()
	}
	return ""
}

// RoomOptions say whose speakers to keep quiet.
type RoomOptions struct {
	// Socket and Cookie reach the user's sound server; empty finds them
	// where libpulse would.
	Socket string
	Cookie []byte
	// Hold is handed a RoomUndo for RoomStep before each change the room
	// makes, and a change it refuses is not made.
	Hold func(kind string, undo any) error
	Log  *slog.Logger

	// module makes the sink a module even under PipeWire with pw-record.
	module bool
}

// roomDescription is what the user's sound settings call the silent sink.
const roomDescription = "Robin silent room"

// sinkPrefix starts the name of every silent sink, robin_silent_<pid>.
const sinkPrefix = "robin_silent_"

// Room keeps a desktop's speakers quiet while its sound goes on. It adds a
// sink that plays nowhere and moves every playback stream into it, as the
// stream appears or whenever something moves it out again, so the session
// records the sound from that sink's monitor and the room hears none of it.
// The default sink stays what it was. A stream in another session's silent
// sink plays nowhere already and is left there; one that refuses to move is
// reported by Leak.
//
// Under PipeWire the sink is a pw-record child in its own process group with
// Pdeathsig, so it goes with the agent even if nothing else is left to
// remove it. Elsewhere, or without pw-record, it is a module-null-sink.
type Room struct {
	log  *slog.Logger
	hold func(kind string, undo any) error
	at   RoomUndo // Socket, Cookie and Sink

	c       *proto.Client
	conn    net.Conn
	events  net.Conn
	index   uint32 // the silent sink's
	monitor string
	child   *exec.Cmd     // owns the sink under PipeWire; nil for a module's
	exited  chan struct{} // closed when the child exits; nil without one
	stderr  tail

	pending  chan sinkInputEvent
	rescan   atomic.Bool
	wake     chan struct{}
	gone     chan struct{}
	goneErr  error
	changed  chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	mu     sync.Mutex
	moved  map[uint32]Moved
	stuck  map[uint32]string // streams that refuse to move, by index, with their program's name
	closed bool
}

type sinkInputEvent struct {
	index   uint32
	removed bool
}

// Silence starts keeping the room quiet. ctx bounds the start only.
func Silence(ctx context.Context, o RoomOptions) (_ *Room, err error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if o.Socket == "" {
		o.Socket, o.Cookie = userSocket(), userCookie()
	}
	r := &Room{
		log: log, hold: o.Hold, moved: map[uint32]Moved{}, stuck: map[uint32]string{},
		at:      RoomUndo{Socket: o.Socket, Cookie: o.Cookie, Sink: fmt.Sprintf("%s%d", sinkPrefix, os.Getpid())},
		pending: make(chan sinkInputEvent, 256), wake: make(chan struct{}, 1), changed: make(chan struct{}, 1),
		gone: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if r.c, r.conn, err = connect(context.Background(), o.Socket, o.Cookie, nil); err != nil {
		return nil, fmt.Errorf("silent room: reach the sound server at %s: %w", o.Socket, err)
	}
	defer func() {
		if err != nil {
			giveBack(r.c, r.undo())
			r.stopChild()
			r.conn.Close()
			if r.events != nil {
				r.events.Close()
			}
		}
	}()
	var info proto.GetServerInfoReply
	if err := r.c.Request(&proto.GetServerInfo{}, &info); err != nil {
		return nil, fmt.Errorf("silent room: %w", err)
	}
	if err := r.keep(r.undo()); err != nil {
		return nil, fmt.Errorf("silent room: %w", err)
	}
	if err := r.addSink(ctx, strings.Contains(info.PackageName, "PipeWire"), o.module); err != nil {
		return nil, fmt.Errorf("silent room: %w", err)
	}
	if r.events, err = r.subscribe(); err != nil {
		return nil, fmt.Errorf("silent room: %w", err)
	}
	r.scan()
	go r.run()
	where := "module-null-sink"
	if r.child != nil {
		where = "pw-record"
	}
	log.Info("silent room up", "sink", r.at.Sink, "from", where, "moved", len(r.moved))
	return r, nil
}

// Monitor is the source the session records while the room is silent.
func (r *Room) Monitor() string { return r.monitor }

// Gone is closed when the silent sink goes away underneath the room, which
// sends its streams back to the room's speakers.
func (r *Room) Gone() <-chan struct{} { return r.gone }

// Leak says what the room still hears while it is silent: the programs
// whose streams refuse to move, or nil.
func (r *Room) Leak() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stuck) == 0 {
		return nil
	}
	var names []string
	for _, name := range r.stuck {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 1 {
		return fmt.Errorf("a program refuses to move and still plays at the host: %s", names[0])
	}
	return fmt.Errorf("%d programs refuse to move and still play at the host: %s", len(names), strings.Join(names, ", "))
}

// Changed is signalled whenever Leak changes.
func (r *Room) Changed() <-chan struct{} { return r.changed }

func (r *Room) leakChanged() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// Close moves every stream back where it came from and removes the sink.
// It is safe to call more than once.
func (r *Room) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.stopWatching()
	r.mu.Lock()
	u := r.undo()
	r.mu.Unlock()
	err := giveBack(r.c, u)
	r.abandon()
	return err
}

// stopWatching stops moving streams into the silent sink.
func (r *Room) stopWatching() {
	r.stopOnce.Do(func() {
		close(r.stop)
		r.events.Close()
		<-r.done
	})
}

// abandon stops the room without giving anything back, as the agent dying
// would, which leaves that to the keeper.
func (r *Room) abandon() {
	r.stopWatching()
	r.stopChild()
	r.conn.Close()
}

// addSink adds the silent sink and waits for the server to list it.
func (r *Room) addSink(ctx context.Context, pipewire, module bool) error {
	if bin, err := exec.LookPath("pw-record"); pipewire && !module && err == nil {
		props := fmt.Sprintf(`{ node.name=%s node.description="%s" media.class=Audio/Sink audio.position=[ FL FR ] }`, r.at.Sink, roomDescription)
		r.child = exec.Command(bin, "--properties="+props, fmt.Sprint("--rate=", opus.SampleRate), fmt.Sprint("--channels=", opus.Channels), os.DevNull)
		r.child.Stderr = &r.stderr
		r.child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		if err := r.child.Start(); err != nil {
			r.child = nil
			return fmt.Errorf("start pw-record: %w", err)
		}
		r.exited = make(chan struct{})
		go func() {
			r.child.Wait()
			close(r.exited)
		}()
	} else {
		args := fmt.Sprintf("sink_name=%s sink_properties=\"device.description='%s'\" rate=%d channels=%d", r.at.Sink, roomDescription, opus.SampleRate, opus.Channels)
		if !pipewire {
			// The monitor of a null sink that rewinds delivers a fraction of
			// real time in 20 ms fragments; see the private server.
			args += " norewinds=1"
		}
		if err := r.c.Request(&proto.LoadModule{Name: "module-null-sink", Args: args}, &proto.LoadModuleReply{}); err != nil {
			return fmt.Errorf("load module-null-sink: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, serverTimeout)
	defer cancel()
	for {
		var sink proto.GetSinkInfoReply
		err := r.c.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined, SinkName: r.at.Sink}, &sink)
		if err == nil {
			r.index, r.monitor = sink.SinkIndex, sink.MonitorSourceName
			return nil
		}
		select {
		case <-r.exited:
			return fmt.Errorf("pw-record exited: %s", strings.TrimSpace(r.stderr.String()))
		case <-ctx.Done():
			return fmt.Errorf("the silent sink never appeared: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stopChild ends the pw-record that holds the silent sink, if one does.
// giveBack removes a module's.
func (r *Room) stopChild() {
	if r.child == nil {
		return
	}
	pgid := r.child.Process.Pid
	syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-r.exited:
	case <-time.After(termGrace):
		syscall.Kill(-pgid, syscall.SIGKILL)
		<-r.exited
	}
}

// subscribe opens the connection that hears about playback streams. Its
// callback runs on that connection's read goroutine and only queues; the
// requests go out on r.c from run.
func (r *Room) subscribe() (net.Conn, error) {
	c, conn, err := connect(context.Background(), r.at.Socket, r.at.Cookie, func(msg any) {
		switch m := msg.(type) {
		case *proto.SubscribeEvent:
			switch m.Event.GetFacility() {
			case proto.EventSinkSinkInput:
				ev := sinkInputEvent{index: m.Index, removed: m.Event.GetType() == proto.EventRemove}
				select {
				case r.pending <- ev:
				default:
					r.rescan.Store(true)
				}
			case proto.EventSink:
				if m.Index == r.index && m.Event.GetType() == proto.EventRemove {
					r.rescan.Store(true)
				}
			default:
				return
			}
		case *proto.ConnectionClosed:
			r.rescan.Store(true)
		default:
			return
		}
		select {
		case r.wake <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return nil, err
	}
	if err := c.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskSinkInput | proto.SubscriptionMaskSink}, nil); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// run moves streams into the silent sink as they appear, until Close.
func (r *Room) run() {
	defer close(r.done)
	for {
		select {
		case <-r.stop:
			return
		case <-r.exited:
			r.lose(fmt.Errorf("pw-record, which holds the silent sink, exited: %s", strings.TrimSpace(r.stderr.String())))
			<-r.stop
			return
		case ev := <-r.pending:
			r.look(ev)
		case <-r.wake:
		}
		if r.rescan.Swap(false) {
			r.scan()
		}
		select {
		case <-r.gone:
			<-r.stop
			return
		default:
		}
	}
}

// look takes one stream that appeared or changed into the silent sink.
func (r *Room) look(ev sinkInputEvent) {
	if ev.removed {
		r.forget(ev.index)
		return
	}
	var in proto.GetSinkInputInfoReply
	if err := r.c.Request(&proto.GetSinkInputInfo{SinkInputIndex: ev.index}, &in); err != nil {
		r.forget(ev.index)
		return
	}
	r.take(&in)
}

// scan takes every stream not in the silent sink, and forgets the ones that
// ended, as after missed events.
func (r *Room) scan() {
	var sink proto.GetSinkInfoReply
	if err := r.c.Request(&proto.GetSinkInfo{SinkIndex: r.index}, &sink); err != nil || sink.SinkName != r.at.Sink {
		r.lose(errors.New("the silent sink went away"))
		return
	}
	var list proto.GetSinkInputInfoListReply
	if err := r.c.Request(&proto.GetSinkInputInfoList{}, &list); err != nil {
		r.log.Warn("silent room: list playback streams", "err", err)
		return
	}
	live := map[uint32]bool{}
	for _, in := range list {
		live[in.SinkInputIndex] = true
		r.take(in)
	}
	r.mu.Lock()
	ended := len(r.stuck)
	maps.DeleteFunc(r.moved, func(idx uint32, _ Moved) bool { return !live[idx] })
	maps.DeleteFunc(r.stuck, func(idx uint32, _ string) bool { return !live[idx] })
	ended -= len(r.stuck)
	r.mu.Unlock()
	if ended > 0 {
		r.leakChanged()
	}
}

// take moves a stream into the silent sink unless it is there already,
// telling the keeper where it came from first. A stream in another
// session's silent sink plays nowhere already, and is left there.
func (r *Room) take(in *proto.GetSinkInputInfoReply) {
	idx := in.SinkInputIndex
	r.mu.Lock()
	_, stuck := r.stuck[idx]
	r.mu.Unlock()
	if in.SinkIndex == r.index || stuck {
		return
	}
	var from proto.GetSinkInfoReply
	if err := r.c.Request(&proto.GetSinkInfo{SinkIndex: in.SinkIndex}, &from); err != nil {
		r.log.Debug("silent room: the sink of a stream", "stream", idx, "err", err)
		return
	}
	if strings.HasPrefix(from.SinkName, sinkPrefix) {
		return
	}
	r.mu.Lock()
	before, had := r.moved[idx]
	r.moved[idx] = movedFrom(in, from.SinkName)
	u := r.undo()
	r.mu.Unlock()
	putBack := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if had {
			r.moved[idx] = before
		} else {
			delete(r.moved, idx)
		}
	}
	if err := r.keep(u); err != nil {
		r.log.Warn("silent room: the room hears a stream nothing could move back", "stream", idx, "err", err)
		putBack()
		return
	}
	err := r.c.Request(&proto.MoveSinkInput{SinkInputIndex: idx, DeviceIndex: r.index}, nil)
	if err == nil {
		return
	}
	var now proto.GetSinkInputInfoReply
	if r.c.Request(&proto.GetSinkInputInfo{SinkInputIndex: idx}, &now) != nil {
		r.log.Debug("silent room: a stream ended as it moved", "stream", idx, "err", err)
		return // its removal forgets it
	}
	app := prop(&now, "application.name")
	r.log.Warn("silent room: a program refuses to move and still plays at the host", "stream", idx, "app", app, "err", err)
	putBack()
	r.mu.Lock()
	r.stuck[idx] = app
	u = r.undo()
	r.mu.Unlock()
	if err := r.keep(u); err != nil {
		r.log.Debug("silent room: forget a stream", "err", err)
	}
	r.leakChanged()
}

func (r *Room) forget(idx uint32) {
	r.mu.Lock()
	_, had := r.moved[idx]
	_, stuck := r.stuck[idx]
	delete(r.moved, idx)
	delete(r.stuck, idx)
	u := r.undo()
	r.mu.Unlock()
	if had {
		if err := r.keep(u); err != nil {
			r.log.Debug("silent room: forget a stream", "err", err)
		}
	}
	if stuck {
		r.leakChanged()
	}
}

// keep hands the keeper u.
func (r *Room) keep(u RoomUndo) error { return r.hold(RoomStep, u) }

func (r *Room) lose(err error) {
	select {
	case <-r.gone:
	default:
		r.goneErr = err
		r.log.Warn("silent room: the room hears its speakers again", "err", err)
		close(r.gone)
	}
}

// Err says why the room is no longer silent, once Gone is closed.
func (r *Room) Err() error {
	select {
	case <-r.gone:
		return r.goneErr
	default:
		return nil
	}
}

// undo is what gives the room back its speakers now. r.mu must be held
// once the room runs.
func (r *Room) undo() RoomUndo {
	u := r.at
	u.Moved = maps.Clone(r.moved)
	return u
}

// giveBack moves each stream in u that still plays, and is still the stream
// u says, back to the sink it came from, where that sink still exists, and
// then unloads the silent sink if a module made it. While the silent sink
// is there, only the streams in it go back: one moved out of it since is
// not the room's to move. Moving a stream back to the default sink is also
// what clears the target WirePlumber 0.4 remembers for its program.
func giveBack(c *proto.Client, u RoomUndo) error {
	var sink proto.GetSinkInfoReply
	up := c.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined, SinkName: u.Sink}, &sink) == nil
	var errs []error
	for idx, m := range u.Moved {
		var in proto.GetSinkInputInfoReply
		if c.Request(&proto.GetSinkInputInfo{SinkInputIndex: idx}, &in) != nil || !m.is(&in) {
			continue
		}
		if up && in.SinkIndex != sink.SinkIndex {
			continue
		}
		var to proto.GetSinkInfoReply
		if c.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined, SinkName: m.From}, &to) != nil {
			continue
		}
		if err := c.Request(&proto.MoveSinkInput{SinkInputIndex: idx, DeviceIndex: to.SinkIndex}, nil); err != nil {
			errs = append(errs, fmt.Errorf("silent room: move stream %d back to %s: %w", idx, m.From, err))
		}
	}
	if up && sink.ModuleIndex != proto.Undefined {
		var m proto.GetModuleInfoReply
		if c.Request(&proto.GetModuleInfo{ModuleIndex: sink.ModuleIndex}, &m) == nil &&
			m.ModuleName == "module-null-sink" && strings.Contains(m.ModuleArgs, "sink_name="+u.Sink) {
			if err := c.Request(&proto.UnloadModule{ModuleIndex: sink.ModuleIndex}, nil); err != nil {
				errs = append(errs, fmt.Errorf("silent room: remove the silent sink: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// UndoRoom is the keeper's undo for a RoomUndo. ctx's deadline holds on
// its connection to the sound server.
func UndoRoom(ctx context.Context, data json.RawMessage) error {
	var u RoomUndo
	if err := json.Unmarshal(data, &u); err != nil {
		return err
	}
	c, conn, err := connect(ctx, u.Socket, u.Cookie, nil)
	if err != nil {
		return fmt.Errorf("silent room: reach the sound server at %s: %w", u.Socket, err)
	}
	defer conn.Close()
	return giveBack(c, u)
}
