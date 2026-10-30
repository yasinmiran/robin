package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestJoinCodeRoundTrips(t *testing.T) {
	c := JoinCode{Session: "a1b2c3d4", Owner: 1000, Secret: "k3j4m5n6p7q2r3s4t5u6v7w2x3"}
	got, err := ParseJoinCode(c.String())
	if err != nil || got != c {
		t.Fatalf("ParseJoinCode(%q) = %+v, %v", c.String(), got, err)
	}
	if c.String() != "a1b2c3d4-1000-k3j4m5n6p7q2r3s4t5u6v7w2x3" {
		t.Errorf("String() = %q", c.String())
	}
}

// Whatever ParseJoinCode returns names a socket and a uid, so it refuses
// anything a JoinCode would not have written.
func TestParseJoinCodeRefusesWhatItDidNotWrite(t *testing.T) {
	for _, s := range []string{
		"", "a1b2c3d4", "a1b2c3d4-1000", "a1b2c3d4-1000-abc-def",
		"-1000-abc", "a1b2c3d4-1000-", "A1B2-1000-abc", "a1/b2-1000-abc",
		"a1b2-x-abc", "a1b2--1-abc", "a1b2-01000-abc", "a1b2-+1-abc",
		"a1b2-1000-abc\n", "a1b2-1000-" + strings.Repeat("a", 200),
		"a1b2-99999999999999999999-abc",
	} {
		if c, err := ParseJoinCode(s); !errors.Is(err, ErrJoinCode) {
			t.Errorf("ParseJoinCode(%q) = %+v, %v; want ErrJoinCode", s, c, err)
		}
	}
}

// A target names a session and an owner robin-agent join may dial, before
// any secret is known.
func TestJoinCodeCheckTarget(t *testing.T) {
	if err := (JoinCode{Session: "a1b2c3d4", Owner: 0}).CheckTarget(); err != nil {
		t.Errorf("a good target: %v", err)
	}
	for _, c := range []JoinCode{
		{Owner: 1000}, {Session: "a1b2", Owner: -1}, {Session: "A1B2", Owner: 1000},
		{Session: "a1/b2", Owner: 1000}, {Session: "a1-b2", Owner: 1000},
		{Session: strings.Repeat("a", 129), Owner: 1000},
	} {
		if err := c.CheckTarget(); !errors.Is(err, ErrJoinCode) {
			t.Errorf("%+v: %v, want ErrJoinCode", c, err)
		}
	}
}

func TestShareSocketIsAbstract(t *testing.T) {
	if got := ShareSocket("a1b2c3d4"); got != "@robin-share-a1b2c3d4" {
		t.Errorf("ShareSocket = %q", got)
	}
}

// The sharing messages ride the existing envelopes, and an invitation
// without an expiry leaves the field out rather than send year one.
func TestShareMessagesEncode(t *testing.T) {
	b, err := json.Marshal(Message{Type: TypeInvited, Invitation: &Invitation{User: "alice", Error: "no such user"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"type":"invited","invitation":{"user":"alice","error":"no such user"}}` {
		t.Errorf("invited = %s", got)
	}
	exp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c := Control{T: ControlGuests, Guests: &Guests{
		List:        []Guest{{ID: "g1", User: "alice", Control: true}},
		Invitations: []Invitation{{User: "bob", Code: "x-1-y", Expires: exp}},
	}}
	b, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Control
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Guests.List[0] != c.Guests.List[0] || !back.Guests.Invitations[0].Expires.Equal(exp) {
		t.Errorf("guests did not round-trip: %s", b)
	}
}
