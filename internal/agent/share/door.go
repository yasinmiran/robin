//go:build linux

package share

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agent/peercred"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Bounds on the guests' door: how long a connection has to present its
// code, how many may be at it at once, and how long an admitted guest
// waits for the session to take it.
const (
	joinWait    = 5 * time.Second
	maxJoining  = 4
	handoffWait = 2 * time.Second
)

// writeWait bounds one write to a guest: a guest that stops reading fails
// its connection instead of holding up whoever writes to it.
const writeWait = 10 * time.Second

// boundedWriter writes to c within wait each time.
type boundedWriter struct {
	c    net.Conn
	wait time.Duration
}

func (w boundedWriter) Write(p []byte) (int, error) {
	if err := w.c.SetWriteDeadline(time.Now().Add(w.wait)); err != nil {
		return 0, err
	}
	return w.c.Write(p)
}

// errNoGuests is what an admitted guest hears when the session takes no
// guests just then: its owner is away, or it holds as many as it takes.
var errNoGuests = errors.New("the session takes no guests now")

// maxRefusedUIDs bounds the uids whose first refusal the door logs at Warn.
const maxRefusedUIDs = 64

// refusals keeps the door's log of refusals bounded: anyone on the host may
// knock, as often as it likes, and every knock is refused before a byte is
// read. Each uid's first refusal goes at Warn, the rest at Debug.
type refusals struct {
	mu   sync.Mutex
	seen map[int]bool
}

func (r *refusals) level(uid int) slog.Level {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[uid] || len(r.seen) >= maxRefusedUIDs {
		return slog.LevelDebug
	}
	if r.seen == nil {
		r.seen = map[int]bool{}
	}
	r.seen[uid] = true
	return slog.LevelWarn
}

// Guest is a connection the door admitted: a guest's CLI, through its
// agent's proxy, run by the invited account.
type Guest struct {
	Conn *protocol.Conn
	UID  int
	// User is the name the owner invited.
	User string
	// Tunnel is the guest's Join.Tunnel.
	Tunnel bool
}

// Door takes guests on the session's abstract unix socket.
type Door struct {
	inv       *Invites
	ln        net.Listener
	log       *slog.Logger
	writeWait time.Duration
	joining   chan struct{} // a slot per connection presenting its code
	refused   refusals
	arrivals  chan *Guest
	// ctx ends when the door closes.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Open listens on protocol.ShareSocket of inv's session. Anyone on the
// host may connect to an abstract socket; the door hangs up on a
// connection whose uid holds no invitation before reading a byte of it.
func Open(inv *Invites, log *slog.Logger) (*Door, error) {
	return open(inv, log, writeWait)
}

// open is Open with each write to a guest bounded by wait.
func open(inv *Invites, log *slog.Logger, wait time.Duration) (*Door, error) {
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("unix", protocol.ShareSocket(inv.session))
	if err != nil {
		return nil, fmt.Errorf("share: listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Door{
		inv: inv, ln: ln, log: log, writeWait: wait,
		joining: make(chan struct{}, maxJoining), arrivals: make(chan *Guest),
		ctx: ctx, cancel: cancel,
	}
	d.wg.Go(d.accept)
	return d, nil
}

// Invites returns the door's invitations.
func (d *Door) Invites() *Invites { return d.inv }

func (d *Door) accept() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.admit(c)
	}
}

// admit checks a connection's uid on the accepting goroutine, which reads
// nothing, and lets it present its code on one of its own.
func (d *Door) admit(c net.Conn) {
	cred, err := peercred.Of(c)
	if err != nil {
		d.log.Log(context.Background(), d.refused.level(-1), "guest refused", "err", err)
		c.Close()
		return
	}
	uid := int(cred.Uid)
	if !d.inv.Invited(uid) {
		d.log.Log(context.Background(), d.refused.level(uid), "guest refused: no invitation", "uid", uid)
		c.Close()
		return
	}
	select {
	case d.joining <- struct{}{}:
	default:
		d.log.Log(context.Background(), d.refused.level(uid), "guest refused: too many joining at once", "uid", uid)
		c.Close()
		return
	}
	d.wg.Go(func() {
		defer func() { <-d.joining }()
		d.join(c, uid)
	})
}

