//go:build linux

package audio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/robin/internal/agent/keeper"
)

// These make the test binary the silent room's keeper, or an agent that
// silences the room and waits to be killed.
const (
	keeperEnv = "ROBIN_AUDIO_TEST_KEEPER"
	roomEnv   = "ROBIN_AUDIO_TEST_ROOM" // a roomSpec, as JSON
)

type roomSpec struct {
	Socket string
	Cookie []byte
	Module bool
}

func serveKeeper() {
	keeper.Main(keeper.Undo{RoomStep: UndoRoom}, quiet())
}

func silenceForTest(spec string) {
	var o roomSpec
	if err := json.Unmarshal([]byte(spec), &o); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	exe, _ := os.Executable()
	k := keeper.New([]string{exe, "-test.run=^$"}, []string{keeperEnv + "=1", roomEnv + "="})
	r, err := Silence(context.Background(), RoomOptions{
		Socket: o.Socket, Cookie: o.Cookie, module: o.Module, Log: quiet(),
		Hold: func(kind string, u any) error { return k.Hold("silent", kind, u) },
	})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("silenced", r.Monitor())
	select {}
}

// soundServer is the user's sound server as the room's tests see it: where
// it listens, and the sink the room hears.
type soundServer struct {
	socket string
	cookie []byte
	env    []string // for paplay
	room   string
	stop   func() // ends a server the test started
}

// pulseRoom starts a pulseaudio that stands in for the user's, playing into
// a null sink called robin. Unlike the agent's own it loads modules when
// asked, as a desktop's does, and extra goes on its command line.
func pulseRoom(t *testing.T, extra ...string) soundServer {
	t.Helper()
	needServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := startServer(ctx, t.TempDir(), append([]string{"--disallow-module-loading=no"}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cookie, err := os.ReadFile(s.cookie)
	if err != nil {
		t.Fatal(err)
	}
	return soundServer{socket: s.socket, cookie: cookie, env: s.Env(), room: "robin", stop: func() { s.Close() }}
}

// pipeWireRoom is the test session's PipeWire, through pipewire-pulse, whose
// speakers are the null sink robin_speakers (wltest's session.sh).
func pipeWireRoom(t *testing.T) soundServer {
	t.Helper()
	if os.Getenv("ROBIN_PIPEWIRE_PULSE") == "" {
		t.Skip("needs the user's PipeWire with pipewire-pulse; run internal/agent/wayland/testdata/wltest.sh")
	}
	return soundServer{socket: userSocket(), cookie: userCookie(), room: "robin_speakers"}
}

func (s soundServer) client(t *testing.T) *proto.Client {
	t.Helper()
	c, conn, err := connect(context.Background(), s.socket, s.cookie, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return c
}

// sinkOf is the sink app's stream plays on, or "" while it has none.
func sinkOf(t *testing.T, c *proto.Client, app string) string {
	t.Helper()
	var list proto.GetSinkInputInfoListReply
	if err := c.Request(&proto.GetSinkInputInfoList{}, &list); err != nil {
		t.Fatal(err)
	}
	for _, in := range list {
		if name, ok := in.Properties["application.name"]; ok && name.String() == app {
			var sink proto.GetSinkInfoReply
			if c.Request(&proto.GetSinkInfo{SinkIndex: in.SinkIndex}, &sink) == nil {
				return sink.SinkName
			}
		}
	}
	return ""
}

func streamOf(t *testing.T, c *proto.Client, app string) uint32 {
	t.Helper()
	var list proto.GetSinkInputInfoListReply
	if err := c.Request(&proto.GetSinkInputInfoList{}, &list); err != nil {
		t.Fatal(err)
	}
	for _, in := range list {
		if name, ok := in.Properties["application.name"]; ok && name.String() == app {
			return in.SinkInputIndex
		}
	}
	t.Fatalf("no stream from %s", app)
	return 0
}

func waitSink(t *testing.T, c *proto.Client, app, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := sinkOf(t, c, app)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s plays on %q, want %q", app, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sinkExists(c *proto.Client, name string) bool {
	var sink proto.GetSinkInfoReply
	return c.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined, SinkName: name}, &sink) == nil
}

func defaultSink(t *testing.T, c *proto.Client) string {
	t.Helper()
	var info proto.GetServerInfoReply
	if err := c.Request(&proto.GetServerInfo{}, &info); err != nil {
		t.Fatal(err)
	}
	return info.DefaultSinkName
}

// ear records a source: the room sink's monitor when source is "".
type ear struct {
	mu     sync.Mutex
	frames [][]int16
	rec    *recorder
}

func listenTo(t *testing.T, s soundServer, source string) *ear {
	t.Helper()
	e := &ear{}
	rec, err := record(s.socket, s.cookie, func(pcm []int16) {
		e.mu.Lock()
		e.frames = append(e.frames, append([]int16(nil), pcm...))
		e.mu.Unlock()
	}, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.close)
	if rec.sink != s.room {
		t.Fatalf("the default sink is %s, want the room's %s", rec.sink, s.room)
	}
	if source != "" {
		if err := rec.follow(source); err != nil {
			t.Fatal(err)
		}
	}
	e.rec = rec
	return e
}

// reset forgets what was heard so far.
func (e *ear) reset() {
	e.mu.Lock()
	e.frames = nil
	e.mu.Unlock()
}

// loudest is the highest RMS of a frame heard over d.
func (e *ear) loudest(d time.Duration) float64 {
	e.reset()
	time.Sleep(d)
	return e.peak()
}

// peak is the highest RMS of a frame heard since the last reset.
func (e *ear) peak() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	peak := 0.0
	for _, f := range e.frames {
		peak = max(peak, rms(f))
	}
	return peak
}

