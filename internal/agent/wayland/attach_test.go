//go:build linux

package wayland

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"

	"github.com/yasinmiran/remotejump/internal/agent/desktop"
	"github.com/yasinmiran/remotejump/internal/agent/wayland/wltest"
	"github.com/yasinmiran/remotejump/internal/protocol"
)

// The tests in this file drive a real GNOME Wayland session through
// xdg-desktop-portal. testdata/wltest.sh runs them in a container with
// headless mutter; anywhere else they skip.

var (
	probeScript = os.Getenv("RJ_WAYLAND_PROBE")
	// tokenFile is shared by the tests in one run, so each restores the
	// grant an earlier one got, as consecutive sessions would.
	tokenFile string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wltest")
	if err != nil {
		panic(err)
	}
	tokenFile = filepath.Join(dir, "token")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func needSession(t *testing.T) {
	t.Helper()
	if probeScript == "" {
		t.Skip("needs a GNOME Wayland session; run internal/agent/wayland/testdata/wltest.sh")
	}
}

func TestAttachConsentThenRestore(t *testing.T) {
	needSession(t)
	removeToken(tokenFile)

	d, notices := open(t, true)
	if len(notices) != 1 || notices[0].Kind != protocol.NoticeConsent || notices[0].Wait < 25 {
		t.Errorf("first session: notices %+v, want one consent notice with the wait", notices)
	}
	if d.in.viewOnly {
		t.Error("first session is view only though remote interaction was allowed")
	}
	fi, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("no restore token kept: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	d.Close()
	checkTornDown(t)

	start := time.Now()
	d, notices = open(t, false)
	defer d.Close()
	if len(notices) != 0 {
		t.Errorf("restored session: notices %+v, want none", notices)
	}
	if took := time.Since(start); took > restoreGrace {
		t.Errorf("restored session took %s to open", took)
	}
	if d.in.viewOnly {
		t.Error("restored session is view only")
	}
}

func TestAttachStreamsAndDrives(t *testing.T) {
	needSession(t)
	p := startProbe(t)
	d, _ := open(t, true)

	if w, h := d.Size(); w != 1280 || h != 720 {
		t.Fatalf("size %dx%d, want the 1280x720 monitor", w, h)
	}
	f, _, err := d.Capture(true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != desktop.I420 || f.Width != 1280 || f.Height != 720 || len(f.Pix) != 1280*720*3/2 {
		t.Fatalf("frame %dx%d format %v, %d bytes", f.Width, f.Height, f.Format, len(f.Pix))
	}
	first := bytes.Clone(f.Pix)

	t.Run("pointer", func(t *testing.T) {
		mark := p.mark()
		send(t, d, protocol.Control{T: protocol.ControlMove, X: 301, Y: 203})
		ev := p.await(t, mark, func(e probeEvent) bool { return e.Ev == "motion" && near(e.X, 301) && near(e.Y, 203) })
		t.Logf("motion at %.1f,%.1f", ev.X, ev.Y)
		// The pointer is drawn into the picture, so moving it changes the frame.
		if !frameChanges(d, first) {
			t.Error("frames did not change after the pointer moved")
		}

		mark = p.mark()
		send(t, d, protocol.Control{T: protocol.ControlButton, Button: 2, Down: true})
		send(t, d, protocol.Control{T: protocol.ControlButton, Button: 2})
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "press" && e.Button == 3 })
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "release" && e.Button == 3 })

		mark = p.mark()
		send(t, d, protocol.Control{T: protocol.ControlWheel, DY: 100})
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "scroll" && e.DY > 0 })
		send(t, d, protocol.Control{T: protocol.ControlWheel, DX: -200})
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "scroll" && e.DX < 0 })
	})

	t.Run("keyboard", func(t *testing.T) {
		mark := p.mark()
		// The session's first key is a plain one: GNOME drops the first key
		// event of a session, and Open's warm-up is what keeps this one.
		for _, m := range []protocol.Control{
			{T: protocol.ControlKey, Key: "a", Code: "KeyA", Down: true},
			{T: protocol.ControlKey, Key: "a", Code: "KeyA"},
			{T: protocol.ControlKey, Key: "Shift", Code: "ShiftLeft", Down: true},
			{T: protocol.ControlKey, Key: "H", Code: "KeyH", Down: true},
			{T: protocol.ControlKey, Key: "H", Code: "KeyH"},
			{T: protocol.ControlKey, Key: "Shift", Code: "ShiftLeft"},
			{T: protocol.ControlKey, Key: "i", Code: "KeyI", Down: true},
			{T: protocol.ControlKey, Key: "i", Code: "KeyI"},
			{T: protocol.ControlKey, Key: "!", Code: "Digit1", Down: true},
			{T: protocol.ControlKey, Key: "!", Code: "Digit1"},
			{T: protocol.ControlKey, Key: "Enter", Code: "Enter", Down: true},
			{T: protocol.ControlKey, Key: "Enter", Code: "Enter"},
		} {
			send(t, d, m)
		}
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "key" && e.Keyval == "Return" })
		if got := p.typed(mark); got != "aHi!\r" {
			t.Errorf("typed %q, want %q", got, "aHi!\r")
		}

		// The client repeats a held key itself; the browser's repeats are
		// not passed on, or each would count twice.
		mark = p.mark()
		send(t, d, protocol.Control{T: protocol.ControlKey, Key: "Backspace", Code: "Backspace", Down: true})
		for range 30 {
			time.Sleep(33 * time.Millisecond)
			send(t, d, protocol.Control{T: protocol.ControlKey, Key: "Backspace", Code: "Backspace", Down: true, Repeat: true})
		}
		send(t, d, protocol.Control{T: protocol.ControlKey, Key: "Backspace", Code: "Backspace"})
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "key-up" && e.Keyval == "BackSpace" })
		n := p.count(mark, func(e probeEvent) bool { return e.Ev == "key" && e.Keyval == "BackSpace" })
		t.Logf("Backspace held for 1 s: %d presses", n)
		if n < 2 || n > 45 {
			t.Errorf("Backspace held for 1 s gave %d presses", n)
		}

		mark = p.mark()
		send(t, d, protocol.Control{T: protocol.ControlKey, Key: "Control", Code: "ControlLeft", Down: true})
		send(t, d, protocol.Control{T: protocol.ControlRelease})
		p.await(t, mark, func(e probeEvent) bool { return e.Ev == "key-up" && e.Keyval == "Control_L" })
	})

	t.Run("clipboard", func(t *testing.T) {
		if d.clip == nil {
			t.Fatal("clipboard not granted")
		}
		if err := d.SetClipboard("from-browser"); err != nil {
			t.Fatal(err)
		}
		mark := p.mark()
		tap(t, d, "F2")
		ev := p.await(t, mark, func(e probeEvent) bool { return e.Ev == "clipboard" || e.Ev == "clipboard-error" })
		if ev.Text != "from-browser" {
			t.Errorf("desktop pasted %+v, want from-browser", ev)
		}

		tap(t, d, "F3")
		select {
		case text := <-d.Clipboard():
			if text != "from-session" {
				t.Errorf("desktop copied %q, want from-session", text)
			}
		case <-time.After(5 * time.Second):
			t.Error("nothing copied on the desktop reached the clipboard channel")
		}
	})

	t.Run("cursor", func(t *testing.T) {
		select {
		case c := <-d.Cursor():
			if !c.Hidden {
				t.Errorf("cursor %+v, want hidden: the picture carries the pointer", c)
			}
		default:
			t.Error("no cursor message")
		}
	})

	d.Close()
	checkTornDown(t)
}

