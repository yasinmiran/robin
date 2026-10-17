//go:build linux && cgo && opus

package audio

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/remotejump/internal/agent/opus"
)

// listen decodes what src emits until a second of sound has come through, and
// returns that sound.
func listen(t *testing.T, src *Source) []int16 {
	t.Helper()
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	frame := make([]int16, opus.FrameSamples*opus.Channels)
	var heard []int16
	deadline := time.After(8 * time.Second)
	for len(heard) < opus.SampleRate*opus.Channels {
		select {
		case pkt, ok := <-src.Packets():
			if !ok {
				t.Fatal("packets closed before the tone came through")
			}
			if _, err := dec.Decode(pkt, frame); err != nil {
				t.Fatal(err)
			}
			if rms(frame) > 2000 {
				heard = append(heard, frame...)
			}
		case <-deadline:
			t.Fatalf("heard %d samples of the tone, want a second's worth", len(heard)/opus.Channels)
		}
	}
	return heard
}

// pitch is the frequency of one channel of interleaved stereo, counted in
// swings past a threshold so codec noise around zero does not count twice.
func pitch(pcm []int16, ch int) float64 {
	const threshold = 2000
	swings := 0
	high := pcm[ch] > 0
	for i := ch; i < len(pcm); i += opus.Channels {
		if v := pcm[i]; high && v < -threshold || !high && v > threshold {
			high = !high
			swings++
		}
	}
	return float64(swings) / 2 / (float64(len(pcm)/opus.Channels) / opus.SampleRate)
}

func checkTone(t *testing.T, heard []int16, left, right float64) {
	t.Helper()
	for ch, want := range []float64{left, right} {
		if got := pitch(heard, ch); math.Abs(got-want) > want*0.03 {
			t.Errorf("channel %d decodes at %.1f Hz, want %.0f", ch, got, want)
		}
	}
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestSpawnedDesktopIsHeard plays a tone the way a program on a spawned
// desktop would, through the environment Start hands out, and decodes it
// from the packets.
func TestSpawnedDesktopIsHeard(t *testing.T) {
	needServer(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, env, err := Start(ctx, Options{RunDir: dir, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "PULSE_SERVER=unix:"+filepath.Join(dir, "pulse", "native")) || !strings.Contains(joined, "PULSE_COOKIE=") {
		t.Fatalf("env %q does not lead to the private server", env)
	}

	play(t, env, sine(3, 440, 1000))
	checkTone(t, listen(t, src), 440, 1000)

	src.Close()
	for range src.Packets() {
	}
	if _, err := os.Stat(filepath.Join(dir, "pulse")); !os.IsNotExist(err) {
		t.Errorf("the private server's files outlived Close: %v", err)
	}
	src.Close()
}

// TestAttachedDesktopIsHeard records a server Start did not start, found
// where libpulse would look for the user's own, and leaves it running.
func TestAttachedDesktopIsHeard(t *testing.T) {
	user := serve(t)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Dir(user.dir))
	t.Setenv("PULSE_COOKIE", user.cookie)

	src, env, err := Start(context.Background(), Options{RunDir: t.TempDir(), Attached: true, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if len(env) != 0 {
		t.Errorf("attached capture handed out env %q", env)
	}
	play(t, user.Env(), sine(3, 523, 784))
	checkTone(t, listen(t, src), 523, 784)

	src.Close()
	select {
	case <-user.Done():
		t.Fatal("closing an attached capture stopped the user's server")
	default:
	}
}

// TestAttachedPipeWireIsHeard records the user's PipeWire through
// pipewire-pulse, as an attached session does on a current GNOME or Plasma
// desktop. PipeWire takes no cookie, and none exists here.
func TestAttachedPipeWireIsHeard(t *testing.T) {
	if os.Getenv("RJ_PIPEWIRE_PULSE") == "" {
		t.Skip("needs the user's PipeWire with pipewire-pulse; run internal/agent/wayland/testdata/wltest.sh")
	}
	src, _, err := Start(context.Background(), Options{RunDir: t.TempDir(), Attached: true, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	play(t, nil, sine(3, 523, 784))
	checkTone(t, listen(t, src), 523, 784)
}

// TestServerDeathEndsThePackets: the session carries on without sound when
// the server goes away underneath it.
func TestServerDeathEndsThePackets(t *testing.T) {
	needServer(t)
	src, _, err := Start(context.Background(), Options{RunDir: t.TempDir(), Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.srv.cmd.Process.Kill()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-src.Packets():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("packets still open after the server died")
		}
	}
}

// TestCost reports what the audio path costs while something plays: CPU in
// the agent (capture and encoding) and in the private server, and the
// bitrate on the wire.
func TestCost(t *testing.T) {
	needServer(t)
	src, env, err := Start(context.Background(), Options{RunDir: t.TempDir(), Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	server := src.srv.cmd.Process.Pid

	measure := func(what string, d time.Duration) (kbps float64) {
		for len(src.Packets()) > 0 {
			<-src.Packets() // queued before the window opened
		}
		var bytes, packets int
		self0, srv0, start := selfCPU(), procCPU(server), time.Now()
		for deadline := time.After(d); ; {
			select {
			case pkt := <-src.Packets():
				bytes += len(pkt)
				packets++
				continue
			case <-deadline:
			}
			break
		}
		secs := time.Since(start).Seconds()
		kbps = float64(bytes*8) / secs / 1000
		t.Logf("%s: agent %.2f%% of a core, pulseaudio %.2f%%, %.1f kbit/s, %.1f packets/s", what,
			100*(selfCPU()-self0)/secs, 100*(procCPU(server)-srv0)/secs, kbps, float64(packets)/secs)
		if pps := float64(packets) / secs; pps < 45 || pps > 55 {
			t.Errorf("%s: %.1f packets/s, want 50: the RTP clock would drift from real time", what, pps)
		}
		return kbps
	}
	time.Sleep(time.Second)
	measure("silence", 3*time.Second)
	play(t, env, noise(12))
	time.Sleep(2 * time.Second) // paplay's prebuffer
	if k := measure("noise", 8*time.Second); k < 40 {
		t.Errorf("noise came through at %.1f kbit/s; the capture is not carrying it", k)
	}
}

// noise is seconds of loud white noise as 16-bit stereo.
func noise(seconds int) []byte {
	b := make([]byte, seconds*48000*4)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func selfCPU() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
}

// procCPU is pid's user and system time in seconds, from /proc.
func procCPU(pid int) float64 {
	b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	_, rest, _ := strings.Cut(string(b), ") ")
	f := strings.Fields(rest)
	if len(f) < 13 {
		return 0
	}
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / 100 // USER_HZ
}

func TestAttachWithoutAServer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	_, _, err := Start(context.Background(), Options{RunDir: t.TempDir(), Attached: true, Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "no sound server") {
		t.Fatalf("Start = %v, want no sound server", err)
	}
}
