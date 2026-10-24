//go:build linux

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/agent/files"
	"github.com/yasinmiran/robin/internal/protocol"
)

// fileBrowser plays the page for files and the clipboard: it hears the
// control channel and takes the bulk transfers the agent opens, keeping
// each whose hash matches.
type fileBrowser struct {
	t        *testing.T
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	controls chan protocol.Control
	bulks    chan received
	// refuse makes it answer every transfer with an error instead, and
	// refused counts those it did.
	refuse  atomic.Bool
	refused atomic.Int32
}

// received is one transfer the agent sent, already answered done.
type received struct {
	header protocol.Bulk
	data   []byte
}

func newFileBrowser(t *testing.T, h *harness) *fileBrowser {
	t.Helper()
	pc, dc, _ := connectBrowser(t, h)
	return wrapBrowser(t, pc, dc)
}

// wrapBrowser listens on a browser's control channel and takes the bulk
// transfers the agent opens, once the control channel is open.
func wrapBrowser(t *testing.T, pc *webrtc.PeerConnection, dc *webrtc.DataChannel) *fileBrowser {
	t.Helper()
	b := &fileBrowser{t: t, pc: pc, dc: dc, controls: make(chan protocol.Control, 64), bulks: make(chan received, 8)}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(m.Data, &c) == nil && c.T != protocol.ControlHeartbeat && c.T != protocol.ControlCursor {
			b.controls <- c
		}
	})
	pc.OnDataChannel(func(ch *webrtc.DataChannel) {
		if ch.Label() == protocol.BulkChannel {
			b.take(ch)
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for dc.ReadyState() != webrtc.DataChannelStateOpen {
		if time.Now().After(deadline) {
			t.Fatalf("control channel never opened; ICE %s", pc.ICEConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return b
}

func (b *fileBrowser) take(ch *webrtc.DataChannel) {
	var r received
	sum := sha256.New()
	ch.OnMessage(func(m webrtc.DataChannelMessage) {
		if !m.IsString {
			r.data = append(r.data, m.Data...)
			sum.Write(m.Data)
			return
		}
		f, _ := protocol.ParseBulk(m.Data)
		switch f.T {
		case protocol.BulkHeader:
			r.header = f
		case protocol.BulkEnd:
			verdict := protocol.Bulk{T: protocol.BulkDone}
			if f.SHA256 != hex.EncodeToString(sum.Sum(nil)) {
				verdict = protocol.Bulk{T: protocol.BulkError, Error: "hash"}
			}
			if b.refuse.Load() {
				verdict = protocol.Bulk{T: protocol.BulkError, Error: "no room"}
				defer b.refused.Add(1)
			}
			v, _ := json.Marshal(verdict)
			ch.SendText(string(v))
			ch.Close()
			if !b.refuse.Load() {
				b.bulks <- r
			}
		}
	})
}

func (b *fileBrowser) control(t protocol.ControlType) protocol.Control {
	b.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case c := <-b.controls:
			if c.T == t {
				return c
			}
		case <-deadline:
			b.t.Fatalf("no %s from the agent", t)
		}
	}
}

func (b *fileBrowser) bulk() received {
	b.t.Helper()
	select {
	case r := <-b.bulks:
		return r
	case <-time.After(15 * time.Second):
		b.t.Fatal("the agent sent no bulk transfer")
		return received{}
	}
}

func (b *fileBrowser) sendControl(c protocol.Control) {
	m, _ := json.Marshal(c)
	b.dc.SendText(string(m))
}

// upload sends data as a transfer under h, with end changed by mutate
// when set, and returns the agent's verdict, or a frame of type "closed"
// when the agent closed the channel without one. cancelAfter, when over
// zero, closes the channel after that many chunks instead of ending.
func (b *fileBrowser) upload(h protocol.Bulk, data []byte, mutate func(*protocol.Bulk), cancelAfter int) protocol.Bulk {
	b.t.Helper()
	ch, err := b.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	opened, closed := make(chan struct{}), make(chan struct{})
	verdict := make(chan protocol.Bulk, 1)
	ch.OnOpen(func() { close(opened) })
	ch.OnClose(func() { close(closed) })
	ch.OnMessage(func(m webrtc.DataChannelMessage) {
		v, _ := protocol.ParseBulk(m.Data)
		select {
		case verdict <- v:
		default:
		}
	})
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		b.t.Fatal("bulk channel never opened")
	}
	h.T = protocol.BulkHeader
	head, _ := json.Marshal(h)
	ch.SendText(string(head))
	sum := protocol.NewBulkSum(int64(len(data)))
	for i, rest := 0, data; len(rest) > 0; i++ {
		if cancelAfter > 0 && i == cancelAfter {
			ch.Close()
			return protocol.Bulk{T: "closed"}
		}
		n := min(len(rest), protocol.BulkChunkSize)
		sum.Add(rest[:n])
		ch.Send(rest[:n])
		rest = rest[n:]
	}
	end, _ := sum.End()
	if mutate != nil {
		mutate(&end)
	}
	e, _ := json.Marshal(end)
	ch.SendText(string(e))
	select {
	case v := <-verdict:
		return v
	case <-closed:
		select {
		case v := <-verdict:
			return v
		default:
			return protocol.Bulk{T: "closed"}
		}
	case <-time.After(15 * time.Second):
		b.t.Fatal("no verdict on the upload")
		return protocol.Bulk{}
	}
}

func noise(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(uint64(n), 7))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func runDirOf(t *testing.T, h *harness) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(h.root, "*", files.SocketName))
	if len(dirs) != 1 {
		t.Fatalf("offer sockets %v, want one", dirs)
	}
	return filepath.Dir(dirs[0])
}

