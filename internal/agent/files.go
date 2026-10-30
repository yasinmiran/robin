//go:build linux

package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/files"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

func init() {
	protocol.RegisterBulkKind(protocol.BulkFile, protocol.MaxFile)
	handleBulkFrom(protocol.BulkFile, (*session).acceptUpload)
	handleControlFrom(protocol.ControlFileFetch, (*session).fetchOffer)
	handleControl(protocol.ControlFileDecline, (*session).declineOffer)
}

// SendCommand is the name robin-agent send also answers to, linked into
// the PATH of a spawned session's programs.
const SendCommand = "robin-send"

// fileShare is a session's files: the inbox uploads land in, and the files
// programs on the host offer through its socket, each until the page saves
// or declines it or the program offering it goes.
type fileShare struct {
	inbox *files.Inbox
	ln    *files.Listener
	sock  string
	bin   string // holds the robin-send link
	log   *slog.Logger
	wg    sync.WaitGroup // transfers in progress

	mu     sync.Mutex
	offers map[string]*offer
	seq    int
	closed bool
}

// offer is one file on offer to the page.
type offer struct {
	info   protocol.File
	seq    int // orders the offers for a page that connects later
	file   *os.File
	from   *sendCall
	i      int                // the file's place in from's request
	cancel context.CancelFunc // of the transfer in progress; nil when none
}

// sendCall is one robin-agent send, which stays connected while any file it
// offered is still open.
type sendCall struct {
	req  *files.Request
	open int // offers not yet settled; fileShare.mu guards it
}

// openFiles gets a session's files ready before its desktop starts, so a
// spawned desktop's programs can find the offer socket and robin-send.
func openFiles(runDir string, log *slog.Logger) (*fileShare, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(runDir, files.SocketName)
	ln, err := files.Listen(sock, log)
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(runDir, "bin")
	exe, err := os.Executable()
	if err == nil {
		if err = os.Mkdir(bin, 0o700); err == nil {
			err = os.Symlink(exe, filepath.Join(bin, SendCommand))
		}
	}
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("link %s: %w", SendCommand, err)
	}
	return &fileShare{
		inbox: files.NewInbox(files.DownloadDir(home, os.Getenv), log),
		ln:    ln, sock: sock, bin: bin, log: log,
		offers: map[string]*offer{},
	}, nil
}

// env is what a spawned desktop's programs need to offer files: the socket
// and robin-send on their PATH.
func (fs *fileShare) env() []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return []string{files.SocketEnv + "=" + fs.sock, "PATH=" + fs.bin + ":" + path}
}

// close ends the share once serveFiles has returned, which withdrew every
// offer and so stopped every transfer: it waits for those, and removes
// every upload still under way. It closes the socket too, for a session
// that ended before it served.
func (fs *fileShare) close() {
	fs.ln.Close()
	fs.wg.Wait()
	fs.inbox.Close()
}

// take removes the offer id from the share and returns it, or nil.
func (fs *fileShare) take(id string) *offer {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	o := fs.offers[id]
	delete(fs.offers, id)
	return o
}

// settle tells the offer's sender what became of it, once it is out of the
// share, and lets the sender go once all its files are settled.
func (fs *fileShare) settle(o *offer, s files.Status, detail string) {
	if o.cancel != nil {
		o.cancel()
	}
	o.file.Close()
	o.from.req.Tell(o.i, s, detail)
	fs.mu.Lock()
	o.from.open--
	done := o.from.open == 0
	fs.mu.Unlock()
	if done {
		o.from.req.Close()
	}
}

// withdraw ends the offers that from made, or every offer when from is
// nil, and returns them.
func (fs *fileShare) withdraw(from *sendCall) []*offer {
	fs.mu.Lock()
	var gone []*offer
	for id, o := range fs.offers {
		if from == nil || o.from == from {
			delete(fs.offers, id)
			gone = append(gone, o)
		}
	}
	fs.mu.Unlock()
	for _, o := range gone {
		if o.cancel != nil {
			o.cancel()
		}
		o.file.Close()
	}
	return gone
}

