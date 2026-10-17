//go:build linux

package hwenc

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

const tick = time.Second / 30

// fakeFrame decodes what fakeSlice put in an access unit's slice.
func fakeFrame(t *testing.T, au []byte) (n int, luma byte, idr bool) {
	t.Helper()
	for _, nal := range nalUnits(au) {
		if typ := nalType(nal); typ == nalSlice || typ == nalIDR {
			return int(nal[2])<<8 | int(nal[3]), nal[4], typ == nalIDR
		}
	}
	t.Fatalf("no slice in access unit with NAL types %v", types(au))
	return
}

func pid(e *Encoder) int { return e.proc.cmd.Process.Pid }

func TestEncoderPairsFramesWithAccessUnits(t *testing.T) {
	for _, framing := range []string{"multipart", "flv"} {
		t.Run(framing, func(t *testing.T) {
			e := newEncoder(fakeRoute(framing+":normal"), 64, 48, 30, 1_000_000)
			img := yuv.NewI420(64, 48)
			for i := range 5 {
				img.Y[0] = byte(10 + i)
				au, err := e.Encode(img, time.Duration(i)*tick, i == 0)
				if err != nil {
					t.Fatal(err)
				}
				n, luma, idr := fakeFrame(t, au)
				if n != i || luma != img.Y[0] || idr != (i == 0) {
					t.Fatalf("frame %d came back as frame %d, luma %d, IDR %v", i, n, luma, idr)
				}
				if i == 0 {
					if idc, err := profile(au); err != nil || idc != profileBaseline {
						t.Fatalf("first access unit: profile %d, %v", idc, err)
					}
				}
			}
			p := pid(e)
			e.Close()
			if err := waitGone(p, time.Second); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Encode(img, time.Second, false); !errors.Is(err, errClosed) {
				t.Fatalf("encode after close: %v", err)
			}
		})
	}
}

func TestEncoderRejectsWrongSize(t *testing.T) {
	e := newEncoder(fakeRoute("multipart:normal"), 64, 48, 30, 1_000_000)
	defer e.Close()
	if _, err := e.Encode(yuv.NewI420(64, 64), 0, true); err == nil {
		t.Fatal("encoded a frame of the wrong size")
	}
}

