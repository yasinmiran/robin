//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/files"
	"github.com/yasinmiran/robin/internal/protocol"
)

// sharedSession is a session with files and no browser, its home in a
// fresh directory.
func sharedSession(t *testing.T) (*session, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	runDir := t.TempDir()
	fs, err := openFiles(runDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s := testSession(&fakeDesktop{})
	s.files = fs
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); s.serveFiles(ctx) }()
	t.Cleanup(func() {
		stop()
		<-served
		fs.close()
	})
	return s, runDir
}

func TestFilesEnvForASpawnedDesktop(t *testing.T) {
	s, runDir := sharedSession(t)
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
	s, runDir := sharedSession(t)
	sock := filepath.Join(runDir, files.SocketName)
	res := offerFiles(t, sock, hostFile(t, "a.txt", "a"), t.TempDir())

	// A directory never reaches the session: Send refuses it first.
	if r := <-res; r.err == nil || !strings.Contains(r.err.Error(), "not a regular file") {
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
	select {
	case r := <-res:
		if !errors.Is(r.err, files.ErrNotSaved) || strings.Count(r.out, "declined in the browser") != 2 {
			t.Errorf("Send = %v, printed %q", r.err, r.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return once both files were declined")
	}
	openOffers(t, s, 0)
}

func TestOffersEndWithTheSession(t *testing.T) {
	s, runDir := sharedSession(t)
	res := offerFiles(t, filepath.Join(runDir, files.SocketName), hostFile(t, "keep.bin", "k"))
	o := openOffers(t, s, 1)[0]
	s.files.close()
	select {
	case r := <-res:
		if r.err == nil || !strings.Contains(r.err.Error(), "ended") {
			t.Errorf("Send when the session ended: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return when the session ended")
	}
	if _, err := o.file.Stat(); err == nil {
		t.Error("the offered file is still open")
	}
}

func TestUploadThroughTheSession(t *testing.T) {
	s, _ := sharedSession(t)
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