// tone waits for a second of sound and returns it.
func (e *ear) tone(t *testing.T) []int16 {
	t.Helper()
	e.reset()
	deadline := time.Now().Add(8 * time.Second)
	for {
		e.mu.Lock()
		var heard []int16
		for _, f := range e.frames {
			if rms(f) > 2000 {
				heard = append(heard, f...)
			}
		}
		e.mu.Unlock()
		if len(heard) >= 48000*2 {
			return heard
		}
		if time.Now().After(deadline) {
			t.Fatalf("heard %d samples of the tone, want a second's worth", len(heard)/2)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holder keeps what a room hands its keeper.
type holder struct {
	mu   sync.Mutex
	last RoomUndo
	n    int
	err  error
}

func (h *holder) hold(kind string, undo any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	u, ok := undo.(RoomUndo)
	if kind != RoomStep || !ok {
		return errors.New("not a RoomUndo for " + RoomStep)
	}
	h.last, h.n = u, h.n+1
	return h.err
}

func (h *holder) undo() RoomUndo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

func (h *holder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// silentRoom runs the whole of a silent room against s: a tone played on
// the desktop reaches the room's monitor, the room's sink hears none of it,
// and closing gives the room its sound back as it was.
func silentRoom(t *testing.T, s soundServer, module bool) {
	c := s.client(t)
	before := defaultSink(t, c)
	room := listenTo(t, s, "")
	var h holder
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: h.hold, Log: quiet(), module: module})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !sinkExists(c, r.at.Sink) || r.Monitor() == "" {
		t.Fatalf("no silent sink %s with a monitor (%q)", r.at.Sink, r.Monitor())
	}
	if (r.child != nil) == module {
		t.Errorf("the silent sink's owner: child %v, asked for a module %v", r.child != nil, module)
	}

	stream := listenTo(t, s, r.Monitor())
	room.reset()
	playAs(t, "robin-test-tone", s.env, sine(30, 523, 784))
	waitSink(t, c, "robin-test-tone", r.at.Sink)
	if u := h.undo(); u.Moved[streamOf(t, c, "robin-test-tone")].From != s.room || u.Sink != r.at.Sink {
		t.Fatalf("the keeper holds %+v, want the tone's stream from %s", u, s.room)
	}
	checkTone(t, stream.tone(t), 523, 784)
	if peak := room.loudest(1500 * time.Millisecond); peak > 50 {
		t.Fatalf("the room's sink heard RMS %.0f while silent", peak)
	}

	// Something at the host moves the stream back to the speakers, as a
	// sound settings panel would: the room takes it again.
	if err := c.Request(&proto.MoveSinkInput{SinkInputIndex: streamOf(t, c, "robin-test-tone"), DeviceIndex: proto.Undefined, DeviceName: s.room}, nil); err != nil {
		t.Fatal(err)
	}
	waitSink(t, c, "robin-test-tone", r.at.Sink)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	waitSink(t, c, "robin-test-tone", s.room)
	checkTone(t, room.tone(t), 523, 784)
	if sinkExists(c, r.at.Sink) {
		t.Error("the silent sink outlived Close")
	}
	if got := defaultSink(t, c); got != before {
		t.Errorf("the default sink is %s after the silent room, was %s", got, before)
	}
	if err := r.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

func TestSilentRoomPulseAudio(t *testing.T) {
	silentRoom(t, pulseRoom(t), true)
}

// A desktop's pulseaudio remembers where each program's stream was moved
// (module-stream-restore). Moving a stream back to the default sink forgets
// that again, so a program the room silenced still follows the default.
func TestSilentRoomLeavesPulseAudioNoPreference(t *testing.T) {
	s := pulseRoom(t, "--load=module-stream-restore", "--load=module-null-sink sink_name=other")
	c := s.client(t)
	stop := playAs(t, "robin-test-pref", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-pref", s.room)
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: (&holder{}).hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	waitSink(t, c, "robin-test-pref", r.at.Sink)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	waitSink(t, c, "robin-test-pref", s.room)
	stop()

	if err := c.Request(&proto.SetDefaultSink{SinkName: "other"}, nil); err != nil {
		t.Fatal(err)
	}
	playAs(t, "robin-test-pref", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-pref", "other")
}

// A stream that ends while the room is silent is no longer the keeper's to
// move back; one the keeper cannot hold stays in the room, where nothing
// would have to move it back.
func TestSilentRoomKeepsTheKeeperCurrent(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	var h holder
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: h.hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	stop := playAs(t, "robin-test-ends", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-ends", r.at.Sink)
	stop()
	waitFor(t, "the keeper to forget the stream", func() bool { return len(h.undo().Moved) == 0 })

	h.mu.Lock()
	h.err = errors.New("the keeper died")
	h.mu.Unlock()
	tried := h.count()
	playAs(t, "robin-test-unheld", s.env, sine(30, 440, 660))
	// The room hands the keeper the stream before it moves it, so once it
	// has tried, the stream stays where it is or it moved.
	waitFor(t, "the room to try to hold the new stream", func() bool { return h.count() > tried })
	if got := sinkOf(t, c, "robin-test-unheld"); got != s.room {
		t.Fatalf("a stream the keeper could not hold went to %s", got)
	}
}

// A silent output removed underneath the room ends it.
func TestSilentRoomWhenItsOutputGoes(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: (&holder{}).hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Err() != nil {
		t.Fatalf("a live room reports %v", r.Err())
	}
	var sink proto.GetSinkInfoReply
	if err := c.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined, SinkName: r.at.Sink}, &sink); err != nil {
		t.Fatal(err)
	}
	if err := c.Request(&proto.UnloadModule{ModuleIndex: sink.ModuleIndex}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Gone():
	case <-time.After(5 * time.Second):
		t.Fatal("the room did not notice its output go")
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "went away") {
		t.Fatalf("the room says %v", err)
	}
}

// A server that loads no modules, as the agent's own private one, has no
// room to silence.
func TestSilentRoomNeedsAServerThatLoadsModules(t *testing.T) {
	s := serve(t)
	cookie, err := os.ReadFile(s.cookie)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: cookie, Hold: (&holder{}).hold}); err == nil || !strings.Contains(err.Error(), "module-null-sink") {
		t.Fatalf("Silence on a server that loads no modules: %v", err)
	}
}

// A sound server that goes away ends the room too.
func TestSilentRoomWhenTheServerGoes(t *testing.T) {
	s := pulseRoom(t)
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: (&holder{}).hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	s.stop()
	select {
	case <-r.Gone():
	case <-time.After(5 * time.Second):
		t.Fatal("the room did not notice its sound server go")
	}
	if r.Err() == nil {
		t.Error("a room whose server went says nothing of it")
	}
	r.Close()
}

// The keeper's undo in this process, after a room that was never closed.
func TestUndoRoomAfterTheRoomIsGone(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	var h holder
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: h.hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	playAs(t, "robin-test-undo", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-undo", r.at.Sink)
	r.abandon()

	data, _ := json.Marshal(h.undo())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := UndoRoom(ctx, data); err != nil {
		t.Fatal(err)
	}
	waitSink(t, c, "robin-test-undo", s.room)
	if sinkExists(c, r.at.Sink) {
		t.Error("the silent output outlived the keeper's undo")
	}
	if err := UndoRoom(ctx, []byte("{")); err == nil {
		t.Error("UndoRoom took broken data")
	}
	if err := UndoRoom(ctx, []byte(`{"socket":"/nonexistent"}`)); err == nil {
		t.Error("UndoRoom reached a sound server that is not there")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := UndoRoom(expired, data); err == nil {
		t.Error("UndoRoom ran past its deadline")
	}
}

// An undo that no longer fits moves nothing it does not own: a stream that
// took a moved stream's index since, or one moved out of the silent sink by
// someone else while that sink is there.
func TestUndoRoomMovesOnlyWhatItMoved(t *testing.T) {
	s := pulseRoom(t, "--load=module-null-sink sink_name=other", "--load=module-null-sink sink_name="+sinkPrefix+"1")
	c := s.client(t)
	playAs(t, "robin-test-other", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-other", s.room)
	idx := streamOf(t, c, "robin-test-other")
	var in proto.GetSinkInputInfoReply
	if err := c.Request(&proto.GetSinkInputInfo{SinkInputIndex: idx}, &in); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	someoneElse := movedFrom(&in, "other")
	someoneElse.Client++
	data, _ := json.Marshal(RoomUndo{Socket: s.socket, Cookie: s.cookie, Sink: sinkPrefix + "0", Moved: map[uint32]Moved{idx: someoneElse}})
	if err := UndoRoom(ctx, data); err != nil {
		t.Fatal(err)
	}
	if got := sinkOf(t, c, "robin-test-other"); got != s.room {
		t.Fatalf("an undo for another stream at its index moved it to %s", got)
	}

	data, _ = json.Marshal(RoomUndo{Socket: s.socket, Cookie: s.cookie, Sink: sinkPrefix + "1", Moved: map[uint32]Moved{idx: movedFrom(&in, "other")}})
	if err := UndoRoom(ctx, data); err != nil {
		t.Fatal(err)
	}
	if got := sinkOf(t, c, "robin-test-other"); got != s.room {
		t.Fatalf("an undo moved a stream outside its silent sink to %s", got)
	}
	if sinkExists(c, sinkPrefix+"1") {
		t.Error("the undo left its silent sink")
	}
}

// Two sessions silencing one desktop leave each other's streams alone,
// rather than moving them back and forth for ever.
func TestSilentRoomsLeaveEachOthersStreams(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: (&holder{}).hold, Log: quiet(), module: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	theirs := fmt.Sprintf("%s%d", sinkPrefix, startRoomAgent(t, s, true).Process.Pid)

	playAs(t, "robin-test-both", s.env, sine(30, 440, 660))
	waitFor(t, "one of the rooms to take the stream", func() bool {
		got := sinkOf(t, c, "robin-test-both")
		return got == r.at.Sink || got == theirs
	})
	var changes atomic.Int32
	ec, events, err := connect(context.Background(), s.socket, s.cookie, func(msg any) {
		if m, ok := msg.(*proto.SubscribeEvent); ok && m.Event.GetFacility() == proto.EventSinkSinkInput && m.Event.GetType() == proto.EventChange {
			changes.Add(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	if err := ec.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskSinkInput}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if n := changes.Load(); n > 10 {
		t.Fatalf("the stream changed %d times in a second between two silent rooms", n)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

// A stream that plays before the room goes silent is taken too.
func TestSilentRoomTakesWhatAlreadyPlays(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	playAs(t, "robin-test-early", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-early", s.room)
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: (&holder{}).hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	waitSink(t, c, "robin-test-early", r.at.Sink)
}

// A program that will not have its stream moved still plays at the host,
// and the room says so until it stops.
func TestSilentRoomSaysWhatRefusesToMove(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	var h holder
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: h.hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	changed := func(what string) {
		t.Helper()
		select {
		case <-r.Changed():
		case <-time.After(5 * time.Second):
			t.Fatalf("the room said nothing of %s", what)
		}
	}
	stop := stayingStream(t, s, "robin-test-stays")
	changed("a stream that refuses to move")
	if err := r.Leak(); err == nil || !strings.Contains(err.Error(), "robin-test-stays") {
		t.Fatalf("with a stream that refuses to move the room says %v", err)
	}
	if got := sinkOf(t, c, "robin-test-stays"); got != s.room {
		t.Fatalf("a stream that refuses to move plays on %s", got)
	}
	if len(h.undo().Moved) != 0 {
		t.Errorf("the keeper would move back a stream the room never moved: %+v", h.undo().Moved)
	}
	stop()
	changed("the stream ending")
	if err := r.Leak(); err != nil {
		t.Fatalf("with the stream gone the room says %v", err)
	}
}

// stayingStream opens a playback stream as app that the server must not
// move, as PA_STREAM_DONT_MOVE asks, and closes it with stop. It plays
// nothing: corked, it sits on the default sink all the same.
func stayingStream(t *testing.T, s soundServer, app string) (stop func()) {
	t.Helper()
	c, conn, err := connect(context.Background(), s.socket, s.cookie, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := c.Request(&proto.CreatePlaybackStream{
		SampleSpec:            proto.SampleSpec{Format: proto.FormatInt16LE, Channels: 2, Rate: 48000},
		ChannelMap:            proto.ChannelMap{proto.ChannelLeft, proto.ChannelRight},
		ChannelVolumes:        proto.ChannelVolumes{proto.VolumeNorm, proto.VolumeNorm},
		SinkIndex:             proto.Undefined,
		BufferMaxLength:       proto.Undefined,
		BufferTargetLength:    proto.Undefined,
		BufferPrebufferLength: proto.Undefined,
		BufferMinimumRequest:  proto.Undefined,
		Corked:                true,
		NoMove:                true,
		Properties:            proto.PropList{"application.name": proto.PropListString(app), "media.name": proto.PropListString(app)},
	}, &proto.CreatePlaybackStreamReply{}); err != nil {
		t.Fatal(err)
	}
	return func() { conn.Close() }
}

func TestSilentRoomNeedsItsKeeper(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	h := holder{err: errors.New("no keeper")}
	if _, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: h.hold, Log: quiet()}); err == nil {
		t.Fatal("Silence succeeded with nothing to undo it")
	}
	if sinkExists(c, fmt.Sprintf("%s%d", sinkPrefix, os.Getpid())) {
		t.Error("a refused silent room left its sink")
	}
	if _, err := Silence(context.Background(), RoomOptions{Socket: filepath.Join(t.TempDir(), "none"), Hold: h.hold, Log: quiet()}); err == nil {
		t.Error("Silence reached a sound server that is not there")
	}
}

// The point of the keeper: an agent killed with SIGKILL, which runs none of
// its own cleanup, still leaves the room its speakers.
func keeperGivesTheRoomBack(t *testing.T, s soundServer, module bool) {
	c := s.client(t)
	agent := startRoomAgent(t, s, module)
	sink := fmt.Sprintf("%s%d", sinkPrefix, agent.Process.Pid)
	playAs(t, "robin-test-kill", s.env, sine(30, 523, 784))
	waitSink(t, c, "robin-test-kill", sink)

	agent.Process.Kill()
	agent.Wait()
	waitSink(t, c, "robin-test-kill", s.room)
	deadline := time.Now().Add(5 * time.Second)
	for sinkExists(c, sink) {
		if time.Now().After(deadline) {
			t.Fatal("the silent sink outlived the agent")
		}
		time.Sleep(50 * time.Millisecond)
	}
	checkTone(t, listenTo(t, s, "").tone(t), 523, 784)
}

// startRoomAgent runs an agent that silences s's room, with a keeper, until
// it is killed.
func startRoomAgent(t *testing.T, s soundServer, module bool) *exec.Cmd {
	t.Helper()
	spec, _ := json.Marshal(roomSpec{Socket: s.socket, Cookie: s.cookie, Module: module})
	exe, _ := os.Executable()
	agent := exec.Command(exe, "-test.run=^$")
	agent.Env = append(os.Environ(), roomEnv+"="+string(spec))
	agent.Stderr = os.Stderr
	out, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agent.Process.Kill()
		agent.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "silenced ") {
		t.Fatalf("agent said %q, %v", line, err)
	}
	return agent
}

func TestSilentRoomKeeperAfterSIGKILL(t *testing.T) {
	keeperGivesTheRoomBack(t, pulseRoom(t), true)
}

// Under PipeWire, pw-record owns the silent sink where the host has it;
// GNOME 46's image does, the Fedora ones do not, and every one can take a
// module through pipewire-pulse.
func TestSilentRoomPipeWire(t *testing.T) {
	s := pipeWireRoom(t)
	if _, err := exec.LookPath("pw-record"); err == nil {
		t.Run("child", func(t *testing.T) { silentRoom(t, s, false); checkWirePlumberState(t) })
	}
	t.Run("module", func(t *testing.T) { silentRoom(t, s, true); checkWirePlumberState(t) })
}

// pw-record dying takes the silent output with it, which ends the room and
// sends its streams back to the room's speakers.
func TestSilentRoomPipeWireWhenPwRecordDies(t *testing.T) {
	s := pipeWireRoom(t)
	if _, err := exec.LookPath("pw-record"); err != nil {
		t.Skip("no pw-record here")
	}
	c := s.client(t)
	r, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: (&holder{}).hold, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	playAs(t, "robin-test-pwdies", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-pwdies", r.at.Sink)
	r.child.Process.Kill()
	select {
	case <-r.Gone():
	case <-time.After(5 * time.Second):
		t.Fatal("the room did not notice pw-record die")
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "pw-record") {
		t.Fatalf("the room says %v", err)
	}
	waitSink(t, c, "robin-test-pwdies", s.room)
}

func TestSilentRoomPipeWireKeeperAfterSIGKILL(t *testing.T) {
	s := pipeWireRoom(t)
	if _, err := exec.LookPath("pw-record"); err == nil {
		t.Run("child", func(t *testing.T) { keeperGivesTheRoomBack(t, s, false); checkWirePlumberState(t) })
	}
	t.Run("module", func(t *testing.T) { keeperGivesTheRoomBack(t, s, true); checkWirePlumberState(t) })
}

// checkWirePlumberState fails if WirePlumber remembers an output for a
// program the room moved, or a silent sink for any. WirePlumber 0.4 stores
// the target of every moved stream per program, and forgets it when the
// stream moves to the default sink; 0.5 stores none.
func checkWirePlumberState(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".local", "state", "wireplumber", "restore-stream"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "target=robin_silent") || strings.Contains(line, "application.name:robin-test-") && strings.Contains(line, ":target=") {
			t.Errorf("WirePlumber remembers %s", line)
		}
	}
}