func TestKeyframeRestartsTool(t *testing.T) {
	e := newEncoder(fakeRoute("flv:normal"), 64, 48, 30, 1_000_000)
	defer e.Close()
	img := yuv.NewI420(64, 48)
	if _, err := e.Encode(img, 0, true); err != nil {
		t.Fatal(err)
	}
	first := pid(e)
	for i := 1; i < 3; i++ {
		if _, err := e.Encode(img, time.Duration(i)*tick, false); err != nil {
			t.Fatal(err)
		}
	}
	au, err := e.Encode(img, 3*tick, true)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, idr := fakeFrame(t, au); n != 0 || !idr {
		t.Fatalf("requested keyframe came back as frame %d, IDR %v", n, idr)
	}
	if _, err := profile(au); err != nil {
		t.Fatalf("requested keyframe: %v", err)
	}
	if pid(e) == first {
		t.Fatal("tool was not restarted")
	}
	if err := waitGone(first, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestBitrateRestartsOnlyForLargeMoves(t *testing.T) {
	e := newEncoder(fakeRoute("multipart:normal"), 64, 48, 30, 1_000_000)
	defer e.Close()
	img := yuv.NewI420(64, 48)
	var ts time.Duration
	step := func(bps int, until time.Duration, wantBitrate int) {
		t.Helper()
		e.SetBitrate(bps)
		for ; ts < until; ts += tick {
			if _, err := e.Encode(img, ts, ts == 0); err != nil {
				t.Fatal(err)
			}
		}
		if e.proc.bitrate != wantBitrate {
			t.Fatalf("at %v with target %d the tool runs at %d, want %d", ts, bps, e.proc.bitrate, wantBitrate)
		}
	}
	step(1_000_000, time.Second, 1_000_000)
	step(1_900_000, 3*time.Second, 1_000_000) // under twice the running rate
	step(2_000_000, 3500*time.Millisecond, 2_000_000)
	step(900_000, 5*time.Second, 2_000_000) // a big move, but too soon
	step(900_000, 6*time.Second, 900_000)
}

func TestBudgetHoldsTargetBitrate(t *testing.T) {
	const target = 1_000_000
	// Every slice is 10 kB, 2.4 Mbit/s at 30 frames a second.
	e := newEncoder(fakeRoute("multipart:normal", "ROBIN_HWENC_FAKE_AU=10000"), 64, 48, 30, target)
	defer e.Close()
	img := yuv.NewI420(64, 48)
	bits, skipped := 0, 0
	for i := range 4 * 30 {
		ts := time.Duration(i) * tick
		au, err := e.Encode(img, ts, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(au) == 0 {
			skipped++
		}
		if ts >= time.Second {
			bits += 8 * len(au)
		}
	}
	got := float64(bits) / 3
	t.Logf("sent %.0f kbit/s against %d, skipped %d of 120 frames", got/1000, target/1000, skipped)
	if got > 1.1*target || got < 0.8*target {
		t.Fatalf("sent %.0f kbit/s against a %d kbit/s target", got/1000, target/1000)
	}
	// In debt now, a requested keyframe still goes out.
	if au, err := e.Encode(img, 4*time.Second, true); err != nil || len(au) == 0 {
		t.Fatalf("keyframe while over budget: %d bytes, %v", len(au), err)
	}
}

func TestToolFailureBacksOff(t *testing.T) {
	e := newEncoder(fakeRoute("multipart:die"), 64, 48, 30, 1_000_000)
	defer e.Close()
	img := yuv.NewI420(64, 48)
	_, err := e.Encode(img, 0, true)
	if err == nil || !strings.Contains(err.Error(), "fake: exploded") {
		t.Fatalf("got %v, want the tool's own message", err)
	}
	if e.proc != nil {
		t.Fatal("failed tool still held")
	}
	if au, err := e.Encode(img, failureBackoff/2, true); err != nil || au != nil || e.proc != nil {
		t.Fatalf("within the backoff: %d bytes, %v, tool started %v", len(au), err, e.proc != nil)
	}
	if _, err := e.Encode(img, failureBackoff, true); err == nil {
		t.Fatal("no second attempt after the backoff")
	}
}

func TestStalledToolTimesOut(t *testing.T) {
	e := newEncoder(fakeRoute("multipart:hang"), 64, 48, 30, 1_000_000)
	defer e.Close()
	img := yuv.NewI420(64, 48)
	if _, err := e.Encode(img, 0, true); err != nil {
		t.Fatal(err)
	}
	p := pid(e)
	start := time.Now()
	if _, err := e.Encode(img, tick, false); err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > frameTimeout+500*time.Millisecond {
		t.Fatalf("gave up after %v", d)
	}
	if err := waitGone(p, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCloseKillsProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	e := newEncoder(fakeRoute("multipart:spawn", "ROBIN_HWENC_FAKE_PIDFILE="+pidfile), 64, 48, 30, 1_000_000)
	if _, err := e.Encode(yuv.NewI420(64, 48), 0, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, _ := strconv.Atoi(string(b))
	tool := pid(e)
	e.Close()
	for _, p := range []int{tool, grandchild} {
		if err := waitGone(p, time.Second); err != nil {
			t.Error(err)
		}
	}
}

func TestToolDiesWithAgent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agent := exec.Command(exe)
	agent.Env = append(os.Environ(), fakeEnv+"=multipart:parent")
	out, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	tool, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("agent said %q", line)
	}
	agent.Process.Signal(syscall.SIGKILL)
	agent.Wait()
	if err := waitGone(tool, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTrialChecksTheStream(t *testing.T) {
	if err := trial(fakeRoute("multipart:normal"), time.Second); err != nil {
		t.Fatalf("normal: %v", err)
	}
	for mode, want := range map[string]string{
		"multipart:high": "profile_idc 100",
		"flv:lag":        "in time",
		"flv:die":        "fake: exploded",
	} {
		if err := trial(fakeRoute(mode), time.Second); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", mode, err, want)
		}
	}
}

func TestOpenerReportsEveryRoute(t *testing.T) {
	calls := 0
	o := NewOpener(VAAPI, nil)
	o.routes = func() ([]route, error) {
		calls++
		return []route{*fakeRoute("multipart:die"), *fakeRoute("flv:high")}, nil
	}
	_, err := o.Open(640, 480, 30, 1_000_000)
	if err == nil {
		t.Fatal("opened with no working route")
	}
	for _, want := range []string{"fake multipart:die", "exploded", "fake flv:high", "profile_idc 100", "h264_vaapi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if _, err2 := o.Open(640, 480, 30, 1_000_000); err2 == nil || err2.Error() != err.Error() || calls != 1 {
		t.Errorf("second open: %v after %d route lookups", err2, calls)
	}

	o = NewOpener(NVENC, nil)
	o.routes = func() ([]route, error) { return []route{*fakeRoute("multipart:die"), *fakeRoute("flv:normal")}, nil }
	e, err := o.Open(640, 480, 30, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.route.name != "fake flv:normal" {
		t.Fatalf("picked %s", e.route.name)
	}
	if _, err := o.Open(30, 480, 30, 1_000_000); err == nil {
		t.Fatal("opened a 30 pixel wide encoder")
	}
}
