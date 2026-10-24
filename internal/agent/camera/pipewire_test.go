//go:build linux && cgo && openh264

package camera

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperEnv makes the test binary act as an agent that starts the camera
// and waits to be killed; see TestPipeWireCameraDiesWithTheAgent.
const helperEnv = "ROBIN_CAMERA_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		c, err := Start(Options{})
		if err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("ready " + c.Where() + "\n")
		select {}
	}
	os.Exit(m.Run())
}

func needPipeWire(t *testing.T) {
	t.Helper()
	if os.Getenv("ROBIN_PIPEWIRE_PULSE") == "" {
		t.Skip("needs the user's PipeWire; run internal/agent/wayland/testdata/wltest.sh")
	}
}

// grab reads one frame from node the way a program on the desktop would,
// through pipewiresrc, at the camera's own size.
func grab(node string) (frame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", "-q",
		"pipewiresrc", "target-object="+node, "num-buffers=1",
		"!", "videoconvert", "!", "video/x-raw,format=I420,width="+strconv.Itoa(Width)+",height="+strconv.Itoa(Height),
		"!", "fdsink", "fd=1")
	var errs strings.Builder
	cmd.Stderr = &errs
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("grab a frame of %s: %v: %s", node, err, errs.String())
	}
	if len(out) != Width*Height*3/2 {
		return nil, fmt.Errorf("grabbed %d bytes from %s, want one %dx%d frame", len(out), node, Width, Height)
	}
	return frame(out), nil
}

// grabWhen grabs frames of node, once it is there, until one passes ok.
func grabWhen(t *testing.T, node, want string, ok func(y, u, v byte) bool) {
	t.Helper()
	var last string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !nodeExists(t, node) {
			last = "no node " + node
			continue
		}
		f, err := grab(node)
		if err != nil {
			last = err.Error()
			continue
		}
		y, u, v := at(f, 640, 400)
		if ok(y, u, v) {
			return
		}
		last = fmt.Sprintf("the camera shows %d/%d/%d", y, u, v)
	}
	t.Fatalf("want %s; last: %s", want, last)
}

// nodeExists asks PipeWire whether a node of that name is there.
func nodeExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("pw-dump").Output()
	if err != nil {
		t.Fatalf("pw-dump: %v", err)
	}
	var objects []struct {
		Type string `json:"type"`
		Info struct {
			Props map[string]any `json:"props"`
		} `json:"info"`
	}
	if err := json.Unmarshal(out, &objects); err != nil {
		t.Fatalf("pw-dump: %v", err)
	}
	for _, o := range objects {
		if strings.HasSuffix(o.Type, ":Node") && o.Info.Props["node.name"] == name {
			return true
		}
	}
	return false
}

// nodeAppears waits a few seconds for a node: PipeWire registers it once
// the first frame has fixed its format.
func nodeAppears(t *testing.T, name string) bool {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if nodeExists(t, name) {
			return true
		}
	}
	return false
}

// TestPipeWireCamera puts the camera on the user's PipeWire as a node a
// program reads frames from: black before the browser sends, the
// browser's pictures while it does, and gone after Close.
func TestPipeWireCamera(t *testing.T) {
	needPipeWire(t)
	c, err := Start(Options{Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	node := "robin-camera-" + strconv.Itoa(os.Getpid())
	if !nodeAppears(t, node) {
		t.Fatalf("no PipeWire node %s", node)
	}
	grabWhen(t, node, "black before the browser sends", func(y, u, v byte) bool { return y <= 24 && near(u, 128) && near(v, 128) })

	// Five seconds of the browser's camera in one colour, read twice: the
	// second read follows the first one's leaving, which pipewiresink in
	// PipeWire 1.4 does not survive without a restart.
	frames := browserCamera(t, 640, 360, 600)[:300]
	playing := make(chan error, 1)
	go func() {
		playing <- c.Play(&pacedReader{packets: flatten(frames), every: 10 * time.Millisecond}, func() {})
	}()
	for range 2 {
		grabWhen(t, node, "the browser's 81/90/240", func(y, u, v byte) bool { return near(y, 81) && near(u, 90) && near(v, 240) })
	}
	if err := <-playing; err != nil {
		t.Fatal(err)
	}

	c.Close()
	for deadline := time.Now().Add(3 * time.Second); nodeExists(t, node); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("node %s outlived Close", node)
		}
	}
}

// TestPipeWireCameraDiesWithTheAgent kills an agent with SIGKILL, which runs
// no cleanup, and checks its camera's node goes with it.
func TestPipeWireCameraDiesWithTheAgent(t *testing.T) {
	needPipeWire(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready ") {
		cmd.Process.Kill()
		t.Fatalf("helper said %q: %v", line, err)
	}
	node := "robin-camera-" + strconv.Itoa(cmd.Process.Pid)
	if !nodeAppears(t, node) {
		cmd.Process.Kill()
		t.Fatalf("the helper's node %s is not there", node)
	}
	cmd.Process.Kill()
	cmd.Wait()
	io.Copy(io.Discard, out)
	for deadline := time.Now().Add(3 * time.Second); nodeExists(t, node); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("node %s outlived its agent", node)
		}
	}
}