func TestAttachEndsWhenTheStreamDies(t *testing.T) {
	needSession(t)
	d, _ := open(t, true)
	defer d.Close()
	d.frames.cmd.Process.Kill()
	select {
	case err := <-d.Done():
		if !strings.Contains(err.Error(), "gst-launch-1.0") {
			t.Errorf("Done: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire after gst-launch died")
	}
}

// TestAttachTornDownWhenAgentDies kills a process holding the desktop with
// SIGKILL, so no Close runs: the kernel must take gst-launch with it and the
// portal must end the session when the process's bus connection drops.
func TestAttachTornDownWhenAgentDies(t *testing.T) {
	needSession(t)
	if os.Getenv("RJ_WAYLAND_HOLD") != "" {
		d, _ := open(t, true)
		defer d.Close()
		fmt.Println("holding")
		time.Sleep(time.Minute)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttachTornDownWhenAgentDies$")
	cmd.Env = append(os.Environ(), "RJ_WAYLAND_HOLD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	held := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "holding" {
				held <- true
			}
		}
		close(held)
	}()
	select {
	case ok := <-held:
		if !ok {
			t.Fatal("the holding process exited before opening the desktop")
		}
	case <-time.After(time.Minute):
		t.Fatal("the holding process did not open the desktop")
	}
	if n := countProcs("gst-launch-1.0"); n != 1 {
		t.Fatalf("%d gst-launch-1.0 processes while holding, want 1", n)
	}
	cmd.Process.Kill()
	checkTornDown(t)
}