func TestSessionUploads(t *testing.T) {
	downloads := filepath.Join(t.TempDir(), "Downloads")
	t.Setenv("XDG_DOWNLOAD_DIR", downloads)
	h := startAgent(t)
	ready := h.start()
	b := newFileBrowser(t, h)

	data := noise(300 << 10)
	header := protocol.Bulk{Kind: protocol.BulkFile, ID: "u1", Name: "photo.jpg", Size: int64(len(data))}
	if v := b.upload(header, data, nil, 0); v.T != protocol.BulkDone {
		t.Fatalf("upload verdict %+v", v)
	}
	if saved := b.control(protocol.ControlFileSaved); saved.File.ID != "u1" || saved.File.Name != "photo.jpg" {
		t.Errorf("saved %+v", saved.File)
	}
	header.ID = "u2"
	if v := b.upload(header, data, nil, 0); v.T != protocol.BulkDone {
		t.Fatalf("second upload verdict %+v", v)
	}
	if saved := b.control(protocol.ControlFileSaved); saved.File.Name != "photo (1).jpg" {
		t.Errorf("second upload saved as %q, want photo (1).jpg", saved.File.Name)
	}
	for _, name := range []string{"photo.jpg", "photo (1).jpg"} {
		if got, _ := os.ReadFile(filepath.Join(downloads, name)); !bytes.Equal(got, data) {
			t.Errorf("%s holds %d bytes, want the %d sent", name, len(got), len(data))
		}
	}

	for name, tc := range map[string]struct {
		header  protocol.Bulk
		data    []byte
		mutate  func(*protocol.Bulk)
		cancel  int
		verdict string
	}{
		"corrupt": {protocol.Bulk{Kind: protocol.BulkFile, Name: "c.bin", Size: 200 << 10}, noise(200 << 10),
			func(e *protocol.Bulk) { e.SHA256 = strings.Repeat("0", 64) }, 0, "SHA-256 mismatch"},
		"truncated": {protocol.Bulk{Kind: protocol.BulkFile, Name: "t.bin", Size: 300 << 10}, noise(200 << 10), nil, 0, "ended early"},
		"cancelled": {protocol.Bulk{Kind: protocol.BulkFile, Name: "x.bin", Size: 1 << 20}, noise(1 << 20), nil, 3, ""},
		"path":      {protocol.Bulk{Kind: protocol.BulkFile, Name: "../escape.sh", Size: 4}, []byte("echo"), nil, 0, "bad file name"},
		"control":   {protocol.Bulk{Kind: protocol.BulkFile, Name: "a\x1b[2Jb", Size: 4}, []byte("echo"), nil, 0, "control character"},
		"no space": {protocol.Bulk{Kind: protocol.BulkFile, Name: "huge.iso", Size: protocol.MaxFile}, nil, nil, 0,
			"not enough free space"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.verdict == "not enough free space" && freeSpace(t, downloads) > protocol.MaxFile+files.SpaceMargin {
				t.Skip("the download directory has room for the largest file")
			}
			v := b.upload(tc.header, tc.data, tc.mutate, tc.cancel)
			if tc.cancel > 0 {
				return
			}
			if v.T != protocol.BulkError || !strings.Contains(v.Error, tc.verdict) {
				t.Errorf("verdict %+v, want an error with %q", v, tc.verdict)
			}
		})
	}
	// Nothing but the two good uploads, once the writers have cleaned up.
	want := []string{"photo (1).jpg", "photo.jpg"}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(dirNames(t, downloads), want) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := dirNames(t, downloads); !slices.Equal(got, want) {
		t.Errorf("Downloads holds %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(downloads), "escape.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an upload escaped the download directory: %v", err)
	}

	h.stdin.Close()
	h.waitGone(ready.Display)
}

