package relay

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/pion/stun/v4"
)

const (
	agentUfrag = "aGeNtUfRaGaGeNtU"
	agentPwd   = "aGeNtPaSsWoRdaGeNtPaSsWo"
)

// check is a connectivity check as the browser sends it to the agent:
// USERNAME is the agent's ufrag, a colon and the browser's, and
// MESSAGE-INTEGRITY is keyed with the agent's password.
func check(t *testing.T, username, pwd string, setters ...stun.Setter) []byte {
	t.Helper()
	tieBreaker := make([]byte, 8)
	priority := make([]byte, 4)
	binary.BigEndian.PutUint32(priority, 1845501695)
	all := append([]stun.Setter{stun.TransactionID, stun.BindingRequest, stun.NewUsername(username),
		stun.RawAttribute{Type: stun.AttrICEControlling, Value: tieBreaker},
		stun.RawAttribute{Type: stun.AttrPriority, Value: priority},
	}, setters...)
	if pwd != "" {
		all = append(all, stun.NewShortTermIntegrity(pwd))
	}
	all = append(all, stun.Fingerprint)
	m, err := stun.Build(all...)
	if err != nil {
		t.Fatal(err)
	}
	return m.Raw
}

func TestAnswerCredentials(t *testing.T) {
	sdp := strings.Replace(answer(fpA), "a=mid:0\r\n", "a=mid:0\r\na=ice-ufrag:"+agentUfrag+"\r\na=ice-pwd:"+agentPwd+"\r\n", 1)
	sdp = strings.Replace(sdp, "a=mid:1\r\n", "a=mid:1\r\na=ice-ufrag:"+agentUfrag+"\r\na=ice-pwd:"+agentPwd+"\r\n", 1)
	c, err := AnswerCredentials(sdp)
	if err != nil || c.Ufrag != agentUfrag || c.Pwd != agentPwd {
		t.Fatalf("AnswerCredentials = %+v, %v", c, err)
	}
	for name, bad := range map[string]string{
		"none":        answer(fpA),
		"no password": strings.Replace(answer(fpA), "a=mid:0\r\n", "a=mid:0\r\na=ice-ufrag:"+agentUfrag+"\r\n", 1),
		"two ufrags":  sdp + "a=ice-ufrag:other\r\n",
		"two pwds":    sdp + "a=ice-pwd:otherpasswordotherpassword\r\n",
		"empty":       strings.Replace(sdp, "a=ice-ufrag:"+agentUfrag, "a=ice-ufrag:", 2),
	} {
		if c, err := AnswerCredentials(bad); err == nil {
			t.Errorf("%s: AnswerCredentials = %+v, want an error", name, c)
		}
	}
}

// TestCredentialsAdmitOnlyTheAgentsChecks: the tunnel takes a connection
// only when its first message is a binding request that proves it knows the
// agent's password, which only the page has been told.
func TestCredentialsAdmitOnlyTheAgentsChecks(t *testing.T) {
	c := Credentials{Ufrag: agentUfrag, Pwd: agentPwd}
	good := check(t, agentUfrag+":brws", agentPwd)
	if !c.Admits(good) {
		t.Fatal("a check with the agent's credentials was refused")
	}
	if !c.Admits(check(t, agentUfrag+":brws", agentPwd, stun.RawAttribute{Type: stun.AttrUseCandidate})) {
		t.Fatal("a nominating check was refused")
	}

	response, err := stun.Build(stun.TransactionID, stun.BindingSuccess, stun.NewUsername(agentUfrag+":brws"),
		stun.NewShortTermIntegrity(agentPwd), stun.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	// The agent's ufrag only in a USERNAME after the integrity, which nothing
	// signed: the signed one names a stranger.
	stranger := check(t, "strangerufrag:brws", agentPwd)
	afterIntegrity := append([]byte(nil), stranger[:len(stranger)-8]...) // drop FINGERPRINT
	late := agentUfrag + ":brws"
	afterIntegrity = binary.BigEndian.AppendUint16(afterIntegrity, 0x0006)
	afterIntegrity = binary.BigEndian.AppendUint16(afterIntegrity, uint16(len(late)))
	afterIntegrity = append(afterIntegrity, late...)
	afterIntegrity = append(afterIntegrity, make([]byte, (4-len(late)%4)%4)...)
	binary.BigEndian.PutUint16(afterIntegrity[2:4], uint16(len(afterIntegrity)-20))
	for name, msg := range map[string][]byte{
		"another ufrag":        check(t, "strangerufrag:brws", agentPwd),
		"the ufrag as a peer":  check(t, "brws:"+agentUfrag, agentPwd),
		"the ufrag, no colon":  check(t, agentUfrag, agentPwd),
		"a guessed password":   check(t, agentUfrag+":brws", "guessedpasswordguessedpa"),
		"no integrity":         check(t, agentUfrag+":brws", ""),
		"a response":           response.Raw,
		"a short header":       good[:19],
		"a cut attribute":      good[:len(good)-3],
		"a wrong length":       withLength(good, len(good)-20+4),
		"no magic cookie":      withByte(good, 4, 0),
		"a flipped bit":        withByte(good, 30, good[30]^1),
		"username after":       afterIntegrity,
		"nothing":              nil,
		"rtp":                  []byte{0x80, 0x66, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 2, 3, 4, 5, 6, 7, 8},
		"a bare header":        good[:20],
		"an odd length header": withLength(good[:20], 2),
	} {
		if c.Admits(msg) {
			t.Errorf("%s: admitted", name)
		}
	}
	if (Credentials{}).Admits(good) {
		t.Error("no credentials yet, and a check was admitted")
	}
	if (Credentials{Ufrag: agentUfrag}).Admits(check(t, agentUfrag+":brws", "")) {
		t.Error("credentials without a password admitted a check without integrity")
	}
}

func withLength(msg []byte, n int) []byte {
	out := append([]byte(nil), msg...)
	binary.BigEndian.PutUint16(out[2:4], uint16(n))
	return out
}

func withByte(msg []byte, i int, b byte) []byte {
	out := append([]byte(nil), msg...)
	out[i] = b
	return out
}