func TestAttachViewOnly(t *testing.T) {
	needSession(t)
	removeToken(tokenFile)
	d, notices := open(t, false)
	defer d.Close()
	if len(notices) != 2 || notices[0].Kind != protocol.NoticeConsent || notices[1].Kind != protocol.NoticeViewOnly {
		t.Errorf("notices %+v, want consent then view-only", notices)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Errorf("a view-only grant was kept: %v", err)
	}
	if err := d.Input(protocol.Control{T: protocol.ControlMove, X: 10, Y: 10}); !errors.Is(err, errViewOnly) {
		t.Errorf("input on a view-only session: %v", err)
	}
	if f, _, err := d.Capture(true); err != nil || f.Width != 1280 {
		t.Errorf("view-only session streams no picture: %v", err)
	}
}

// TestAttachFrameRate reports what streaming a fully animated screen costs.
func TestAttachFrameRate(t *testing.T) {
	needSession(t)
	startProbe(t, "--animate")
	for _, limit := range [][2]int{{1920, 1080}, {960, 540}} {
		t.Run(fmt.Sprintf("max %dx%d", limit[0], limit[1]), func(t *testing.T) {
			d, _ := openWith(t, true, func(o *Options) { o.MaxWidth, o.MaxHeight = limit[0], limit[1] })
			defer d.Close()
			const fps, secs = 30, 10
			tick := time.NewTicker(time.Second / fps)
			defer tick.Stop()
			gst := d.frames.cmd.Process.Pid
			gst0, self0 := cpuTicks(gst), cpuTicks(os.Getpid())
			changed := 0
			for range fps * secs {
				<-tick.C
				if _, ok, err := d.Capture(false); err == nil && ok {
					changed++
				}
			}
			w, h := d.Size()
			t.Logf("%dx%d: %.1f fps of %d, gst-launch %.0f%% CPU, reader %.0f%% CPU", w, h,
				float64(changed)/secs, fps, percent(cpuTicks(gst)-gst0, secs), percent(cpuTicks(os.Getpid())-self0, secs))
			if changed < fps*secs/2 {
				t.Errorf("%d new frames in %d s at %d fps", changed, secs, fps)
			}
		})
	}
}

func open(t *testing.T, allowInput bool) (*Desktop, []protocol.Notice) {
	return openWith(t, allowInput, nil)
}

