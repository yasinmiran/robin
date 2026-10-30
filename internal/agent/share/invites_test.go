//go:build linux

package share

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// clock is a time the test moves by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// testInvites is a table for session s1 of owner uid 4242 whose accounts
// are those of users, by name.
func testInvites(users map[string]int) (*Invites, *clock) {
	c := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	in := NewInvites("s1", 4242)
	in.now = c.now
	in.lookup = func(name string) (int, error) {
		if uid, ok := users[name]; ok {
			return uid, nil
		}
		return 0, fmt.Errorf("no user %s on this host", name)
	}
	return in, c
}

func TestAnInvitationAdmitsItsUserOnce(t *testing.T) {
	in, _ := testInvites(map[string]int{"alice": 1001})
	inv, err := in.Invite("alice")
	if err != nil {
		t.Fatal(err)
	}
	code, err := protocol.ParseJoinCode(inv.Code)
	if err != nil || code.Session != "s1" || code.Owner != 4242 || len(code.Secret) != 26 {
		t.Fatalf("code %q parses to %+v, %v", inv.Code, code, err)
	}
	if !in.Invited(1001) || in.Invited(1002) {
		t.Fatal("Invited does not follow the invitation")
	}
	user, err := in.Redeem(1001, inv.Code)
	if err != nil || user != "alice" {
		t.Fatalf("Redeem = %q, %v", user, err)
	}
	if _, err := in.Redeem(1001, inv.Code); !errors.Is(err, ErrBadCode) {
		t.Errorf("a second use: %v, want ErrBadCode", err)
	}
	if in.Invited(1001) || len(in.Open()) != 0 {
		t.Error("a spent invitation is still open")
	}
}

func TestAnInvitationExpires(t *testing.T) {
	in, c := testInvites(map[string]int{"alice": 1001})
	inv, err := in.Invite("alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := c.t.Add(protocol.InviteTTL); !inv.Expires.Equal(want) {
		t.Errorf("expires %v, want %v", inv.Expires, want)
	}
	c.t = c.t.Add(protocol.InviteTTL - time.Second)
	if !in.Invited(1001) {
		t.Fatal("expired a second early")
	}
	c.t = c.t.Add(time.Second)
	if in.Invited(1001) {
		t.Error("still open at its expiry")
	}
	if _, err := in.Redeem(1001, inv.Code); !errors.Is(err, ErrBadCode) {
		t.Errorf("redeemed after expiry: %v", err)
	}
}

// A code admits only the uid it was made for, in the session and of the
// owner it names.
func TestACodeAdmitsOnlyItsUID(t *testing.T) {
	in, _ := testInvites(map[string]int{"alice": 1001, "bob": 1002})
	alice, _ := in.Invite("alice")
	if _, err := in.Invite("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Redeem(1002, alice.Code); !errors.Is(err, ErrBadCode) {
		t.Fatalf("bob redeemed alice's code: %v", err)
	}
	if _, err := in.Redeem(1003, alice.Code); !errors.Is(err, ErrBadCode) || !strings.Contains(err.Error(), "no open invitation") {
		t.Fatalf("an uninvited uid: %v", err)
	}
	if user, err := in.Redeem(1001, alice.Code); err != nil || user != "alice" {
		t.Fatalf("alice's own code after bob tried it: %q, %v", user, err)
	}
}

func TestWrongCodesWithdrawTheInvitation(t *testing.T) {
	in, _ := testInvites(map[string]int{"alice": 1001})
	inv, _ := in.Invite("alice")
	c, _ := protocol.ParseJoinCode(inv.Code)
	wrong := []string{
		"not a code",
		protocol.JoinCode{Session: "s2", Owner: 4242, Secret: c.Secret}.String(),
		protocol.JoinCode{Session: "s1", Owner: 4243, Secret: c.Secret}.String(),
	}
	for i, code := range wrong {
		if _, err := in.Redeem(1001, code); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code %q: %v", code, err)
		}
		if open := in.Invited(1001); open != (i < maxTries-1) {
			t.Fatalf("after %d wrong codes the invitation is open: %v", i+1, open)
		}
	}
	if _, err := in.Redeem(1001, inv.Code); !errors.Is(err, ErrBadCode) {
		t.Errorf("the right code after the invitation was withdrawn: %v", err)
	}
}

