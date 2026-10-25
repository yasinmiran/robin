//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/files"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// sharedSession is a session of desk with files and no browser, its home
// in a fresh directory. end ends it as Run does, and the test's end does
// too.
func sharedSession(t *testing.T, desk desktop.Desktop) (s *session, runDir string, end func()) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	runDir = t.TempDir()
	fs, err := openFiles(runDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s = testSession(desk)
	s.files = fs
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); s.serveFiles(ctx) }()
	var once sync.Once
	end = func() {
		once.Do(func() {
			stop()
			<-served
			fs.close()
		})
	}
	t.Cleanup(end)
	return s, runDir, end
}

// viewOnlyDesktop is a desktop whose host allowed viewing only.
type viewOnlyDesktop struct{ *fakeDesktop }

func (viewOnlyDesktop) ViewOnly() bool { return true }

// waitSent is what robin-agent send's offer returned, or a failure after
// a while.
func waitSent(t *testing.T, res <-chan sent) sent {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("robin-agent send did not return")
		return sent{}
	}
}

func TestFilesEnvForASpawnedDesktop(t *testing.T) {
	s, runDir, _ := sharedSession(t, &fakeDesktop{})
	env := s.files.env()
	if want := files.SocketEnv + "=" + filepath.Join(runDir, files.SocketName); env[0] != want {
		t.Errorf("env %q, want %q", env[0], want)
	}
	if want := "PATH=" + filepath.Join(runDir, "bin") + ":"; !strings.HasPrefix(env[1], want) || len(env[1]) == len(want) {
		t.Errorf("env %q, want robin-send's directory first on the PATH", env[1])
	}
	t.Setenv("PATH", "")
	if got := s.files.env()[1]; !strings.HasSuffix(got, ":/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("with no PATH: %q", got)
	}
	link, err := os.Readlink(filepath.Join(runDir, "bin", SendCommand))
	exe, _ := os.Executable()
	if err != nil || link != exe {
		t.Errorf("%s links to %q (%v), want %q", SendCommand, link, err, exe)
	}
	if want := filepath.Join(os.Getenv("HOME"), "Downloads"); s.files.inbox.Dir() != want {
		t.Errorf("uploads go to %q, want %q", s.files.inbox.Dir(), want)
	}
}

func TestOpenFilesFails(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := openFiles(filepath.Join(t.TempDir(), "missing"), log); err == nil {
		t.Error("openFiles in a missing run dir succeeded")
	}
	runDir := t.TempDir()
	os.WriteFile(filepath.Join(runDir, "bin"), nil, 0o600)
	if _, err := openFiles(runDir, log); err == nil {
		t.Error("openFiles with bin taken succeeded")
	}
	if _, err := os.Stat(filepath.Join(runDir, files.SocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed openFiles left its socket: %v", err)
	}
	t.Setenv("HOME", "")
	if _, err := openFiles(t.TempDir(), log); err == nil {
		t.Error("openFiles without a home succeeded")
	}
}

type sent struct {
	err error
	out string
}

// offerFiles runs robin-agent send's offer until it returns or the test
// ends.
func offerFiles(t *testing.T, sock string, paths ...string) <-chan sent {
	res := make(chan sent, 1)
	go func() {
		var out bytes.Buffer
		err := files.Send(t.Context(), sock, paths, &out)
		res <- sent{err, out.String()}
	}()
	return res
}

func openOffers(t *testing.T, s *session, n int) []*offer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.files.mu.Lock()
		var list []*offer
		for _, o := range s.files.offers {
			list = append(list, o)
		}
		s.files.mu.Unlock()
		if len(list) == n {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d offers open, want %d", len(list), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hostFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	os.WriteFile(p, []byte(content), 0o600)
	return p
}

func TestOffersSettleOnDecline(t *testing.T) {
	s, runDir, _ := sharedSession(t, &fakeDesktop{})
	sock := filepath.Join(runDir, files.SocketName)
	res := offerFiles(t, sock, hostFile(t, "a.txt", "a"), t.TempDir())

	// A directory never reaches the session: Send refuses it first.
	if r := waitSent(t, res); r.err == nil || !strings.Contains(r.err.Error(), "not a regular file") {
		t.Fatalf("offering a directory: %v", r.err)
	}
	res = offerFiles(t, sock, hostFile(t, "a.txt", "a"), hostFile(t, "b.txt", "bb"))
	offers := openOffers(t, s, 2)
	for _, o := range offers {
		if o.info.ID == "" || o.info.Size != map[string]int64{"a.txt": 1, "b.txt": 2}[o.info.Name] {
			t.Errorf("offer %+v", o.info)
		}
	}
	// Without a browser a fetch does nothing, and the offer stands.
	s.fetchOffer(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: offers[0].info.ID}})
	s.resendOffers()
	openOffers(t, s, 2)

	s.declineOffer(protocol.Control{T: protocol.ControlFileDecline, File: &protocol.File{ID: "unknown"}})
	s.declineOffer(protocol.Control{T: protocol.ControlFileDecline})
	for _, o := range offers {
		s.declineOffer(protocol.Control{T: protocol.ControlFileDecline, File: &protocol.File{ID: o.info.ID}})
	}
	if r := waitSent(t, res); !errors.Is(r.err, files.ErrNotSaved) || strings.Count(r.out, "declined in the browser") != 2 {
		t.Errorf("Send = %v, printed %q", r.err, r.out)
	}
	openOffers(t, s, 0)
}

