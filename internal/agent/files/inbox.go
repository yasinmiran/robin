//go:build linux

package files

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// queueChunks bounds what an upload holds for its writer: 256 chunks,
	// 16 MiB at the page's 64 KiB, which a disk that keeps up never fills.
	queueChunks = 256
	// SpaceMargin is left free on the download directory's filesystem, so
	// an upload cannot fill the disk the desktop runs on.
	SpaceMargin = 256 << 20
	// maxUploads bounds the uploads in progress at once, and with the queue
	// the memory they hold.
	maxUploads = 4
	// diskTimeout bounds how long Accept and Commit wait on the disk, since
	// they hold the transfer's read loop meanwhile.
	diskTimeout = 5 * time.Second
	// closeTimeout bounds how long Close waits for writers stuck on a disk.
	closeTimeout = 5 * time.Second
	// maxNumbered bounds the " (n)" names tried before giving up.
	maxNumbered = 999
	// tempPrefix starts the name a file has until it is whole.
	tempPrefix = ".robin-upload-"
)

// Reasons an upload fails.
var (
	ErrNoSpace  = errors.New("files: not enough free space")
	ErrSlowDisk = errors.New("files: the host's disk is not keeping up with the upload")
	ErrBusy     = errors.New("files: too many uploads at once")
	ErrClosed   = errors.New("files: the session is ending")
	errAborted  = errors.New("files: upload cancelled")
)

// Inbox takes uploads into one directory.
type Inbox struct {
	dir    string
	margin int64
	log    *slog.Logger
	// The disk, through calls a test can replace, and how long a wait on it
	// may hold the read loop.
	statfs  func(fd int, st *unix.Statfs_t) error
	create  func(dir int, name string) (*os.File, error)
	rename  func(olddirfd int, oldpath string, newdirfd int, newpath string, flags uint) error
	timeout time.Duration

	mu       sync.Mutex
	uploads  map[*Upload]struct{}
	reserved int64 // bytes still to come to the uploads in progress
	closed   bool
	wg       sync.WaitGroup
}

// NewInbox takes uploads into dir, creating it when the first one comes.
func NewInbox(dir string, log *slog.Logger) *Inbox {
	return &Inbox{
		dir: dir, margin: SpaceMargin, log: log,
		statfs: unix.Fstatfs, create: createTemp, rename: unix.Renameat2, timeout: diskTimeout,
		uploads: map[*Upload]struct{}{},
	}
}

// Dir is where uploads land.
func (in *Inbox) Dir() string { return in.dir }

// Accept starts an upload of size bytes to be called name, or says why it
// cannot: a bad name, too little space, too many uploads at once, or a
// disk that does not answer within the inbox's timeout. A name CheckName
// refuses is bad, and so is one that starts with a dot: it would be
// hidden, and in a home directory could be a startup file. The bytes go
// to a file of a temporary name, opened so that it can be neither an
// existing file nor a symlink, and the file gets its name only once the
// whole upload is in.
func (in *Inbox) Accept(name string, size int64) (*Upload, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("%w: %q would be a hidden file", ErrName, name)
	}
	u := &Upload{
		in: in, name: name, size: size, tmp: tempPrefix + rand.Text()[:16],
		queue: make(chan []byte, queueChunks), cancel: make(chan struct{}), done: make(chan struct{}),
	}
	// The slot and the bytes are the upload's from here, while the disk is
	// asked outside the lock, so that Close is never held up behind it.
	in.mu.Lock()
	switch n := len(in.uploads); {
	case in.closed:
		in.mu.Unlock()
		return nil, ErrClosed
	case n >= maxUploads:
		in.mu.Unlock()
		return nil, fmt.Errorf("%w: %d in progress", ErrBusy, n)
	}
	in.uploads[u] = struct{}{}
	in.reserved += size
	in.wg.Add(1)
	in.mu.Unlock()

	// The caller is the read loop, and the disk may be a mount that stopped
	// answering. One that answers late finds the upload abandoned and
	// clears up after it; until then it holds its slot.
	opened, abandoned := make(chan error), make(chan struct{})
	go func() {
		err := u.open()
		select {
		case opened <- err:
		case <-abandoned:
			if err == nil {
				u.discard()
			}
			in.forget(u)
		}
	}()
	var err error
	select {
	case err = <-opened:
	case <-time.After(in.timeout):
		close(abandoned)
		return nil, fmt.Errorf("%w: %s took over %v to start the upload", ErrSlowDisk, in.dir, in.timeout)
	}
	if err == nil {
		select {
		case <-u.cancel: // closed meanwhile
			u.discard()
			err = ErrClosed
		default:
		}
	}
	if err != nil {
		in.forget(u)
		return nil, err
	}
	go u.run()
	return u, nil
}

// open creates the directory when it is missing, checks that the upload
// fits, and creates its temporary file.
func (u *Upload) open() error {
	in := u.in
	if err := os.MkdirAll(in.dir, 0o700); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	dir, err := unix.Open(in.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("files: open %s: %w", in.dir, err)
	}
	var st unix.Statfs_t
	if err := in.statfs(dir, &st); err != nil {
		unix.Close(dir)
		return fmt.Errorf("files: free space in %s: %w", in.dir, err)
	}
	in.mu.Lock()
	others := in.reserved - u.size
	in.mu.Unlock()
	if free := int64(st.Bavail)*int64(st.Bsize) - others; u.size > free-in.margin {
		unix.Close(dir)
		return fmt.Errorf("%w in %s for %s: %s free, of which %s stays free",
			ErrNoSpace, in.dir, humanSize(u.size), humanSize(max(free, 0)), humanSize(in.margin))
	}
	f, err := in.create(dir, u.tmp)
	if err != nil {
		unix.Close(dir)
		return fmt.Errorf("files: create in %s: %w", in.dir, err)
	}
	u.dir, u.f = dir, f
	return nil
}