// withdrawAll ends every offer and sender, for the end of the session.
func (fs *fileShare) withdrawAll() {
	fs.mu.Lock()
	fs.closed = true
	fs.mu.Unlock()
	senders := map[*sendCall]bool{}
	for _, o := range fs.withdraw(nil) {
		senders[o.from] = true
	}
	for s := range senders {
		s.req.Close()
	}
}

// serveFiles takes offers from robin-agent send until the session ends,
// then closes the socket and withdraws what is still on offer.
func (s *session) serveFiles(ctx context.Context) {
	if s.files == nil {
		return
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.files.ln.Serve(s.takeOffers)
	}()
	<-ctx.Done()
	s.files.ln.Close()
	// Requests wait on their senders; withdrawing lets them go.
	s.files.withdrawAll()
	<-served
}

// maxSessionOffers bounds the files on offer at once, from every
// robin-send together. Each keeps a descriptor open in the agent and a
// line in the page, and four full requests are more than anyone picks
// through; a program that offers and never hangs up holds no more than
// that.
const maxSessionOffers = 4 * files.MaxOffer

// takeOffers puts the files of one robin-agent send on offer and keeps
// them there until each is settled or the sender goes. A request that
// would bring more than maxSessionOffers on offer is refused whole.
func (s *session) takeOffers(r *files.Request) {
	defer r.Close()
	fs := s.files
	from := &sendCall{req: r}
	var mine []*offer
	for i, f := range r.Files {
		if f.Err != nil {
			r.Tell(i, files.Refused, f.Err.Error())
			continue
		}
		mine = append(mine, &offer{info: protocol.File{ID: rand.Text()[:16], Name: f.Name, Size: f.Size}, file: f.File, from: from, i: i})
	}
	fs.mu.Lock()
	closed := fs.closed
	offered := len(fs.offers)
	full := offered+len(mine) > maxSessionOffers
	if !closed && !full {
		for _, o := range mine {
			fs.seq++
			o.seq = fs.seq
			fs.offers[o.info.ID] = o
		}
		from.open = len(mine)
	}
	fs.mu.Unlock()
	if closed || full || len(mine) == 0 {
		for _, o := range mine {
			o.file.Close()
		}
		if full && !closed {
			s.log.Info("offer refused", "offered", offered, "files", len(mine), "limit", maxSessionOffers)
			r.Refuse(fmt.Sprintf("offering %d more would pass the %d files a session holds on offer", len(mine), maxSessionOffers))
		}
		return
	}
	for _, o := range mine {
		s.log.Info("file offered", "name", o.info.Name, "bytes", o.info.Size)
		r.Tell(o.i, files.Offered, "")
		s.showOffer(o.info)
	}
	<-r.Gone()
	for _, o := range fs.withdraw(from) {
		for _, v := range s.controlling() {
			s.sendFileControl(v, protocol.ControlFileWithdraw, protocol.File{ID: o.info.ID})
		}
	}
}

// showOffer shows every page with control a file on offer.
func (s *session) showOffer(f protocol.File) {
	for _, v := range s.controlling() {
		s.sendFileControl(v, protocol.ControlFileOffer, f)
	}
}

func (s *session) sendFileControl(v *viewer, t protocol.ControlType, f protocol.File) {
	if err := v.peer.SendControl(protocol.Control{T: t, File: &f}); err != nil && !errors.Is(err, peer.ErrNoControl) {
		s.log.Warn("send file message", "type", t, "err", err)
	}
}

// openOffers are the files on offer, in the order they came.
func (s *session) openOffers() []*offer {
	if s.files == nil {
		return nil
	}
	s.files.mu.Lock()
	open := slices.Collect(maps.Values(s.files.offers))
	s.files.mu.Unlock()
	slices.SortFunc(open, func(a, b *offer) int { return cmp.Compare(a.seq, b.seq) })
	return open
}

// resendOffers shows v's page, which has just connected or been given
// control, what is on offer.
func (s *session) resendOffers(v *viewer) {
	for _, o := range s.openOffers() {
		s.sendFileControl(v, protocol.ControlFileOffer, o.info)
	}
}

// withdrawOffers takes what is on offer off v's page, which lost control.
func (s *session) withdrawOffers(v *viewer) {
	for _, o := range s.openOffers() {
		s.sendFileControl(v, protocol.ControlFileWithdraw, protocol.File{ID: o.info.ID})
	}
}

