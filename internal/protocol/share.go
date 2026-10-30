package protocol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sharing a session with another account on the host. The owner invites a
// user by name, from the page (ControlInvite) or from the CLI (TypeInvite);
// the agent answers with an Invitation whose Code that user's own robin gui
// presents in TypeJoin, over that user's own SSH login, to join as a guest.
// A guest watches the owner's stream and, once the owner grants it, has
// control: input, the clipboard, files and the microphone.
const (
	// TypeJoin is a guest CLI's first message, in place of Start.
	TypeJoin Type = "join"
	// TypeInvite goes from the owner's CLI to the agent, and TypeInvited
	// back with the Invitation.
	TypeInvite  Type = "invite"
	TypeInvited Type = "invited"
)

// Control types of a shared session. The owner's page sends ControlInvite
// and ControlGuest; the agent tells it ControlGuests whenever a guest or an
// invitation comes or goes, and tells each guest's page ControlShare
// whenever its control changes.
const (
	ControlInvite ControlType = "invite"
	ControlGuest  ControlType = "guest"
	ControlGuests ControlType = "guests"
	ControlShare  ControlType = "share"
)

// InviteTTL is how long an invitation can be redeemed. It is spent by the
// first join that presents it.
const InviteTTL = 10 * time.Minute

// Join is a guest CLI's handshake: the code of its invitation.
type Join struct {
	Version int    `json:"version"`
	Code    string `json:"code"`
}

// Invite names the account the owner shares the session with.
type Invite struct {
	User string `json:"user"`
}

// Invitation is the agent's answer to an Invite: the code to hand the user,
// and when it stops working, or Error, why there is none.
type Invitation struct {
	User    string    `json:"user"`
	Code    string    `json:"code,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
	Error   string    `json:"error,omitempty"`
}

// Share is what a guest's page shows: whose desktop it is, and whether the
// guest has control.
type Share struct {
	Owner   string `json:"owner"`
	Control bool   `json:"control,omitempty"`
}

// Guest is one guest as the owner's page lists it.
type Guest struct {
	ID      string `json:"id"`
	User    string `json:"user"`
	Control bool   `json:"control,omitempty"`
}

// Guests is the owner page's view of the sharing: who is connected, the
// invitations still open, and why the last change failed.
type Guests struct {
	List        []Guest      `json:"list"`
	Invitations []Invitation `json:"invitations"`
	Error       string       `json:"error,omitempty"`
}

// Actions of a GuestChange.
const (
	GuestGrant  = "grant"  // give the guest control, taking it from any other
	GuestRevoke = "revoke" // take control back
	GuestRemove = "remove" // close the guest's peer connection and let it go
)

// GuestChange is the owner page's change to one guest.
type GuestChange struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

// JoinCode is what an invitation's code names: the session, its owner's
// uid, which the guest checks the session's socket against, and the secret.
type JoinCode struct {
	Session string
	Owner   int
	Secret  string
}

// codeAlphabet is every character a session id or secret holds.
const codeAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// String is the code as the user passes it to robin gui --join.
func (c JoinCode) String() string {
	return c.Session + "-" + strconv.Itoa(c.Owner) + "-" + c.Secret
}

// ErrJoinCode is what ParseJoinCode returns for anything it did not make.
var ErrJoinCode = errors.New("not a join code: robin gui --join takes the code the owner was given, such as a1b2c3d4-1000-...")

// ParseJoinCode reads a code that JoinCode.String wrote. It refuses
// anything else, so what it returns can name a socket and a uid.
func ParseJoinCode(s string) (JoinCode, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 || len(s) > 128 {
		return JoinCode{}, ErrJoinCode
	}
	for _, p := range []string{parts[0], parts[2]} {
		if p == "" || strings.Trim(p, codeAlphabet) != "" {
			return JoinCode{}, ErrJoinCode
		}
	}
	uid, err := strconv.Atoi(parts[1])
	if err != nil || uid < 0 || strconv.Itoa(uid) != parts[1] {
		return JoinCode{}, ErrJoinCode
	}
	return JoinCode{Session: parts[0], Owner: uid, Secret: parts[2]}, nil
}

// ShareSocket is the abstract unix socket the session daemon of session
// id takes guests on. An abstract socket has no file and no permissions,
// so anyone may connect; the daemon admits only an invited uid that
// presents its code, and the guest checks the listener is the owner's.
func ShareSocket(id string) string {
	return fmt.Sprintf("@robin-share-%s", id)
}
