//go:build linux

package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/protocol"
)

// serve listens in a fresh directory and hands each request to the
// returned channel. configure may change the listener before it serves.
func serve(t *testing.T, configure ...func(*Listener)) (*Listener, string, <-chan *Request) {
	t.Helper()
	path := filepath.Join(t.TempDir(), SocketName)
	l, err := Listen(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configure {
		c(l)
	}
	reqs := make(chan *Request, 4)
	done := make(chan struct{})
	go func() { defer close(done); l.Serve(func(r *Request) { reqs <- r }) }()
	t.Cleanup(func() {
		l.Close()
		<-done
	})
	return l, path, reqs
}

func next(t *testing.T, reqs <-chan *Request) *Request {
	t.Helper()
	select {
	case r := <-reqs:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no request arrived")
		return nil
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type sendResult struct {
	err error
	out string
}

// sendAsync runs Send until it returns or ctx ends.
func sendAsync(ctx context.Context, sock string, paths ...string) <-chan sendResult {
	res := make(chan sendResult, 1)
	go func() {
		var out bytes.Buffer
		err := Send(ctx, sock, paths, &out)
		res <- sendResult{err, out.String()}
	}()
	return res
}

func result(t *testing.T, res <-chan sendResult) sendResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return")
		return sendResult{}
	}
}

func TestListenerSocketIsPrivate(t *testing.T) {
	_, path, _ := serve(t)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v, want a 0600 socket", st.Mode())
	}
}

func TestSendOffersFilesAndFollowsThem(t *testing.T) {
	_, sock, reqs := serve(t)
	a := writeFile(t, "a.txt", "alpha")
	b := writeFile(t, "b.bin", "bravo!")
	res := sendAsync(t.Context(), sock, a, b)

	r := next(t, reqs)
	if len(r.Files) != 2 {
		t.Fatalf("request offers %d files", len(r.Files))
	}
	for i, want := range []struct {
		name, content string
	}{{"a.txt", "alpha"}, {"b.bin", "bravo!"}} {
		f := r.Files[i]
		if f.Err != nil || f.Name != want.name || f.Size != int64(len(want.content)) {
			t.Fatalf("file %d: %+v", i, f)
		}
		// The session reads the very file the sender opened.
		got, _ := io.ReadAll(f.File)
		if string(got) != want.content {
			t.Errorf("file %d reads %q", i, got)
		}
		f.File.Close()
	}
	r.Tell(0, Offered, "")
	r.Tell(1, Offered, "")
	r.Tell(1, Failed, "the browser went away\x1b[2J")
	r.Tell(0, Saved, "")
	r.Tell(0, Declined, "") // after the fact: ignored
	r.Tell(7, Saved, "")    // no such file: ignored
	r.Tell(1, Saved, "")

	got := result(t, res)
	if got.err != nil {
		t.Fatalf("Send: %v\n%s", got.err, got.out)
	}
	want := "a.txt: offered; click Save in the browser to download it, or press Ctrl+C here to withdraw it\n" +
		"b.bin: offered; click Save in the browser to download it, or press Ctrl+C here to withdraw it\n" +
		"b.bin: transfer failed, still on offer: the browser went away\uFFFD[2J\n" +
		"a.txt: saved by the browser\n" +
		"b.bin: saved by the browser\n"
	if got.out != want {
		t.Errorf("Send printed:\n%s\nwant:\n%s", got.out, want)
	}
	select {
	case <-r.Gone():
	case <-time.After(5 * time.Second):
		t.Error("the request never noticed Send leave")
	}
}

func TestSendReportsWhatWasNotSaved(t *testing.T) {
	_, sock, reqs := serve(t)
	res := sendAsync(t.Context(), sock, writeFile(t, "a", "1"), writeFile(t, "b", "2"))
	r := next(t, reqs)
	r.Tell(0, Declined, "")
	r.Tell(1, Refused, "not a regular file")
	got := result(t, res)
	if !errors.Is(got.err, ErrNotSaved) {
		t.Errorf("Send = %v, want ErrNotSaved", got.err)
	}
	if want := "a: declined in the browser\nb: not offered: not a regular file\n"; got.out != want {
		t.Errorf("Send printed %q, want %q", got.out, want)
	}
}

func TestSendWhenTheSessionEnds(t *testing.T) {
	_, sock, reqs := serve(t)
	res := sendAsync(t.Context(), sock, writeFile(t, "a", "1"))
	r := next(t, reqs)
	r.Tell(0, Offered, "")
	r.Close()
	if got := result(t, res); got.err == nil || !strings.Contains(got.err.Error(), "ended") {
		t.Errorf("Send after the session hung up: %v", got.err)
	}
}

