//go:build linux && cgo && opus

package audio

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
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

func TestAttachWithoutAServer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	_, _, err := Start(context.Background(), Options{RunDir: t.TempDir(), Attached: true, Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "no sound server") {
		t.Fatalf("Start = %v, want no sound server", err)
	}
}