// A sender that goes is hung up on, rather than its connection waiting for
// the garbage collector.
func TestGoneSenderIsHungUp(t *testing.T) {
	s, runDir, _ := sharedSession(t, &fakeDesktop{})
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: filepath.Join(runDir, files.SocketName), Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := os.Open(hostFile(t, "a.txt", "a"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(map[string][]string{"names": {"a.txt"}})
	if _, _, err := c.WriteMsgUnix(req, unix.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	openOffers(t, s, 1)
	// Gone, as far as the session can tell, yet still able to hear it.
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	openOffers(t, s, 0)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	for {
		_, err := c.Read(buf)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("the session kept the connection of a sender that went: %v", err)
		}
	}
}

// A host that allowed viewing only gets no files from the page.
func TestViewOnlyTakesNoUploads(t *testing.T) {
	s, _, _ := sharedSession(t, viewOnlyDesktop{&fakeDesktop{}})
	b := peerBrowser(t, s)
	data := noise(64 << 10)
	v := b.upload(protocol.Bulk{Kind: protocol.BulkFile, ID: "v1", Name: "notes.txt", Size: int64(len(data))}, data, nil, 0)
	if v.T != protocol.BulkError || !strings.Contains(v.Error, "viewing only") {
		t.Errorf("an upload to a view-only session: %+v, want refused", v)
	}
	if names := dirNames(t, s.files.inbox.Dir()); len(names) != 0 {
		t.Errorf("Downloads holds %q", names)
	}
	if _, err := s.acceptUpload(protocol.Bulk{Kind: protocol.BulkFile, Name: "x", Size: 1}); err == nil {
		t.Error("a view-only session took an upload")
	}
}

func TestOffersEndWithTheSession(t *testing.T) {
	s, runDir, end := sharedSession(t, &fakeDesktop{})
	res := offerFiles(t, filepath.Join(runDir, files.SocketName), hostFile(t, "keep.bin", "k"))
	o := openOffers(t, s, 1)[0]
	end()
	if r := waitSent(t, res); r.err == nil || !strings.Contains(r.err.Error(), "ended") {
		t.Errorf("Send when the session ended: %v", r.err)
	}
	if _, err := o.file.Stat(); err == nil {
		t.Error("the offered file is still open")
	}
}

func TestUploadThroughTheSession(t *testing.T) {
	s, _, _ := sharedSession(t, &fakeDesktop{})
	sink, err := s.acceptUpload(protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkFile, ID: "u1", Name: "notes.txt", Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	sink.Write([]byte("hello"))
	if err := sink.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "Downloads", "notes.txt"))
	if string(got) != "hello" {
		t.Errorf("Downloads/notes.txt = %q, %v", got, err)
	}
	if _, err := s.acceptUpload(protocol.Bulk{Name: "../x", Size: 1}); !errors.Is(err, files.ErrName) {
		t.Errorf("an upload named ../x: %v", err)
	}
	if _, err := testSession(&fakeDesktop{}).acceptUpload(protocol.Bulk{Name: "x", Size: 1}); err == nil {
		t.Error("a session without files took an upload")
	}
}

// robin-send prints the browser's reason for refusing a transfer as its
// own, and any other failure as it is.
func TestFailureGivesTheBrowsersReason(t *testing.T) {
	refused := fmt.Errorf("send: %w", &peer.BulkRefusedError{Reason: "no room"})
	if got, want := failure(refused), "the browser refused it: no room"; got != want {
		t.Errorf("refused: %q, want %q", got, want)
	}
	if got, want := failure(errors.New("peer: bulk transfer cancelled")), "peer: bulk transfer cancelled"; got != want {
		t.Errorf("cancelled: %q, want %q", got, want)
	}
}

// rawOffer offers path n times over one connection, under names f0, f1 and
// so on, as robin-agent send would, and stays connected.
func rawOffer(t *testing.T, sock, path string, n int) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var names []string
	var fds []int
	for i := range n {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		names = append(names, "f"+strconv.Itoa(i))
		fds = append(fds, int(f.Fd()))
	}
	req, _ := json.Marshal(map[string][]string{"names": names})
	if _, _, err := c.WriteMsgUnix(req, unix.UnixRights(fds...), nil); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestOffersAreBounded keeps offering files without hanging up: a request
// that would bring more than maxSessionOffers on offer is refused whole,
// its descriptors closed, even when part of it would fit, and once a
// sender goes there is room again.
func TestOffersAreBounded(t *testing.T) {
	s, runDir, _ := sharedSession(t, &fakeDesktop{})
	sock := filepath.Join(runDir, files.SocketName)
	path := hostFile(t, "a.txt", "a")
	var senders []*net.UnixConn
	for range maxSessionOffers/files.MaxOffer - 1 {
		senders = append(senders, rawOffer(t, sock, path, files.MaxOffer))
	}
	rawOffer(t, sock, path, files.MaxOffer-1)
	openOffers(t, s, maxSessionOffers-1)

	refused(t, rawOffer(t, sock, path, 2), "offering 2 more")
	openOffers(t, s, maxSessionOffers-1)
	rawOffer(t, sock, path, 1)
	openOffers(t, s, maxSessionOffers)
	refused(t, rawOffer(t, sock, path, 1), "offering 1 more")
	openOffers(t, s, maxSessionOffers)

	if err := senders[0].CloseWrite(); err != nil {
		t.Fatal(err)
	}
	openOffers(t, s, maxSessionOffers-files.MaxOffer)
	rawOffer(t, sock, path, 1)
	openOffers(t, s, maxSessionOffers-files.MaxOffer+1)
}

// refused checks that the session refused c's whole request, saying want,
// and hung up.
func refused(t *testing.T, c *net.UnixConn, want string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no answer to an offer past the bound: %v", err)
	}
	var rep struct {
		I      int    `json:"i"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(buf[:n], &rep); err != nil || rep.I != -1 || !strings.Contains(rep.Detail, want) {
		t.Errorf("an offer past the bound got %s, want a refusal of the request saying %q", buf[:n], want)
	}
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("the refused sender was not hung up on: %v", err)
	}
}
