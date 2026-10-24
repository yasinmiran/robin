//go:build linux

package audio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

// helperEnv makes the test binary act as an agent that starts a server and
// waits to be killed; see TestServerDiesWithTheAgent.
const helperEnv = "ROBIN_AUDIO_TEST_HELPER"

func TestMain(m *testing.M) {
	if dir := os.Getenv(helperEnv); dir != "" {
		s, err := startServer(context.Background(), dir)
		if err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("ready " + strconv.Itoa(s.cmd.Process.Pid) + "\n")
		select {}
	}
	if os.Getenv(keeperEnv) != "" {
		serveKeeper()
		return
	}
	if room := os.Getenv(roomEnv); room != "" {
		silenceForTest(room)
	}
	os.Exit(m.Run())
}

// needServer skips unless the host can run a private pulseaudio and has no
// sound server of the user's own. These tests start a daemon; on a desktop
// where one already plays, they belong in the X test container instead.
func needServer(t *testing.T) {
	t.Helper()
	for _, c := range []string{"pulseaudio", "paplay"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip(c + " not on PATH; run internal/agent/x11/testdata/xtest.sh")
		}
	}
	if _, err := os.Stat(userSocket()); err == nil {
		t.Skip("this user already runs a sound server; run internal/agent/x11/testdata/xtest.sh")
	}
}

func serve(t *testing.T) *server {
	t.Helper()
	needServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := startServer(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// sine is seconds of 16-bit stereo with left and right at their own pitch.
func sine(seconds, left, right float64) []byte {
	var b bytes.Buffer
	for i := range int(seconds * 48000) {
		t := float64(i) / 48000
		binary.Write(&b, binary.LittleEndian, int16(8000*math.Sin(2*math.Pi*left*t)))
		binary.Write(&b, binary.LittleEndian, int16(8000*math.Sin(2*math.Pi*right*t)))
	}
	return b.Bytes()
}

// play plays raw 48 kHz stereo through paplay with env added, the way a
// program on the desktop would. A test may stop listening, or stop the
// server, before the sound ends; paplay is killed at cleanup, and what it
// said is logged.
func play(t *testing.T, env []string, pcm []byte) {
	t.Helper()
	playAs(t, "paplay", env, pcm)
}

// playAs plays as play does, under the program name app.
func playAs(t *testing.T, app string, env []string, pcm []byte) {
	t.Helper()
	cmd := exec.Command("paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--client-name="+app)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(pcm)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if s := strings.TrimSpace(out.String()); s != "" {
			t.Logf("paplay: %s", s)
		}
	})
}

func rms(pcm []int16) float64 {
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

func TestPrivateServerCapturesWhatPlays(t *testing.T) {
	s := serve(t)
	for path, mode := range map[string]os.FileMode{s.dir: 0o700 | os.ModeDir, s.cookie: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode() != mode {
			t.Errorf("%s: mode %v (%v), want %v", path, fi.Mode(), err, mode)
		}
	}
	if fi, err := os.Stat(s.socket); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no socket at %s: %v", s.socket, err)
	}

	cookie, _ := os.ReadFile(s.cookie)
	levels := make(chan float64, 1000)
	rec, err := record(s.socket, cookie, func(pcm []int16) {
		select {
		case levels <- rms(pcm):
		default:
		}
	}, func(err error) { t.Errorf("recording ended: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	defer rec.close()
	if rec.sink != "robin" {
		t.Errorf("recording sink %q, want the null sink", rec.sink)
	}

	// The monitor runs in real time with nothing playing.
	start := time.Now()
	for range 25 {
		select {
		case l := <-levels:
			if l != 0 {
				t.Fatalf("silence has level %.0f", l)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no frames while idle")
		}
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("25 frames of silence in %v; the monitor is not paced", d)
	}

	play(t, s.Env(), sine(1, 440, 660))
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l := <-levels:
			if l > 2000 {
				return
			}
		case <-deadline:
			t.Fatal("the tone never reached the monitor")
		}
	}
}

func TestServerCloseLeavesNothing(t *testing.T) {
	s := serve(t)
	pid := s.cmd.Process.Pid
	start := time.Now()
	s.Close()
	t.Logf("Close took %v", time.Since(start).Round(time.Millisecond))
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Errorf("pulseaudio %d still running after Close", pid)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Errorf("%s left behind: %v", s.dir, err)
	}
	s.Close()
}

func TestServerDiesWithTheAgent(t *testing.T) {
	needServer(t)
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), helperEnv+"="+dir)
	stdout, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "ready ")))
	if err != nil {
		helper.Process.Kill()
		t.Fatalf("helper: %q", line)
	}
	helper.Process.Signal(syscall.SIGKILL)
	helper.Wait()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !running(pid) {
			return
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("pulseaudio %d outlived the agent", pid)
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

// running reports a live process; a zombie has already been killed.
func running(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	_, rest, _ := bytes.Cut(b, []byte(") "))
	return len(rest) > 0 && rest[0] != 'Z' && rest[0] != 'X'
}

func TestServerFailureReportsItsLog(t *testing.T) {
	needServer(t)
	fake := filepath.Join(t.TempDir(), "pulseaudio")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'E: module-null-sink failed' >&2\nexit 1\n"), 0o755)
	t.Setenv("PATH", filepath.Dir(fake)+":"+os.Getenv("PATH"))
	dir := t.TempDir()
	_, err := startServer(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "module-null-sink failed") {
		t.Fatalf("startServer = %v, want the daemon's log in the error", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("run dir not cleaned: %v", left)
	}
}
