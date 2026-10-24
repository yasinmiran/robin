//go:build linux

package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// fakeSink records what a Mic writes, and can be held up to fill the queue.
type fakeSink struct {
	mu      sync.Mutex
	writes  [][]byte
	hold    chan struct{} // writes wait for it to close, when set
	holding chan struct{} // gets one value for each write that waits
	fail    error
	closed  bool
}

func (s *fakeSink) write(b []byte) error {
	if s.hold != nil {
		select {
		case s.holding <- struct{}{}:
		default:
		}
		<-s.hold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, b)
	return s.fail
}

func (s *fakeSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func TestMicQueueDropsWhenTheServerIsBehind(t *testing.T) {
	sink := &fakeSink{hold: make(chan struct{}), holding: make(chan struct{}, 1)}
	m := newMic(sink, "test", nil, quiet())
	m.enqueue([]int16{0, -1})
	<-sink.holding
	for i := 1; i < micQueue+5; i++ {
		m.enqueue([]int16{int16(i), -1})
	}
	// One piece is with the held writer, micQueue wait, the rest are gone.
	m.mu.Lock()
	dropped := m.dropped
	m.mu.Unlock()
	if dropped != 4 {
		t.Errorf("dropped %d pieces, want 4", dropped)
	}
	close(sink.hold)
	m.Close()
	if n := sink.count(); n != micQueue+1 {
		t.Errorf("the sink got %d pieces, want %d", n, micQueue+1)
	}
	if got := sink.writes[1]; !slices.Equal(got, []byte{1, 0, 0xff, 0xff}) {
		t.Errorf("piece 1 is %v, want little-endian 1, -1", got)
	}
	if !sink.closed {
		t.Error("Close left the sink open")
	}
	m.enqueue([]int16{1})
	if m.Close() != nil || sink.count() != micQueue+1 {
		t.Error("a closed microphone took sound, or a second Close failed")
	}
}

// TestMicFillsSilenceBetweenTheBrowsersSound: a sink that needs feeding
// gets 20 ms of silence on each tick while the browser sends nothing, the
// browser's sound as it comes, and silence again only once that has
// stopped for more than micIdleTicks.
func TestMicFillsSilenceBetweenTheBrowsersSound(t *testing.T) {
	sink := &fakeSink{}
	ticks := make(chan time.Time)
	m := newMic(sink, "test", ticks, quiet())
	// A send returns once the writer has taken the tick, so each is dealt
	// with before the test goes on.
	tick := func(n int) {
		for range n {
			ticks <- time.Time{}
		}
	}
	tick(2)
	m.enqueue(slices.Repeat([]int16{0x1234}, micFrameBytes/2))
	waitWrites(t, sink, 3)
	tick(micIdleTicks)
	tick(1)
	m.Close()

	silence := make([]byte, micFrameBytes)
	sound := bytes.Repeat([]byte{0x34, 0x12}, micFrameBytes/2)
	want := [][]byte{silence, silence, sound, silence}
	if !slices.EqualFunc(sink.writes, want, bytes.Equal) {
		t.Errorf("the sink got %s, want silence, silence, the sound, silence", describe(sink.writes))
	}
}

// TestMicWithoutTicksAddsNoSilence: a sink that makes its own silence, as
// PulseAudio does, gets the browser's sound and nothing else.
func TestMicWithoutTicksAddsNoSilence(t *testing.T) {
	sink := &fakeSink{}
	m := newMic(sink, "test", nil, quiet())
	m.enqueue([]int16{0x1234})
	m.Close()
	if !slices.EqualFunc(sink.writes, [][]byte{{0x34, 0x12}}, bytes.Equal) {
		t.Errorf("the sink got %s, want the one piece of sound", describe(sink.writes))
	}
}

func waitWrites(t *testing.T, sink *fakeSink, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); sink.count() < n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the sink got %d writes, want %d", sink.count(), n)
		}
	}
}

// describe says of each write whether it is silence, and how long it is.
func describe(writes [][]byte) string {
	var s []string
	for _, b := range writes {
		kind := "sound"
		if !slices.ContainsFunc(b, func(v byte) bool { return v != 0 }) {
			kind = "silence"
		}
		s = append(s, fmt.Sprintf("%d bytes of %s", len(b), kind))
	}
	return "[" + strings.Join(s, ", ") + "]"
}

func TestMicCarriesOnWhenTheServerRefuses(t *testing.T) {
	sink := &fakeSink{fail: errors.New("stream gone")}
	m := newMic(sink, "test", nil, quiet())
	for range 3 {
		m.enqueue([]int16{1})
	}
	m.Close()
	if sink.count() != 3 {
		t.Errorf("the sink was offered %d pieces, want 3", sink.count())
	}
}