func TestAWrongSecretIsWrong(t *testing.T) {
	in, _ := testInvites(map[string]int{"alice": 1001})
	inv, _ := in.Invite("alice")
	c, _ := protocol.ParseJoinCode(inv.Code)
	c.Secret = strings.Repeat("a", len(c.Secret))
	if _, err := in.Redeem(1001, c.String()); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a made-up secret: %v", err)
	}
	c.Secret = c.Secret[:10]
	if _, err := in.Redeem(1001, c.String()); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a short secret: %v", err)
	}
	if user, err := in.Redeem(1001, inv.Code); err != nil || user != "alice" {
		t.Fatalf("the right code after two wrong ones: %q, %v", user, err)
	}
}

func TestInviteAgainReplacesTheCode(t *testing.T) {
	in, _ := testInvites(map[string]int{"alice": 1001})
	first, _ := in.Invite("alice")
	second, _ := in.Invite("alice")
	if first.Code == second.Code {
		t.Fatal("the same code twice")
	}
	if open := in.Open(); len(open) != 1 || open[0].Code != second.Code {
		t.Fatalf("open = %+v", open)
	}
	if _, err := in.Redeem(1001, first.Code); !errors.Is(err, ErrBadCode) {
		t.Errorf("the replaced code: %v", err)
	}
	if _, err := in.Redeem(1001, second.Code); err != nil {
		t.Errorf("the new code: %v", err)
	}
}

func TestInviteRefuses(t *testing.T) {
	users := map[string]int{"owner": 4242}
	for i := range MaxInvites {
		users["u"+strconv.Itoa(i)] = 2000 + i
	}
	users["late"] = 3000
	in, c := testInvites(users)
	if _, err := in.Invite("nobody"); err == nil || !strings.Contains(err.Error(), "no user nobody") {
		t.Errorf("an unknown user: %v", err)
	}
	if _, err := in.Invite("owner"); err == nil || !strings.Contains(err.Error(), "own user") {
		t.Errorf("the owner: %v", err)
	}
	for i := range MaxInvites {
		if _, err := in.Invite("u" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := in.Invite("late"); err == nil || !strings.Contains(err.Error(), "open already") {
		t.Errorf("past MaxInvites: %v", err)
	}
	c.t = c.t.Add(protocol.InviteTTL)
	if _, err := in.Invite("late"); err != nil {
		t.Errorf("once the others expired: %v", err)
	}
	in.Clear()
	if len(in.Open()) != 0 || in.Invited(3000) {
		t.Error("Clear left invitations open")
	}
}

func TestLookupUser(t *testing.T) {
	for _, name := range []string{"", "-n", "a b", "a:b", "a/b", strings.Repeat("a", 33), "root\n"} {
		if _, err := LookupUser(name); err == nil || !strings.Contains(err.Error(), "not a user name") {
			t.Errorf("LookupUser(%q): %v", name, err)
		}
	}
	if uid, err := LookupUser("root"); err != nil || uid != 0 {
		t.Errorf("root = %d, %v", uid, err)
	}
	if _, err := LookupUser("robin-no-such-user"); err == nil || !strings.Contains(err.Error(), "no user robin-no-such-user") {
		t.Errorf("a missing user: %v", err)
	}
}

func TestUserName(t *testing.T) {
	if got := UserName(0); got != "root" {
		t.Errorf("UserName(0) = %q", got)
	}
	if got := UserName(2147483000); got != "uid 2147483000" {
		t.Errorf("an unknown uid = %q", got)
	}
	if got, want := UserName(os.Getuid()), os.Getenv("USER"); want != "" && got != want {
		t.Errorf("UserName(own uid) = %q, want %q", got, want)
	}
}

// getent answers for an account /etc/passwd lacks, as a directory service
// would; a stand-in getent on PATH plays that service.
func TestLookupFallsBackToGetent(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = passwd ] && [ \"$2\" = -- ] || exit 2\ncase $3 in\nldapuser|5005) echo 'ldapuser:x:5005:5005::/home/ldapuser:/bin/sh';;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(bin+"/getent", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if uid, err := LookupUser("ldapuser"); err != nil || uid != 5005 {
		t.Errorf("LookupUser(ldapuser) = %d, %v", uid, err)
	}
	if got := UserName(5005); got != "ldapuser" {
		t.Errorf("UserName(5005) = %q", got)
	}
}

// Invites made with NewInvites alone keep the host's clock and look up
// the host's accounts.
func TestInvitesUseTheHostByDefault(t *testing.T) {
	in := NewInvites("s1", 4242)
	before := time.Now()
	inv, err := in.Invite("root")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Expires.Before(before.Add(protocol.InviteTTL)) || inv.Expires.After(time.Now().Add(protocol.InviteTTL)) {
		t.Errorf("expires %v, want %v from now", inv.Expires, protocol.InviteTTL)
	}
	if !in.Invited(0) {
		t.Error("root's invitation is not open")
	}
}