func freeSpace(t *testing.T, dir string) int64 {
	os.MkdirAll(dir, 0o700)
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func TestSessionOffersFiles(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	h := startAgent(t)
	ready := h.start(func(s *protocol.Start) {
		s.Session = `echo "$ROBIN_FILES_SOCK" > ` + envFile + `; command -v robin-send >> ` + envFile + `; exec sleep 1000`
	})
	runDir := runDirOf(t, h)
	sock := filepath.Join(runDir, files.SocketName)
	// The spawned desktop's programs find the socket and robin-send.
	deadline := time.Now().Add(10 * time.Second)
	var env []byte
	for len(bytes.Split(bytes.TrimSpace(env), []byte("\n"))) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		env, _ = os.ReadFile(envFile)
	}
	if want := sock + "\n" + filepath.Join(runDir, "bin", SendCommand) + "\n"; string(env) != want {
		t.Errorf("the session sees %q, want %q", env, want)
	}

	// An offer made before any page connects waits for one.
	content := noise(200 << 10)
	path := filepath.Join(t.TempDir(), "report.pdf")
	os.WriteFile(path, content, 0o600)
	res := offerFiles(t, sock, path)
	b := newFileBrowser(t, h)
	offered := b.control(protocol.ControlFileOffer).File
	if offered.Name != "report.pdf" || offered.Size != int64(len(content)) || offered.ID == "" {
		t.Fatalf("offer %+v", offered)
	}
	// Nothing comes before the page asks.
	select {
	case r := <-b.bulks:
		t.Fatalf("the agent sent %+v unasked", r.header)
	case <-time.After(500 * time.Millisecond):
	}
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: offered.ID}})
	got := b.bulk()
	if got.header.Kind != protocol.BulkFile || got.header.ID != offered.ID || got.header.Name != "report.pdf" || !bytes.Equal(got.data, content) {
		t.Errorf("fetched %+v with %d bytes, want the offered file", got.header, len(got.data))
	}
	select {
	case r := <-res:
		if r.err != nil || !strings.Contains(r.out, "report.pdf: saved by the browser") {
			t.Errorf("robin-agent send: %v, printed %q", r.err, r.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("robin-agent send did not return once the file was saved")
	}

	// Declined.
	res = offerFiles(t, sock, path)
	offered = b.control(protocol.ControlFileOffer).File
	b.sendControl(protocol.Control{T: protocol.ControlFileDecline, File: &protocol.File{ID: offered.ID}})
	if r := <-res; !errors.Is(r.err, files.ErrNotSaved) || !strings.Contains(r.out, "declined") {
		t.Errorf("a declined offer: %v, printed %q", r.err, r.out)
	}

	// Withdrawn: the sender goes, and the page hears of it.
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	req, _ := json.Marshal(map[string][]string{"names": {"report.pdf"}})
	if _, _, err := c.WriteMsgUnix(req, unix.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	offered = b.control(protocol.ControlFileOffer).File
	c.Close()
	if gone := b.control(protocol.ControlFileWithdraw).File; gone.ID != offered.ID {
		t.Errorf("withdrew %q, want %q", gone.ID, offered.ID)
	}
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: offered.ID}})
	select {
	case r := <-b.bulks:
		t.Errorf("a withdrawn offer was sent: %+v", r.header)
	case <-time.After(500 * time.Millisecond):
	}

	// Still open when the session ends: robin-agent send hears it ended.
	res = offerFiles(t, sock, path)
	b.control(protocol.ControlFileOffer)
	h.stdin.Close()
	if r := <-res; r.err == nil || !strings.Contains(r.err.Error(), "ended") {
		t.Errorf("robin-agent send at the end of the session: %v", r.err)
	}
	h.waitGone(ready.Display)
}