// openWith opens the desktop and, if a consent dialog comes up, answers it
// as the person at the screen would, allowing input or not.
func openWith(t *testing.T, allowInput bool, opt func(*Options)) (*Desktop, []protocol.Notice) {
	t.Helper()
	var mu sync.Mutex
	var notices []protocol.Notice
	answered := make(chan error, 1)
	o := Options{
		TokenFile: tokenFile, MaxWidth: 1920, MaxHeight: 1080, FPS: 30, ConsentWait: 30 * time.Second,
		Notify: func(n protocol.Notice) {
			mu.Lock()
			notices = append(notices, n)
			mu.Unlock()
			if n.Kind == protocol.NoticeConsent {
				go func() { answered <- wltest.AnswerDialog(allowInput) }()
			}
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	if opt != nil {
		opt(&o)
	}
	d, err := Open(context.Background(), o)
	select {
	case aerr := <-answered:
		if aerr != nil {
			t.Fatalf("answer the consent dialog: %v", aerr)
		}
	default:
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return d, notices
}

// checkTornDown waits for every trace of the session to go: the gst-launch
// process, the portal's session object and mutter's sessions behind it.
func checkTornDown(t *testing.T) {
	t.Helper()
	conn, err := sessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	children := func(dest, path string) int {
		node, err := introspect.Call(conn.Object(dest, dbus.ObjectPath(path)))
		if err != nil {
			return 0
		}
		return len(node.Children)
	}
	var left []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		left = nil
		if n := countProcs("gst-launch-1.0"); n > 0 {
			left = append(left, fmt.Sprintf("%d gst-launch-1.0", n))
		}
		for _, o := range [][2]string{
			{portalBus, "/org/freedesktop/portal/desktop/session"},
			{"org.gnome.Mutter.RemoteDesktop", "/org/gnome/Mutter/RemoteDesktop/Session"},
			{"org.gnome.Mutter.ScreenCast", "/org/gnome/Mutter/ScreenCast/Session"},
		} {
			if n := children(o[0], o[1]); n > 0 {
				left = append(left, fmt.Sprintf("%d under %s", n, o[1]))
			}
		}
		if len(left) == 0 {
			return
		}
	}
	t.Errorf("left behind after Close: %s", strings.Join(left, ", "))
}

func send(t *testing.T, d *Desktop, m protocol.Control) {
	t.Helper()
	if err := d.Input(m); err != nil {
		t.Fatalf("input %+v: %v", m, err)
	}
	time.Sleep(10 * time.Millisecond)
}

func tap(t *testing.T, d *Desktop, key string) {
	send(t, d, protocol.Control{T: protocol.ControlKey, Key: key, Code: key, Down: true})
	send(t, d, protocol.Control{T: protocol.ControlKey, Key: key, Code: key})
}

func frameChanges(d *Desktop, before []byte) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(33 * time.Millisecond) {
		if f, changed, err := d.Capture(false); err == nil && changed && !bytes.Equal(f.Pix, before) {
			return true
		}
	}
	return false
}

func near(got, want float64) bool { return got > want-1.5 && got < want+1.5 }

// probe runs testdata/probe.py and collects what it reports.
type probe struct {
	mu     sync.Mutex
	events []probeEvent
	added  chan struct{}
}

type probeEvent struct {
	Ev     string  `json:"ev"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	DX     float64 `json:"dx"`
	DY     float64 `json:"dy"`
	Button int     `json:"button"`
	Keyval string  `json:"keyval"`
	Text   string  `json:"text"`
}

func startProbe(t *testing.T, args ...string) *probe {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"-I", probeScript}, args...)...)
	cmd.Env = append(os.Environ(), "WAYLAND_DISPLAY=wayland-0", "GDK_BACKEND=wayland", "GSK_RENDERER=cairo",
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(runtimeDir(), "bus"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	p := &probe{added: make(chan struct{}, 1)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var e probeEvent
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			p.mu.Lock()
			p.events = append(p.events, e)
			p.mu.Unlock()
			select {
			case p.added <- struct{}{}:
			default:
			}
		}
	}()
	p.await(t, 0, func(e probeEvent) bool { return e.Ev == "ready" })
	return p
}

func (p *probe) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// await returns the first event from mark on that matches, failing the
// test if none arrives within a few seconds.
func (p *probe) await(t *testing.T, mark int, match func(probeEvent) bool) probeEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		p.mu.Lock()
		for _, e := range p.events[mark:] {
			if match(e) {
				p.mu.Unlock()
				return e
			}
		}
		seen := append([]probeEvent(nil), p.events[mark:]...)
		p.mu.Unlock()
		select {
		case <-p.added:
		case <-timeout:
			t.Fatalf("the probe saw no such event; it saw %+v", seen)
		}
	}
}

func (p *probe) count(mark int, match func(probeEvent) bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events[mark:] {
		if match(e) {
			n++
		}
	}
	return n
}

// typed returns the text of the key presses from mark on.
func (p *probe) typed(mark int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, e := range p.events[mark:] {
		if e.Ev == "key" && e.Text != "\x00" {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func countProcs(comm string) int {
	ents, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		c, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		stat, _ := os.ReadFile("/proc/" + e.Name() + "/stat")
		if strings.TrimSpace(string(c)) == comm[:min(len(comm), 15)] && !bytes.Contains(stat, []byte(") Z ")) {
			n++
		}
	}
	return n
}

// cpuTicks is a process's user and system time in clock ticks (USER_HZ, 100).
func cpuTicks(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+2:]))
	u, _ := strconv.ParseInt(f[11], 10, 64)
	s, _ := strconv.ParseInt(f[12], 10, 64)
	return u + s
}

func percent(ticks int64, secs int) float64 { return float64(ticks) / float64(secs) }
