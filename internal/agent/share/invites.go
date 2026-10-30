//go:build linux

// Package share lets other accounts on the host join a session as guests.
// The owner invites a user by name; the session daemon then takes that
// user's connections on an abstract unix socket, admitting one only when
// the kernel says an invited uid made it and it presents that
// invitation's code: once, before the code expires.
package share

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// MaxInvites bounds the invitations open at once, and maxTries the wrong
// codes an invited uid may present before its invitation is withdrawn.
const (
	MaxInvites = 8
	maxTries   = 3
)

// ErrBadCode is what a guest hears of any code that does not admit it:
// unknown, spent, expired or someone else's are told apart only in the
// owner's log.
var ErrBadCode = errors.New("this join code is not valid: it may have expired or been used; ask the owner for another")

// Invites are a session's open invitations.
type Invites struct {
	session string
	owner   int
	lookup  func(name string) (int, error)
	now     func() time.Time

	mu   sync.Mutex
	open []*invite
}

type invite struct {
	user    string
	uid     int
	secret  string
	expires time.Time
	tries   int
}

// NewInvites holds the invitations of session, owned by uid owner.
func NewInvites(session string, owner int) *Invites {
	return &Invites{session: session, owner: owner, lookup: LookupUser, now: time.Now}
}

// Invite opens an invitation for the account named user, replacing any
// that account already had, and returns it with its code.
func (in *Invites) Invite(name string) (protocol.Invitation, error) {
	uid, err := in.lookup(name)
	if err != nil {
		return protocol.Invitation{}, err
	}
	if uid == in.owner {
		return protocol.Invitation{}, fmt.Errorf("%s is the session's own user, who resumes it with robin gui rather than joining", name)
	}
	secret, err := newSecret()
	if err != nil {
		return protocol.Invitation{}, err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.prune()
	in.open = slices.DeleteFunc(in.open, func(i *invite) bool { return i.uid == uid })
	if len(in.open) >= MaxInvites {
		return protocol.Invitation{}, fmt.Errorf("%d invitations are open already; let one be used or expire first", MaxInvites)
	}
	i := &invite{user: name, uid: uid, secret: secret, expires: in.now().Add(protocol.InviteTTL)}
	in.open = append(in.open, i)
	return in.view(i), nil
}

// Invited reports whether uid holds an open invitation: the check made of
// a connection before anything it sent is read.
func (in *Invites) Invited(uid int) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.prune()
	return slices.ContainsFunc(in.open, func(i *invite) bool { return i.uid == uid })
}

// Redeem spends the invitation of uid that code names and returns the
// user it was made for. Any failure is ErrBadCode, with why for the log;
// a uid that presents maxTries wrong codes loses its invitation.
func (in *Invites) Redeem(uid int, code string) (string, error) {
	c, parseErr := protocol.ParseJoinCode(code)
	in.mu.Lock()
	defer in.mu.Unlock()
	in.prune()
	n := slices.IndexFunc(in.open, func(i *invite) bool { return i.uid == uid })
	if n < 0 {
		return "", fmt.Errorf("%w (uid %d holds no open invitation)", ErrBadCode, uid)
	}
	i := in.open[n]
	// The secret is compared in constant time whatever else is wrong, so
	// how long a refusal takes says nothing of the secret.
	match := subtle.ConstantTimeCompare([]byte(c.Secret), []byte(i.secret)) == 1
	if parseErr == nil && match && c.Session == in.session && c.Owner == in.owner {
		in.open = slices.Delete(in.open, n, n+1)
		return i.user, nil
	}
	if i.tries++; i.tries >= maxTries {
		in.open = slices.Delete(in.open, n, n+1)
		return "", fmt.Errorf("%w (uid %d presented %d wrong codes; its invitation is withdrawn)", ErrBadCode, uid, maxTries)
	}
	return "", fmt.Errorf("%w (uid %d presented a wrong code)", ErrBadCode, uid)
}

// Open lists the invitations still open, oldest first, with their codes,
// for the owner's page.
func (in *Invites) Open() []protocol.Invitation {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.prune()
	list := make([]protocol.Invitation, 0, len(in.open))
	for _, i := range in.open {
		list = append(list, in.view(i))
	}
	return list
}

// Clear withdraws every invitation.
func (in *Invites) Clear() {
	in.mu.Lock()
	in.open = nil
	in.mu.Unlock()
}

// prune drops the invitations past their expiry. in.mu must be held.
func (in *Invites) prune() {
	now := in.now()
	in.open = slices.DeleteFunc(in.open, func(i *invite) bool { return !now.Before(i.expires) })
}

func (in *Invites) view(i *invite) protocol.Invitation {
	code := protocol.JoinCode{Session: in.session, Owner: in.owner, Secret: i.secret}
	return protocol.Invitation{User: i.user, Code: code.String(), Expires: i.expires}
}

// secretEncoding writes 16 random bytes as 26 characters of a-z and 2-7,
// which a JoinCode carries as they are.
var secretEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretEncoding.EncodeToString(b), nil
}

// userName is what Invite takes for an account: the portable set of
// useradd(8), which also keeps a name from reading as an option to getent.
var userName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)

// getentTimeout bounds the lookup through the host's name service.
const getentTimeout = 5 * time.Second

// LookupUser is the uid of the account name. The agent is a static binary
// that reads /etc/passwd itself, so an account it does not find there is
// asked of the host's getent, which knows its directory services too.
func LookupUser(name string) (int, error) {
	if !userName.MatchString(name) {
		return 0, fmt.Errorf("%q is not a user name", name)
	}
	if u, err := user.Lookup(name); err == nil {
		return strconv.Atoi(u.Uid)
	}
	if f, err := getent(name); err == nil && len(f) >= 3 && f[0] == name {
		return strconv.Atoi(f[2])
	}
	return 0, fmt.Errorf("no user %s on this host", name)
}

// UserName is the name of the account uid, or "uid <uid>" when the host
// has none.
func UserName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	if f, err := getent(strconv.Itoa(uid)); err == nil && len(f) >= 3 && f[2] == strconv.Itoa(uid) && userName.MatchString(f[0]) {
		return f[0]
	}
	return "uid " + strconv.Itoa(uid)
}

// getent is the passwd entry of key, split into its fields.
func getent(key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), getentTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getent", "passwd", "--", key).Output()
	if err != nil {
		return nil, err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.Split(line, ":"), nil
}
