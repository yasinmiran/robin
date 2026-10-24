//go:build linux && cgo && opus

package audio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

// toneTrack is the browser's microphone playing a tone: Opus packets of
// 20 ms, handed out in real time, then io.EOF.
type toneTrack struct {
	packets [][]byte
	next    int
	start   time.Time
}

func newToneTrack(t *testing.T, hz, seconds float64) *toneTrack {
	t.Helper()
	enc, err := opus.NewEncoder(opus.Bitrate, opus.Complexity)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pcm := make([]int16, opus.FrameSamples*opus.Channels)
	tr := &toneTrack{}
	for f := range int(seconds * 50) {
		for i := range opus.FrameSamples {
			v := int16(9000 * math.Sin(2*math.Pi*hz*float64(f*opus.FrameSamples+i)/opus.SampleRate))
			pcm[2*i], pcm[2*i+1] = v, v
		}
		pkt, err := enc.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		tr.packets = append(tr.packets, pkt)
	}
	return tr
}

func (tr *toneTrack) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if tr.next == 0 {
		tr.start = time.Now()
	}
	if tr.next == len(tr.packets) {
		return nil, nil, io.EOF
	}
	time.Sleep(time.Until(tr.start.Add(time.Duration(tr.next) * 20 * time.Millisecond)))
	n := tr.next
	tr.next++
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: uint16(n), Timestamp: uint32(n * opus.FrameSamples)},
		Payload: tr.packets[n],
	}, nil, nil
}

// playTrack runs m.Play on tr until it ends or the test does.
func playTrack(t *testing.T, m *Mic, tr RTPReader) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- m.Play(tr) }()
	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Errorf("Play: %v", err)
		}
	})
}

// recordSource records seconds of source, mono, the way a program on the
// desktop would, with env added to reach the server.
func recordSource(t *testing.T, env []string, source string, seconds float64) []int16 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, "parec", "-d", source, "--raw", "--format=s16le", "--rate=48000", "--channels=1", "--latency-msec=20")
	cmd.Env = append(os.Environ(), env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		t.Fatalf("parec -d %s: %v: %s", source, err, errs.String())
	}
	pcm := make([]int16, out.Len()/2)
	binary.Read(&out, binary.LittleEndian, pcm)
	return pcm
}

// monoPitch is the frequency of mono pcm, counted in swings past a
// threshold, over the part of it that is not silent.
func monoPitch(pcm []int16) (hz float64, loud int) {
	const threshold = 2000
	swings := 0
	high := false
	for _, v := range pcm {
		if high && v < -threshold || !high && v > threshold {
			high = !high
			swings++
		}
		if v > threshold || v < -threshold {
			loud++
		}
	}
	return float64(swings) / 2 / (float64(len(pcm)) / opus.SampleRate), loud
}

// checkHeard wants most of pcm to be the tone at hz.
func checkHeard(t *testing.T, pcm []int16, hz float64) {
	t.Helper()
	if len(pcm) < opus.SampleRate {
		t.Fatalf("recorded %d samples, want a second's worth", len(pcm))
	}
	got, loud := monoPitch(pcm)
	if loud < len(pcm)/4 {
		t.Fatalf("the microphone was silent: %d of %d samples above the threshold", loud, len(pcm))
	}
	if math.Abs(got-hz) > hz*0.05 {
		t.Errorf("heard %.1f Hz, want %.0f", got, hz)
	}
}