// fetchOffer sends an offered file to the page of v that asked for it. A
// transfer that fails leaves the file on offer, for the
// user to try again.
func (s *session) fetchOffer(v *viewer, m protocol.Control) {
	fs, p := s.files, v.peer
	if fs == nil || m.File == nil {
		return
	}
	// Withdrawing the offer, as the end of the session does, cancels it,
	// and so does v's page losing control.
	ctx, cancel := s.transfer(context.Background(), v)
	fs.mu.Lock()
	o := fs.offers[m.File.ID]
	if o == nil || o.cancel != nil {
		fs.mu.Unlock()
		cancel()
		return
	}
	o.cancel = cancel
	fs.wg.Add(1)
	fs.mu.Unlock()
	go func() {
		defer fs.wg.Done()
		defer cancel()
		err := sendFile(ctx, p, o)
		if err != nil && ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		fs.mu.Lock()
		current := fs.offers[o.info.ID] == o
		if current {
			o.cancel = nil
			if err == nil {
				delete(fs.offers, o.info.ID)
			}
		}
		fs.mu.Unlock()
		switch {
		case !current:
			// Declined or withdrawn meanwhile, which settled it.
		case err == nil:
			s.log.Info("file saved by the browser", "name", o.info.Name)
			fs.settle(o, files.Saved, "")
		default:
			s.log.Info("file transfer failed", "name", o.info.Name, "err", err)
			o.from.req.Tell(o.i, files.Failed, failure(err))
		}
	}()
}

// failure is what robin-send prints for a transfer that failed: the
// browser's own reason when it refused one.
func failure(err error) string {
	var refused *peer.BulkRefusedError
	if errors.As(err, &refused) {
		return "the browser refused it: " + refused.Reason
	}
	return err.Error()
}

// sendFile sends what the file holds now, which may differ from its size
// when it was offered.
func sendFile(ctx context.Context, p *peer.Peer, o *offer) error {
	st, err := o.file.Stat()
	if err != nil {
		return err
	}
	return p.SendBulk(ctx, protocol.Bulk{Kind: protocol.BulkFile, ID: o.info.ID, Name: o.info.Name, Size: st.Size()},
		io.NewSectionReader(o.file, 0, st.Size()))
}

// declineOffer settles a file the user dismissed, stopping its transfer if
// one is under way.
func (s *session) declineOffer(m protocol.Control) {
	if s.files == nil || m.File == nil {
		return
	}
	if o := s.files.take(m.File.ID); o != nil {
		s.log.Info("file declined in the browser", "name", o.info.Name)
		s.files.settle(o, files.Declined, "")
	}
}

// acceptUpload takes a file the page sends into the download directory,
// unless the host allowed viewing only.
func (s *session) acceptUpload(from *viewer, h protocol.Bulk) (peer.BulkSink, error) {
	switch v, ok := s.desk.(desktop.ViewOnly); {
	case s.files == nil:
		return nil, errors.New("this session takes no files")
	case ok && v.ViewOnly():
		return nil, errors.New("the host allowed viewing only, so this session takes no files")
	}
	u, err := s.files.inbox.Accept(h.Name, h.Size)
	if err != nil {
		return nil, err
	}
	s.log.Info("upload started", "name", h.Name, "bytes", h.Size)
	return &upload{Upload: u, s: s, id: h.ID, v: from}, nil
}

// upload tells the page that sent it the name a file was kept under,
// which differs from the one it sent when that was taken.
type upload struct {
	*files.Upload
	s  *session
	id string
	v  *viewer // the page that sent it
}

func (u *upload) Commit() error {
	if err := u.Upload.Commit(); err != nil {
		return err
	}
	u.s.log.Info("upload kept", "name", u.Saved(), "dir", u.s.files.inbox.Dir())
	if err := u.v.peer.SendControl(protocol.Control{T: protocol.ControlFileSaved, File: &protocol.File{ID: u.id, Name: u.Saved()}}); err != nil && !errors.Is(err, peer.ErrNoControl) {
		u.s.log.Warn("send file message", "type", protocol.ControlFileSaved, "err", err)
	}
	return nil
}