// discard undoes what open did, for an upload that never started.
func (u *Upload) discard() {
	u.f.Close()
	unix.Unlinkat(u.dir, u.tmp, 0)
	unix.Close(u.dir)
}

// Close cancels every upload in progress and waits, for a while, until
// their files are gone.
func (in *Inbox) Close() {
	in.mu.Lock()
	in.closed = true
	for u := range in.uploads {
		u.stop()
	}
	in.mu.Unlock()
	done := make(chan struct{})
	go func() { in.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeTimeout):
		in.log.Warn("uploads still writing at exit; their partial files may remain", "dir", in.dir)
	}
}

func (in *Inbox) forget(u *Upload) {
	in.mu.Lock()
	delete(in.uploads, u)
	in.reserved -= u.size
	in.mu.Unlock()
	in.wg.Done()
}

// Upload is one file on its way into an Inbox. Its methods are a bulk
// transfer's sink, and they run on the transfer's read loop, so none of
// them waits on the disk except Commit, and that only for a bounded time:
// a writer goroutine of its own does the writing.
type Upload struct {
	in   *Inbox
	name string
	size int64
	dir  int // the directory, open for the life of the upload
	tmp  string
	f    *os.File

	queue      chan []byte   // closed by Commit
	cancel     chan struct{} // closed by stop
	cancelOnce sync.Once
	failed     atomic.Pointer[error] // the writer's error, for Write to report

	done  chan struct{} // closed when the writer has finished
	err   error         // why it failed, once done
	saved string        // the name it was kept under, once done
}

// Write hands p to the writer. It never waits: when the queue is full the
// disk is not keeping up, and the upload fails rather than hold up the
// read loop, which carries the keyboard and pointer too. p is kept, as a
// data channel delivers each message in a fresh slice.
func (u *Upload) Write(p []byte) (int, error) {
	if err := u.failed.Load(); err != nil {
		return 0, *err
	}
	select {
	case u.queue <- p:
		return len(p), nil
	default:
		return 0, ErrSlowDisk
	}
}

// Commit waits for the writer to drain its queue and gives the file its
// name, or the name with the first free " (n)": it never replaces a file.
// It gives up, and the file goes, after the inbox's timeout.
func (u *Upload) Commit() error {
	close(u.queue)
	select {
	case <-u.done:
		return u.err
	case <-time.After(u.in.timeout):
		u.stop()
		return fmt.Errorf("%w: %s took over %v to finish", ErrSlowDisk, u.name, u.in.timeout)
	}
}

// Abort cancels the upload. The writer removes the partial file.
func (u *Upload) Abort(error) { u.stop() }

// Saved is the name the file was kept under, once Commit succeeded.
func (u *Upload) Saved() string {
	select {
	case <-u.done:
		return u.saved
	default:
		return ""
	}
}

func (u *Upload) stop() { u.cancelOnce.Do(func() { close(u.cancel) }) }

func (u *Upload) run() {
	err := u.drain()
	if err == nil {
		u.saved, err = u.keep()
	}
	if err != nil {
		u.f.Close()
		unix.Unlinkat(u.dir, u.tmp, 0)
	}
	unix.Close(u.dir)
	u.err = err
	close(u.done)
	u.in.forget(u)
}

// drain writes what Write queues until Commit closes the queue.
func (u *Upload) drain() error {
	for {
		select {
		case <-u.cancel:
			return errAborted
		case p, ok := <-u.queue:
			if !ok {
				return nil
			}
			if _, err := u.f.Write(p); err != nil {
				u.failed.Store(&err)
				return err
			}
		}
	}
}

// keep closes the file and names it.
func (u *Upload) keep() (string, error) {
	if err := u.f.Close(); err != nil {
		return "", err
	}
	select {
	case <-u.cancel:
		return "", errAborted
	default:
	}
	for n := 0; n <= maxNumbered; n++ {
		name := Numbered(u.name, n)
		err := u.renameNoReplace(name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("files: name %s: %w", name, err)
		}
	}
	return "", fmt.Errorf("files: %d files named like %s already", maxNumbered+1, u.name)
}

// createTemp opens a new file in dir, which must not exist yet and so
// cannot be a symlink to anywhere else.
func createTemp(dir int, name string) (*os.File, error) {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o666)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// renameNoReplace gives the temporary file the name to, failing with
// EEXIST rather than replacing anything there.
func (u *Upload) renameNoReplace(to string) error {
	err := u.in.rename(u.dir, u.tmp, u.dir, to, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) {
		return err
	}
	// Some filesystems cannot rename without replacing; a hard link never
	// replaces either.
	if err := unix.Linkat(u.dir, u.tmp, u.dir, to, 0); err != nil {
		return err
	}
	return unix.Unlinkat(u.dir, u.tmp, 0)
}

// humanSize formats n for people, in the binary units the page uses.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), -1
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[i])
}