// Ending Send's context hangs up, so the session withdraws the offer.
func TestSendStopsWithItsContext(t *testing.T) {
	_, sock, reqs := serve(t)
	ctx, stop := context.WithCancel(t.Context())
	res := sendAsync(ctx, sock, writeFile(t, "a", "1"))
	r := next(t, reqs)
	r.Tell(0, Offered, "")
	stop()
	if got := result(t, res); got.err == nil || !strings.Contains(got.err.Error(), "withdrawn") {
		t.Errorf("Send once its context ended: %v", got.err)
	}
	select {
	case <-r.Gone():
	case <-time.After(5 * time.Second):
		t.Error("the session never heard Send hang up")
	}
}

func TestSendRefusesWhatItCannotOffer(t *testing.T) {
	_, sock, _ := serve(t)
	dir := t.TempDir()
	for name, paths := range map[string][]string{
		"none":      nil,
		"directory": {dir},
		"missing":   {filepath.Join(dir, "missing")},
		"too many":  make([]string, maxOffer+1),
	} {
		var out bytes.Buffer
		if err := Send(t.Context(), sock, paths, &out); err == nil || errors.Is(err, ErrNotSaved) {
			t.Errorf("%s: Send = %v", name, err)
		}
	}
	if err := Send(t.Context(), filepath.Join(dir, "no.sock"), []string{writeFile(t, "a", "1")}, io.Discard); err == nil {
		t.Error("Send to no session succeeded")
	}
}

// terminalSafe fails t when s holds anything that would act on a terminal
// rather than show: control characters other than newlines, and the
// invisible marks that reorder text.
func terminalSafe(t *testing.T, what, s string) {
	t.Helper()
	for _, r := range s {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)) {
			t.Errorf("%s holds %U: %q", what, r, s)
			return
		}
	}
}

// A name that holds escapes is refused by the session, and robin-send
// says so without sending the escapes to the terminal it runs in.
func TestSendPrintsNamesSafely(t *testing.T) {
	_, sock, reqs := serve(t)
	evil := "a\x1b]0;owned\x07b\u202e.txt"
	res := sendAsync(t.Context(), sock, writeFile(t, evil, "x"))
	r := next(t, reqs)
	r.Tell(0, Refused, "bad name\x1b[2J")
	got := result(t, res)
	if !errors.Is(got.err, ErrNotSaved) {
		t.Errorf("Send = %v, want ErrNotSaved", got.err)
	}
	terminalSafe(t, "the report", got.out)
	if !strings.Contains(got.out, `a\x1b]0;owned\ab\u202e.txt`) {
		t.Errorf("the report %q does not show the name escaped", got.out)
	}

	dir := filepath.Join(t.TempDir(), evil)
	os.Mkdir(dir, 0o700)
	for what, path := range map[string]string{"a directory": dir, "a missing file": dir + "-missing"} {
		err := Send(t.Context(), sock, []string{path}, io.Discard)
		if err == nil {
			t.Fatalf("Send of %s succeeded", what)
		}
		terminalSafe(t, "the error for "+what, err.Error())
	}
}

// rawOffer sends names with the descriptors of files and returns the
// session's first report. It may run on a goroutine of its own.
func rawOffer(t *testing.T, sock string, payload []byte, files ...*os.File) report {
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		t.Error(err)
		return report{I: -2}
	}
	defer c.Close()
	var oob []byte
	if len(files) > 0 {
		var fds []int
		for _, f := range files {
			fds = append(fds, int(f.Fd()))
		}
		oob = unix.UnixRights(fds...)
	}
	if _, _, err := c.WriteMsgUnix(payload, oob, nil); err != nil {
		t.Error(err)
		return report{I: -2}
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return report{I: -2, Detail: err.Error()}
	}
	var rep report
	json.Unmarshal(buf[:n], &rep)
	return rep
}

func TestListenerChecksEachFile(t *testing.T) {
	_, sock, reqs := serve(t)
	dir, _ := os.Open(t.TempDir())
	defer dir.Close()
	huge, _ := os.Create(filepath.Join(t.TempDir(), "huge"))
	defer huge.Close()
	if err := huge.Truncate(protocol.MaxFile + 1); err != nil {
		t.Fatal(err)
	}
	ok, _ := os.Open(writeFile(t, "ok", "x"))
	defer ok.Close()
	badName, _ := os.Open(writeFile(t, "bad", "x"))
	defer badName.Close()

	payload, _ := json.Marshal(request{Names: []string{"dir", "huge", "ok", "evil\nname"}})
	sent := make(chan report, 1)
	go func() { sent <- rawOffer(t, sock, payload, dir, huge, ok, badName) }()
	r := next(t, reqs)
	defer func() {
		r.Close()
		<-sent
	}()
	for i, want := range []string{"not a regular file", "over the 4.0 GiB", "", "control character"} {
		f := r.Files[i]
		if want == "" {
			if f.Err != nil || f.File == nil {
				t.Errorf("file %d: %+v, want it offered", i, f)
			}
			continue
		}
		if f.Err == nil || !strings.Contains(f.Err.Error(), want) || f.File != nil {
			t.Errorf("file %d: %+v, want refused with %q", i, f, want)
		}
	}
}