// join reads the guest's Join and redeems its code, then waits for the
// session to take it.
func (d *Door) join(c net.Conn, uid int) {
	conn := protocol.NewConn(c, boundedWriter{c, d.writeWait}, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout)
	// protocol.Conn never closes its stream; the socket goes when it does.
	// This outlives the door for an admitted guest, whose connection is
	// the session's to close.
	go func() {
		<-conn.Done()
		c.Close()
	}()
	refuse := func(why error, told error) {
		d.log.Warn("guest refused", "uid", uid, "err", why)
		conn.Send(protocol.Message{Type: protocol.TypeError, Error: told.Error()})
		conn.Close()
	}
	ctx, cancel := context.WithTimeoutCause(d.ctx, joinWait, errors.New("no join message within "+joinWait.String()))
	defer cancel()
	m, err := conn.Recv(ctx)
	switch {
	case err != nil:
		refuse(err, err)
		return
	case m.Type != protocol.TypeJoin || m.Join == nil:
		err := fmt.Errorf("expected join, got %q", m.Type)
		refuse(err, err)
		return
	case m.Join.Version != protocol.Version:
		err := fmt.Errorf("the CLI speaks protocol %d, this agent %d", m.Join.Version, protocol.Version)
		refuse(err, err)
		return
	}
	user, err := d.inv.Redeem(uid, m.Join.Code)
	if err != nil {
		refuse(err, ErrBadCode)
		return
	}
	g := &Guest{Conn: conn, UID: uid, User: user, Tunnel: m.Join.Tunnel}
	select {
	case d.arrivals <- g:
		d.log.Info("guest joined", "user", user, "uid", uid)
	case <-time.After(handoffWait):
		refuse(errNoGuests, errNoGuests)
	case <-d.ctx.Done():
		refuse(errNoGuests, errNoGuests)
	}
}

// Next returns the next guest the door admitted.
func (d *Door) Next(ctx context.Context) (*Guest, error) {
	select {
	case g := <-d.arrivals:
		return g, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops taking guests and withdraws every invitation. A guest
// admitted already is the session's to close.
func (d *Door) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.inv.Clear()
		d.cancel()
		err = d.ln.Close()
		d.wg.Wait()
	})
	return err
}

// Dial connects a guest's agent to the door of the session code names, and
// makes sure the session's owner listens there: an abstract name is
// anyone's to take once the daemon that held it is gone.
func Dial(code protocol.JoinCode) (net.Conn, error) {
	c, err := net.Dial("unix", protocol.ShareSocket(code.Session))
	if err != nil {
		return nil, fmt.Errorf("no session %s to join on this host: %w", code.Session, err)
	}
	cred, err := peercred.Of(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	if int(cred.Uid) != code.Owner {
		c.Close()
		return nil, fmt.Errorf("session %s's socket belongs to uid %d, not to its owner, uid %d", code.Session, cred.Uid, code.Owner)
	}
	return c, nil
}

// Relay carries in to c and c to out, byte for byte, until either side
// ends or ctx does: a guest's agent between its SSH channel and the door.
// It closes c and returns once nothing more goes to out. The goroutine
// reading in runs until a read of in returns, since a read blocked on
// stdin cannot be interrupted; robin-agent join exits right after.
func Relay(ctx context.Context, c net.Conn, in io.Reader, out io.Writer) error {
	toDoor, fromDoor := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := io.Copy(c, in)
		toDoor <- err
	}()
	go func() {
		_, err := io.Copy(out, c)
		fromDoor <- err
	}()
	var first error
	select {
	case first = <-toDoor:
	case first = <-fromDoor:
		fromDoor = nil
	case <-ctx.Done():
	}
	c.Close()
	var second error
	if fromDoor != nil {
		second = <-fromDoor
	}
	return errors.Join(quiet(first), quiet(second))
}

// quiet drops what only says a side of the relay ended.
func quiet(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
