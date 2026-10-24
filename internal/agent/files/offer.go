//go:build linux

package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/protocol"
)

// SocketName is the offer socket's name in a session's run directory.
const SocketName = "files.sock"

// SocketEnv names the variable that tells robin-agent send which session
// to offer files to. A spawned session sets it for its programs.
const SocketEnv = "ROBIN_FILES_SOCK"

const (
	// maxOffer bounds the files one request may offer, well under the 253
	// descriptors Linux passes in one message.
	maxOffer = 64
	// maxRequest bounds a request's names.
	maxRequest = 64 << 10
)

// requestTimeout bounds how long a connection may take to say what it
// offers, and a report to it. A variable so that tests need not wait it out.
var requestTimeout = 5 * time.Second

// Status is what became of an offered file, as robin-agent send hears it.
type Status string

// Statuses. Offered and Failed leave the file on offer; the rest are final.
const (
	Offered  Status = "offered"
	Failed   Status = "failed" // a transfer failed; the offer stands
	Saved    Status = "saved"
	Declined Status = "declined"
	Refused  Status = "refused" // never offered: not a regular file, say
)

// Final reports whether s settles the file's fate.
func (s Status) Final() bool { return s == Saved || s == Declined || s == Refused }

// request is what robin-agent send writes, alongside one descriptor per
// name.
type request struct {
	Names []string `json:"names"`
}

// report is what the session writes back: news of file I, or with I of -1
// why the whole request was refused.
type report struct {
	I      int    `json:"i"`
	Status Status `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Listener takes offers from robin-agent send, as one sequenced-packet
// unix socket in the session's private run directory. Only the session's
// own user may offer: the socket is 0600, and each connection's peer
// credentials must name that user too.
type Listener struct {
	ln  *net.UnixListener
	uid int
	log *slog.Logger
	wg  sync.WaitGroup
}

// Listen opens the offer socket at path.
func Listen(path string, log *slog.Logger) (*Listener, error) {
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("files: %w", err)
	}
	return &Listener{ln: ln, uid: os.Getuid(), log: log}, nil
}

// Serve hands each request to handle, on a goroutine of its own, until
// Close. handle owns the request and the files in it.
func (l *Listener) Serve(handle func(*Request)) {
	defer l.wg.Wait()
	for {
		c, err := l.ln.AcceptUnix()
		if err != nil {
			return
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			r, err := l.read(c)
			if err != nil {
				l.log.Info("offer refused", "err", err)
				if !errors.Is(err, errStranger) {
					writeReport(c, report{I: -1, Detail: err.Error()})
				}
				c.Close()
				return
			}
			handle(r)
		}()
	}
}

// Close stops Serve and removes the socket.
func (l *Listener) Close() error { return l.ln.Close() }

var errStranger = errors.New("files: offer from another user")

func (l *Listener) read(c *net.UnixConn) (*Request, error) {
	if err := l.checkPeer(c); err != nil {
		return nil, err
	}
	c.SetReadDeadline(time.Now().Add(requestTimeout))
	buf := make([]byte, maxRequest)
	oob := make([]byte, unix.CmsgSpace(maxOffer*4))
	n, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	fds := rights(oob[:oobn])
	closeAll := func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}
	switch {
	case err != nil:
		closeAll()
		return nil, fmt.Errorf("files: read the offer: %w", err)
	case flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0:
		closeAll()
		return nil, fmt.Errorf("files: more than %d files or %d bytes of names in one offer", maxOffer, maxRequest)
	}
	c.SetReadDeadline(time.Time{})
	var req request
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		closeAll()
		return nil, fmt.Errorf("files: malformed offer: %w", err)
	}
	if len(req.Names) == 0 || len(req.Names) != len(fds) {
		closeAll()
		return nil, fmt.Errorf("files: malformed offer: %d names for %d files", len(req.Names), len(fds))
	}
	r := &Request{conn: c, gone: make(chan struct{})}
	for i, name := range req.Names {
		r.Files = append(r.Files, offered(name, os.NewFile(uintptr(fds[i]), name)))
	}
	go r.watch()
	return r, nil
}

func (l *Listener) checkPeer(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	cerr := raw.Control(func(fd uintptr) {
		cred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if cerr != nil {
		return cerr
	}
	if err != nil {
		return err
	}
	if int(cred.Uid) != l.uid {
		return fmt.Errorf("%w: uid %d", errStranger, cred.Uid)
	}
	return nil
}

// rights collects the descriptors in a message's control data.
func rights(oob []byte) []int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var fds []int
	for _, m := range msgs {
		if got, err := unix.ParseUnixRights(&m); err == nil {
			fds = append(fds, got...)
		}
	}
	return fds
}

// offered checks one file of a request: a regular file within
// protocol.MaxFile, under a name CheckName accepts.
func offered(name string, f *os.File) Offer {
	o := Offer{Name: name, File: f}
	st, err := f.Stat()
	switch {
	case err != nil:
		o.Err = err
	case !st.Mode().IsRegular():
		o.Err = errors.New("not a regular file")
	case st.Size() > protocol.MaxFile:
		o.Err = fmt.Errorf("%s, over the %s a browser takes", humanSize(st.Size()), humanSize(protocol.MaxFile))
	default:
		o.Err = CheckName(name)
		o.Size = st.Size()
	}
	if o.Err != nil {
		f.Close()
		o.File = nil
	}
	return o
}

// Request is one robin-agent send: the files it offers, and the line back
// to it.
type Request struct {
	Files []Offer

	conn *net.UnixConn
	mu   sync.Mutex
	gone chan struct{}
}

// Offer is one file of a request. File is nil, and Err says why, when it
// cannot be offered; otherwise the handler owns File.
type Offer struct {
	Name string
	Size int64
	File *os.File
	Err  error
}

// Tell lets robin-agent send know what became of file i.
func (r *Request) Tell(i int, s Status, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	writeReport(r.conn, report{I: i, Status: s, Detail: detail})
}

// Gone is closed once robin-agent send has gone, or Close was called.
func (r *Request) Gone() <-chan struct{} { return r.gone }

// Close hangs up on robin-agent send.
func (r *Request) Close() { r.conn.Close() }

// watch waits for robin-agent send to hang up. It sends nothing after the
// request, so anything a read returns means it has gone.
func (r *Request) watch() {
	var b [1]byte
	r.conn.Read(b[:])
	close(r.gone)
}

func writeReport(c *net.UnixConn, rep report) {
	b, err := json.Marshal(rep)
	if err != nil {
		return
	}
	c.SetWriteDeadline(time.Now().Add(requestTimeout))
	c.Write(b)
}