// trackReader is a remote track: its packets, then err.
type trackReader struct {
	packets []*rtp.Packet
	err     error
}

func (r *trackReader) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if len(r.packets) == 0 {
		return nil, nil, r.err
	}
	p := r.packets[0]
	r.packets = r.packets[1:]
	return p, nil, nil
}

func TestMicModules(t *testing.T) {
	ms := micModules("robin-mic-feed-42", "robin-mic-42")
	if len(ms) != 2 || ms[0].name != "module-null-sink" || ms[1].name != "module-remap-source" {
		t.Fatalf("modules %v", ms)
	}
	for _, want := range []string{"sink_name=robin-mic-feed-42 ", "channels=1", "norewinds=1", `device.description="Robin microphone feed"`} {
		if !strings.Contains(ms[0].args, want) {
			t.Errorf("sink args %q lack %q", ms[0].args, want)
		}
	}
	for _, want := range []string{"master=robin-mic-feed-42.monitor ", "source_name=robin-mic-42 ", `source_properties='device.description="Robin microphone"'`} {
		if !strings.Contains(ms[1].args, want) {
			t.Errorf("source args %q lack %q", ms[1].args, want)
		}
	}
	for _, m := range ms {
		if sub := micOwner.FindStringSubmatch(m.args); sub == nil || sub[1] != "42" {
			t.Errorf("micOwner finds %v in %q, want 42", sub, m.args)
		}
	}
}

func TestMicOwnerIgnoresOtherModules(t *testing.T) {
	for _, args := range []string{
		"sink_name=robin_speakers",
		"sink_name=robin-mic-feed rate=48000",
		"source_name=robin-mic channels=1",
		"master=robin-mic-feed-42.monitor source_name=mine",
		"sink_name=robin-mic-feed-42x",
	} {
		if sub := micOwner.FindStringSubmatch(args); sub != nil {
			t.Errorf("micOwner claims %q for an agent: %v", args, sub)
		}
	}
}

func TestServerScriptWithTheMicrophone(t *testing.T) {
	s := serverScript("/s", "/c", true)
	sink := strings.Index(s, "set-default-sink robin\n")
	feed := strings.Index(s, "load-module module-null-sink sink_name=robin-mic-feed ")
	remap := strings.Index(s, "load-module module-remap-source master=robin-mic-feed.monitor source_name=robin-mic ")
	source := strings.Index(s, "set-default-source robin-mic\n")
	if sink < 0 || feed < sink || remap < feed || source < remap {
		t.Errorf("script loads the microphone out of order or not at all:\n%s", s)
	}
	if strings.Contains(serverScript("/s", "/c", false), "robin-mic") {
		t.Error("the microphone is there without being asked for")
	}
}

func TestPipeWireMicPipeline(t *testing.T) {
	args := strings.Join(pipewireMicPipeline("robin-mic-42"), " ")
	for _, want := range []string{
		"fdsrc fd=0 ",
		"rawaudioparse use-sink-caps=false format=pcm pcm-format=s16le sample-rate=48000 num-channels=1 ",
		"queue leaky=downstream",
		"pipewiresink mode=provide sync=false ",
		`stream-properties="props,media.class=Audio/Source,node.name=robin-mic-42,node.description=\"Robin microphone\",node.group=pipewire.dummy"`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("pipeline lacks %q:\n%s", want, args)
		}
	}
}

func TestStartMicNeedsAMicrophoneOnTheServer(t *testing.T) {
	if _, err := StartMic(context.Background(), MicOptions{Source: &Source{}, Log: quiet()}); err == nil {
		t.Error("StartMic took a source with no microphone")
	}
}

func TestStartMicWithoutAServer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	_, err := StartMic(context.Background(), MicOptions{Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "no sound server") {
		t.Fatalf("StartMic = %v, want no sound server", err)
	}
}

// TestStartMicOnAServerThatHangsUp: a socket that accepts and closes is not
// a sound server.
func TestStartMicOnAServerThatHangsUp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if err := os.Mkdir(dir+"/pulse", 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenUnix(t, dir+"/pulse/native")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.CopyN(io.Discard, c, 20)
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := StartMic(ctx, MicOptions{Log: quiet()}); err == nil {
		t.Fatal("StartMic succeeded against a server that hangs up")
	}
}

func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// micHelperEnv makes the test binary act as an agent that puts the
// microphone on the user's sound server and waits to be killed; see
// TestPipeWireMicDiesWithTheAgent.
const micHelperEnv = "ROBIN_MIC_TEST_HELPER"

func runMicHelper() {
	m, err := StartMic(context.Background(), MicOptions{})
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("ready " + m.Where() + "\n")
	select {}
}
