package relay

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"strings"
)

// Credentials are the ICE username fragment and password of the agent's
// answer. A connectivity check the browser sends the agent names the ufrag
// in its USERNAME and is signed with the password, which reaches nobody but
// the page, over SSH and the token-guarded websocket.
type Credentials struct{ Ufrag, Pwd string }

// AnswerCredentials reads the agent's ICE credentials from an answer. Every
// a=ice-ufrag line must agree, and every a=ice-pwd line.
func AnswerCredentials(sdp string) (Credentials, error) {
	var c Credentials
	agree := func(field *string, v string) bool {
		if v == "" || (*field != "" && *field != v) {
			return false
		}
		*field = v
		return true
	}
	for _, l := range strings.Split(sdp, "\n") {
		l = strings.TrimRight(l, "\r")
		ok := true
		if v, found := strings.CutPrefix(l, "a=ice-ufrag:"); found {
			ok = agree(&c.Ufrag, v)
		} else if v, found := strings.CutPrefix(l, "a=ice-pwd:"); found {
			ok = agree(&c.Pwd, v)
		}
		if !ok {
			return Credentials{}, errors.New("relay: SDP carries empty or conflicting ICE credentials")
		}
	}
	if c.Ufrag == "" || c.Pwd == "" {
		return Credentials{}, errors.New("relay: SDP has no ICE credentials")
	}
	return c, nil
}

// STUN, as RFC 8489 frames it and RFC 8445 uses it for connectivity checks.
const (
	stunHeader        = 20
	stunMagicCookie   = 0x2112A442
	stunBindingReq    = 0x0001
	stunUsername      = 0x0006
	stunIntegrity     = 0x0008
	stunIntegritySize = sha1.Size
)

// Admits reports whether msg, one STUN message without its RFC 4571 length,
// is a connectivity check for the agent: a Binding request whose USERNAME
// starts with the agent's ufrag and a colon, and whose MESSAGE-INTEGRITY
// verifies with the agent's password. Zero Credentials admit nothing.
func (c Credentials) Admits(msg []byte) bool {
	if c.Ufrag == "" || c.Pwd == "" || len(msg) < stunHeader ||
		binary.BigEndian.Uint16(msg[0:2]) != stunBindingReq ||
		int(binary.BigEndian.Uint16(msg[2:4])) != len(msg)-stunHeader ||
		binary.BigEndian.Uint32(msg[4:8]) != stunMagicCookie {
		return false
	}
	var username []byte
	for at := stunHeader; at+4 <= len(msg); {
		typ := binary.BigEndian.Uint16(msg[at : at+2])
		size := int(binary.BigEndian.Uint16(msg[at+2 : at+4]))
		end := at + 4 + size
		if end > len(msg) {
			return false
		}
		switch typ {
		case stunUsername:
			username = msg[at+4 : end]
		case stunIntegrity:
			// The HMAC covers the message up to this attribute, with the
			// header's length counting up to this attribute's end.
			if size != stunIntegritySize || !strings.HasPrefix(string(username), c.Ufrag+":") {
				return false
			}
			signed := append([]byte(nil), msg[:at]...)
			binary.BigEndian.PutUint16(signed[2:4], uint16(end-stunHeader))
			mac := hmac.New(sha1.New, []byte(c.Pwd))
			mac.Write(signed)
			return hmac.Equal(mac.Sum(nil), msg[at+4:end])
		}
		at = end + (4-size%4)%4
	}
	return false
}