func pactl(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("pactl", args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pactl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestSpawnedMicIsTheDefaultSource sends a tone as the browser would and
// records the private server's default source, as a program on a spawned
// desktop would.
func TestSpawnedMicIsTheDefaultSource(t *testing.T) {
	needServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, env, err := Start(ctx, Options{RunDir: t.TempDir(), Mic: true, NoCapture: true, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	m, err := StartMic(ctx, MicOptions{Source: src, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Where != "the private pulseaudio" {
		t.Errorf("Where = %q", m.Where)
	}
	if info := pactl(t, env, "info"); !strings.Contains(info, "Default Source: robin-mic\n") || !strings.Contains(info, "Default Sink: robin\n") {
		t.Errorf("pactl info:\n%s", info)
	}
	if sources := pactl(t, env, "list", "sources"); !strings.Contains(sources, "Description: "+MicName+"\n") {
		t.Errorf("no source described as %s:\n%s", MicName, sources)
	}

	playTrack(t, m, newToneTrack(t, 440, 4))
	time.Sleep(500 * time.Millisecond)
	checkHeard(t, recordSource(t, env, "@DEFAULT_SOURCE@", 2.5), 440)
}

// TestSpawnedMicKeepsTheSpeakers: with the microphone added the desktop's
// own sound is still what the capture hears.
func TestSpawnedMicKeepsTheSpeakers(t *testing.T) {
	needServer(t)
	src, env, err := Start(context.Background(), Options{RunDir: t.TempDir(), Mic: true, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.rec.sink != "robin" {
		t.Errorf("recording sink %q, want robin", src.rec.sink)
	}
	play(t, env, sine(3, 440, 1000))
	checkTone(t, listen(t, src), 440, 1000)
}

// userServer starts a pulseaudio standing in for the user's own: module
// loading allowed, found through XDG_RUNTIME_DIR as libpulse would.
func userServer(t *testing.T) {
	t.Helper()
	needServer(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pulse"), 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "pulse", "native")
	script := filepath.Join(dir, "user.pa")
	os.WriteFile(script, []byte("load-module module-null-sink sink_name=speakers\nset-default-sink speakers\n"+
		"load-module module-native-protocol-unix socket="+quoteArg(socket)+" auth-anonymous=1\n"), 0o600)
	cmd := exec.Command("pulseaudio", "-n", "--file="+script, "--daemonize=no", "--exit-idle-time=-1",
		"--use-pid-file=no", "--realtime=no", "--high-priority=no", "--log-target=stderr")
	cmd.Env = serverEnv(dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	var log bytes.Buffer
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the user's pulseaudio never opened %s: %s", socket, log.String())
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("PULSE_COOKIE", "")
}

// TestAttachedPulseAudioMic adds the microphone to the user's own
// PulseAudio, where a program records it by name, and takes it away again.
func TestAttachedPulseAudioMic(t *testing.T) {
	userServer(t)
	m, err := StartMic(context.Background(), MicOptions{Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Where != "PulseAudio" {
		t.Errorf("Where = %q", m.Where)
	}
	name := micSourcePrefix + strconv.Itoa(os.Getpid())
	if sources := pactl(t, nil, "list", "sources"); !strings.Contains(sources, "Name: "+name+"\n") ||
		!strings.Contains(sources, "Description: "+MicName+"\n") {
		t.Errorf("no source %s described as %s:\n%s", name, MicName, sources)
	}
	if info := pactl(t, nil, "info"); !strings.Contains(info, "Default Source: speakers.monitor\n") {
		t.Errorf("the user's default source changed:\n%s", info)
	}

	playTrack(t, m, newToneTrack(t, 660, 4))
	time.Sleep(500 * time.Millisecond)
	checkHeard(t, recordSource(t, nil, name, 2.5), 660)

	m.Close()
	if mods := pactl(t, nil, "list", "short", "modules"); strings.Contains(mods, "robin-mic") {
		t.Errorf("Close left the microphone's modules loaded:\n%s", mods)
	}
}

// TestAttachedMicSweepsADeadAgents: an agent killed outright left its
// microphone on the user's server; the next one takes it away.
func TestAttachedMicSweepsADeadAgents(t *testing.T) {
	userServer(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(dead.Process.Pid)
	for _, mod := range micModules(micFeedPrefix+pid, micSourcePrefix+pid) {
		pactl(t, nil, append([]string{"load-module", mod.name}, mod.args)...)
	}
	// One whose agent lives on, as another session of this user would be.
	alive := strconv.Itoa(os.Getppid())
	for _, mod := range micModules(micFeedPrefix+alive, micSourcePrefix+alive) {
		pactl(t, nil, append([]string{"load-module", mod.name}, mod.args)...)
	}

	m, err := StartMic(context.Background(), MicOptions{Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	mods := pactl(t, nil, "list", "short", "modules")
	if strings.Contains(mods, "-"+pid+" ") || strings.Contains(mods, "-"+pid+".") {
		t.Errorf("the dead agent's microphone is still loaded:\n%s", mods)
	}
	for _, want := range []string{micSourcePrefix + alive, micSourcePrefix + strconv.Itoa(os.Getpid())} {
		if !strings.Contains(mods, "source_name="+want+" ") {
			t.Errorf("microphone %s missing:\n%s", want, mods)
		}
	}
}

// TestPipeWireMic adds the microphone to the user's PipeWire as a node of
// a child, which programs record through pipewire-pulse, and which goes
// with the child.
func TestPipeWireMic(t *testing.T) {
	if os.Getenv("ROBIN_PIPEWIRE_PULSE") == "" {
		t.Skip("needs the user's PipeWire with pipewire-pulse; run internal/agent/wayland/testdata/wltest.sh")
	}
	m, err := StartMic(context.Background(), MicOptions{Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Where != "PipeWire" {
		t.Errorf("Where = %q", m.Where)
	}
	name := micSourcePrefix + strconv.Itoa(os.Getpid())
	if sources := pactl(t, nil, "list", "sources"); !strings.Contains(sources, "Name: "+name+"\n") ||
		!strings.Contains(sources, "Description: "+MicName+"\n") {
		t.Errorf("no source %s described as %s:\n%s", name, MicName, sources)
	}

	playTrack(t, m, newToneTrack(t, 550, 4))
	time.Sleep(500 * time.Millisecond)
	checkHeard(t, recordSource(t, nil, name, 2.5), 550)

	m.Close()
	for deadline := time.Now().Add(3 * time.Second); strings.Contains(pactl(t, nil, "list", "short", "sources"), name); {
		if time.Now().After(deadline) {
			t.Fatalf("the microphone's node outlived Close")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMicPlayEndsWithTheTrack(t *testing.T) {
	m := newMic(&fakeSink{}, "test", quiet())
	defer m.Close()
	if err := m.Play(&trackReader{err: io.EOF}); err != nil {
		t.Errorf("Play of an ended track = %v", err)
	}
	broken := errors.New("receiver closed")
	if err := m.Play(&trackReader{err: broken}); !errors.Is(err, broken) {
		t.Errorf("Play = %v, want the track's error", err)
	}
}

// TestMicDecodesWhatArrives: a track's packets reach the sink as 20 ms of
// mono each, a lost one filled in.
func TestMicDecodesWhatArrives(t *testing.T) {
	sink := &fakeSink{}
	m := newMic(sink, "test", quiet())
	tr := newToneTrack(t, 440, 0.2)
	var ps []*rtp.Packet
	for i, p := range tr.packets {
		if i == 4 {
			continue
		}
		ps = append(ps, &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i * opus.FrameSamples)}, Payload: p})
	}
	if err := m.Play(&trackReader{packets: ps, err: io.EOF}); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if n := sink.count(); n != len(tr.packets) {
		t.Fatalf("the sink got %d pieces, want %d", n, len(tr.packets))
	}
	for i, b := range sink.writes {
		if len(b) != micFrameBytes {
			t.Errorf("piece %d is %d bytes, want %d", i, len(b), micFrameBytes)
		}
	}
}

// TestPipeWireMicDiesWithTheAgent kills an agent with SIGKILL, which runs no
// cleanup, and checks its microphone's node goes with it.
func TestPipeWireMicDiesWithTheAgent(t *testing.T) {
	if os.Getenv("ROBIN_PIPEWIRE_PULSE") == "" {
		t.Skip("needs the user's PipeWire with pipewire-pulse; run internal/agent/wayland/testdata/wltest.sh")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), micHelperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready PipeWire\n" {
		cmd.Process.Kill()
		t.Fatalf("helper said %q: %v", line, err)
	}
	name := micSourcePrefix + strconv.Itoa(cmd.Process.Pid)
	if !strings.Contains(pactl(t, nil, "list", "short", "sources"), name) {
		cmd.Process.Kill()
		t.Fatalf("the helper's microphone %s is not there", name)
	}
	cmd.Process.Kill()
	cmd.Wait()
	for deadline := time.Now().Add(3 * time.Second); strings.Contains(pactl(t, nil, "list", "short", "sources"), name); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the microphone %s outlived its agent", name)
		}
	}
}
