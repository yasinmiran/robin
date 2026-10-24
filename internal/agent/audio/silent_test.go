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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	keeper.Serve(ctx, os.Stdin, keeper.Undo{RoomStep: UndoRoom}, quiet())
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
		Hold: func(u RoomUndo) error { return k.Hold("silent", RoomStep, u) },
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
	return soundServer{socket: s.socket, cookie: cookie, env: s.Env(), room: "robin"}
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
	c, conn, err := connect(s.socket, s.cookie, nil)
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

func (h *holder) hold(u RoomUndo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last, h.n = u, h.n+1
	return h.err
}

func (h *holder) undo() RoomUndo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
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
	start := time.Now()
	room.reset()
	playAs(t, "robin-test-tone", s.env, sine(30, 523, 784))
	waitSink(t, c, "robin-test-tone", r.at.Sink)
	took := time.Since(start).Round(time.Millisecond)
	time.Sleep(300 * time.Millisecond) // for what the room heard before the move to reach its monitor
	t.Logf("moved %v after paplay started; the room heard up to RMS %.0f before", took, room.peak())
	if u := h.undo(); u.Moved[streamOf(t, c, "robin-test-tone")] != s.room || u.Sink != r.at.Sink {
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
	playAs(t, "robin-test-unheld", s.env, sine(30, 440, 660))
	waitSink(t, c, "robin-test-unheld", s.room)
	time.Sleep(500 * time.Millisecond)
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
	close(r.stop)
	r.events.Close()
	<-r.done
	r.conn.Close()

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

func TestSilentRoomNeedsItsKeeper(t *testing.T) {
	s := pulseRoom(t)
	c := s.client(t)
	h := holder{err: errors.New("no keeper")}
	if _, err := Silence(context.Background(), RoomOptions{Socket: s.socket, Cookie: s.cookie, Hold: h.hold, Log: quiet()}); err == nil {
		t.Fatal("Silence succeeded with nothing to undo it")
	}
	if sinkExists(c, fmt.Sprintf("robin_silent_%d", os.Getpid())) {
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
	defer agent.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "silenced ") {
		t.Fatalf("agent said %q, %v", line, err)
	}
	sink := fmt.Sprintf("robin_silent_%d", agent.Process.Pid)
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