// onDisplay runs xclip on the session's display.
func xclipOn(runDir string, args ...string) *exec.Cmd {
	disp, _ := os.ReadFile(filepath.Join(runDir, "display"))
	cmd := exec.Command("xclip", append([]string{"-selection", "clipboard"}, args...)...)
	cmd.Env = append(os.Environ(), "DISPLAY="+strings.TrimSpace(string(disp)), "XAUTHORITY="+filepath.Join(runDir, "Xauthority"))
	return cmd
}

func TestSessionRichClipboard(t *testing.T) {
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Skip("no xclip")
	}
	h := startAgent(t)
	ready := h.start()
	runDir := runDirOf(t, h)
	b := newFileBrowser(t, h)

	// Browser to desktop: text, HTML and an image in one transfer.
	clip := protocol.Clip{Text: "from the page", HTML: "<b>from the page</b>", PNG: append([]byte("\x89PNG\r\n\x1a\n"), noise(400<<10)...)}
	payload, _ := protocol.EncodeClip(clip)
	if v := b.upload(protocol.Bulk{Kind: protocol.BulkClipboard, Size: int64(len(payload))}, payload, nil, 0); v.T != protocol.BulkDone {
		t.Fatalf("clipboard verdict %+v", v)
	}
	deadline := time.Now().Add(10 * time.Second)
	var png []byte
	for !bytes.Equal(png, clip.PNG) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		png, _ = xclipOn(runDir, "-o", "-t", "image/png").Output()
	}
	if !bytes.Equal(png, clip.PNG) {
		t.Errorf("the desktop pastes %d bytes of PNG, want %d", len(png), len(clip.PNG))
	}
	if html, _ := xclipOn(runDir, "-o", "-t", "text/html").Output(); string(html) != clip.HTML {
		t.Errorf("the desktop pastes HTML %q", html)
	}

	// Desktop to browser: an image as a bulk transfer.
	img := append([]byte("\x89PNG\r\n\x1a\n"), noise(1<<20)...)
	cmd := xclipOn(runDir, "-i", "-t", "image/png")
	cmd.Stdin = bytes.NewReader(img)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got := b.bulk()
	if got.header.Kind != protocol.BulkClipboard {
		t.Fatalf("the agent sent %+v", got.header)
	}
	if c, err := protocol.DecodeClip(got.data); err != nil || !bytes.Equal(c.PNG, img) || c.Text != "" {
		t.Errorf("the page got %d bytes of PNG and %q (%v), want the %d copied", len(c.PNG), c.Text, err, len(img))
	}

	// Text too long for a control message goes as bulk too; short text
	// as a control message.
	long := strings.Repeat("long text ", 40_000)
	cmd = xclipOn(runDir, "-i")
	cmd.Stdin = strings.NewReader(long)
	cmd.Run()
	if c, _ := protocol.DecodeClip(b.bulk().data); c.Text != long {
		t.Errorf("long text arrived as %d bytes, want %d", len(c.Text), len(long))
	}
	cmd = xclipOn(runDir, "-i")
	cmd.Stdin = strings.NewReader("short")
	cmd.Run()
	if c := b.control(protocol.ControlClipboard); c.Text != "short" {
		t.Errorf("short text arrived as %q", c.Text)
	}

	h.stdin.Close()
	h.waitGone(ready.Display)
}