func TestListenerRefusesMalformedOffers(t *testing.T) {
	_, sock, reqs := serve(t)
	f, _ := os.Open(writeFile(t, "a", "x"))
	defer f.Close()
	two, _ := json.Marshal(request{Names: []string{"a", "b"}})
	one, _ := json.Marshal(request{Names: []string{"a"}})
	for name, tc := range map[string]struct {
		payload []byte
		files   []*os.File
		want    string
	}{
		"not json":         {[]byte("{"), []*os.File{f}, "malformed"},
		"no files":         {one, nil, "1 names for 0 files"},
		"names, one file":  {two, []*os.File{f}, "2 names for 1 files"},
		"no names":         {[]byte(`{"names":[]}`), []*os.File{f}, "0 names"},
		"names too long":   {bytes.Repeat([]byte("x"), maxRequest+1), []*os.File{f}, "in one offer"},
		"too many to pass": {one, filesOf(f, maxOffer+1), "in one offer"},
	} {
		t.Run(name, func(t *testing.T) {
			rep := rawOffer(t, sock, tc.payload, tc.files...)
			if rep.I != -1 || !strings.Contains(rep.Detail, tc.want) {
				t.Errorf("report %+v, want the offer refused with %q", rep, tc.want)
			}
		})
	}
	select {
	case r := <-reqs:
		t.Errorf("a malformed offer reached the handler: %+v", r.Files)
	default:
	}
}

func filesOf(f *os.File, n int) []*os.File {
	out := make([]*os.File, n)
	for i := range out {
		out[i] = f
	}
	return out
}

func TestListenerRefusesOtherUsers(t *testing.T) {
	// As if the socket's owner were someone else.
	_, sock, reqs := serve(t, func(l *Listener) { l.uid = os.Getuid() + 1 })
	res := sendAsync(t.Context(), sock, writeFile(t, "a", "1"))
	if got := result(t, res); got.err == nil || got.out != "" {
		t.Errorf("Send from another user: %v, printed %q", got.err, got.out)
	}
	select {
	case <-reqs:
		t.Error("an offer from another user reached the handler")
	default:
	}
}

func TestListenerTimesOutSilentClients(t *testing.T) {
	l, sock, _ := serve(t, func(l *Listener) { l.timeout = 200 * time.Millisecond })
	c, err := net.Dial("unixpacket", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(l.timeout + 3*time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	var rep report
	if err == nil {
		json.Unmarshal(buf[:n], &rep)
	}
	if rep.I != -1 || !strings.Contains(rep.Detail, "read the offer") {
		t.Errorf("a client that says nothing got %+v, %v", rep, err)
	}
}

func TestFindSocket(t *testing.T) {
	root := t.TempDir()
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == SocketEnv {
				return v
			}
			return ""
		}
	}
	if got, err := FindSocket(env("/run/x/files.sock"), root); err != nil || got != "/run/x/files.sock" {
		t.Errorf("with %s set: %q, %v", SocketEnv, got, err)
	}
	if _, err := FindSocket(env(""), root); err == nil || !strings.Contains(err.Error(), "no robin session") {
		t.Errorf("with no session: %v", err)
	}

	// A socket nobody listens on any more is no session.
	os.Mkdir(filepath.Join(root, "1"), 0o700)
	stale := filepath.Join(root, "1", SocketName)
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: stale, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if _, err := FindSocket(env(""), root); err == nil {
		t.Error("a stale socket counts as a session")
	}

	live := func(dir string) {
		os.Mkdir(filepath.Join(root, dir), 0o700)
		l, err := Listen(filepath.Join(root, dir, SocketName), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		go l.Serve(func(r *Request) { r.Close() })
		t.Cleanup(func() { l.Close() })
	}
	live("2")
	if got, err := FindSocket(env(""), root); err != nil || got != filepath.Join(root, "2", SocketName) {
		t.Errorf("with one live session: %q, %v", got, err)
	}
	live("3")
	if _, err := FindSocket(env(""), root); err == nil || !strings.Contains(err.Error(), "2 robin sessions") {
		t.Errorf("with two live sessions: %v", err)
	}
}

func TestStatusFinal(t *testing.T) {
	for s, want := range map[Status]bool{Offered: false, Failed: false, Saved: true, Declined: true, Refused: true} {
		if s.Final() != want {
			t.Errorf("%s.Final() = %v", s, !want)
		}
	}
	if got := describe("x", report{Status: "odd", Detail: "d"}); got != "x: odd d" {
		t.Errorf("describe of an unknown status: %q", got)
	}
}
