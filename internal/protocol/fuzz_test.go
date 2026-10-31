package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// signalingSeeds are lines the agent's stdio, the guests' door and the
// page's websocket carry, and some that none of them would.
var signalingSeeds = []string{
	`{"type":"start","start":{"version":2,"maxWidth":1920,"maxHeight":1080}}`,
	`{"type":"ready","ready":{"mode":"spawn"}}`,
	`{"type":"ping"}`,
	`{"type":"offer","sdp":"v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\n"}`,
	`{"type":"candidate","candidate":{"candidate":"candidate:1 1 udp 2130706431 10.0.0.2 50000 typ host","sdpMid":"0","sdpMLineIndex":0}}`,
	`{"type":"join","join":{"version":2,"code":"a1b2c3d4-1000-abcdefghijklmnopqrstuvwxyz"}}`,
	`{"type":"invited","invitation":{"user":"bob","code":"s-1-x","expires":"2026-10-30T12:10:00Z"}}`,
	`{"type":"macros","macroChange":{"put":{"id":"m1","name":"Hi","steps":[{"text":"hi"},{"pause":10},{"keys":[{"key":"Control","code":"ControlLeft"}]}]}}}`,
	`{"type":"error","error":"no display","code":"busy"}`,
	`{"type":"resume","resume":{"id":"abc"}}`,
	`{"type":"bye"}`,
	`{"sdp":"no type"}`,
	`{"type":""}`,
	`{"type":7}`,
	`[]`, `null`, `"text"`, `not json`, "\x1b]0;title\x07",
}

// remarshal is what a relay of m sends: m as Send writes it.
func remarshal(t *testing.T, m any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("a decoded %T does not marshal: %v", m, err)
	}
	return b
}

// FuzzDecodeLine feeds decodeLine what the agent's stdio and the guests'
// door read: whatever it accepts has a type, and is a fixed point of
// marshalling, so a relay passes on exactly what it was given.
func FuzzDecodeLine(f *testing.F) {
	for _, s := range signalingSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		m, err := decodeLine(line)
		if err != nil {
			if m.Type != "" {
				t.Fatalf("a refused line gave the message %+v", m)
			}
			return
		}
		if m.Type == "" {
			t.Fatal("accepted a line without a type")
		}
		b := remarshal(t, m)
		again, err := decodeLine(b)
		if err != nil {
			t.Fatalf("decodeLine refuses what it decoded, %s: %v", b, err)
		}
		if b2 := remarshal(t, again); !bytes.Equal(b, b2) {
			t.Fatalf("not a fixed point:\n%s\n%s", b, b2)
		}
	})
}

// FuzzConnStream feeds a whole stream to a Conn: it hands on no ping and
// nothing without a type, takes no more messages than the stream has
// lines, and always ends, with io.EOF or the framing error.
func FuzzConnStream(f *testing.F) {
	f.Add([]byte(strings.Join(signalingSeeds[:6], "\n") + "\n"))
	f.Add([]byte(`{"type":"ping"}` + "\r\n" + `{"type":"bye"}`))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(`{"type":"bye"}` + "\nnot json\n" + `{"type":"bye"}`))
	f.Fuzz(func(t *testing.T, stream []byte) {
		c := NewConn(bytes.NewReader(stream), io.Discard, 0, 0)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lines := bytes.Count(stream, []byte("\n")) + 1
		for n := 0; ; n++ {
			m, err := c.Recv(ctx)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("the connection never ended")
				}
				if n > lines {
					t.Fatalf("%d messages from %d lines", n, lines)
				}
				return
			}
			if m.Type == "" || m.Type == TypePing {
				t.Fatalf("Recv handed on %+v", m)
			}
		}
	})
}

// FuzzControl feeds the agent's decoding of a control channel message: a
// Control it decodes is a fixed point of marshalling, so what the agent
// acts on is what the page meant.
func FuzzControl(f *testing.F) {
	for _, s := range []string{
		`{"t":"key","down":true,"key":"a","code":"KeyA"}`,
		`{"t":"move","x":10,"y":-4}`,
		`{"t":"button","button":2,"down":true}`,
		`{"t":"wheel","dx":0.5,"dy":-120}`,
		`{"t":"resize","w":1920,"h":1080}`,
		`{"t":"clipboard","text":"hi"}`,
		`{"t":"monitor","monitor":"HDMI-1"}`,
		`{"t":"file-fetch","file":{"id":"1"}}`,
		`{"t":"curtain","on":true}`,
		`{"t":"tiles","on":true}`,
		`{"t":"invite","invite":{"user":"bob"}}`,
		`{"t":"guest","guest":{"id":"g1","action":"grant"}}`,
		`{"t":"cursor","cursor":{"id":1,"png":"iVBORw0KGgo=","hotX":1,"hotY":2}}`,
		`{"t":"frame","frame":{"rtp":1,"gen":2,"w":64,"h":64,"gone":[0,1]}}`,
		`{"t":"text","text":"‮evil"}`,
		`{"t":"key","x":1e400}`,
		`{}`, `[]`, `null`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var m Control
		if json.Unmarshal(data, &m) != nil {
			return
		}
		b := remarshal(t, m)
		var again Control
		if err := json.Unmarshal(b, &again); err != nil {
			t.Fatalf("Control refuses what it decoded, %s: %v", b, err)
		}
		if b2 := remarshal(t, again); !bytes.Equal(b, b2) {
			t.Fatalf("not a fixed point:\n%s\n%s", b, b2)
		}
	})
}

// FuzzParseBulk feeds a bulk channel's text frames: what ParseBulk takes
// is at most a chunk and round-trips, and a header CheckHeader passes is
// of a registered kind within its limit.
func FuzzParseBulk(f *testing.F) {
	for _, s := range []string{
		`{"t":"header","kind":"test","id":"7","name":"a.txt","mime":"text/plain","size":3}`,
		`{"t":"header","kind":"test"}`,
		`{"t":"header","kind":"test","size":-1}`,
		`{"t":"header","kind":"test","size":1048577}`,
		`{"t":"header","kind":"nobody","size":1}`,
		`{"t":"end","sha256":"ab"}`,
		`{"t":"done"}`,
		`{"t":"error","error":"no"}`,
		`{"t":"credit","credit":1048576}`,
		`{"t":"credit","credit":-1}`,
		`{"t":"header","size":1.5}`,
		`null`, `7`, `"x"`, `{"t":"header","kind":"test"}{"t":"end"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := ParseBulk(data)
		if err != nil {
			if !errors.Is(err, ErrBulkFrame) {
				t.Fatalf("ParseBulk failed with %v, not ErrBulkFrame", err)
			}
			if b != (Bulk{}) {
				t.Fatalf("a refused frame gave %+v", b)
			}
			return
		}
		if len(data) > BulkChunkSize {
			t.Fatalf("took a frame of %d bytes", len(data))
		}
		again, err := ParseBulk(remarshal(t, b))
		if err != nil || again != b {
			t.Fatalf("round trip of %+v gave %+v, %v", b, again, err)
		}
		if err := b.CheckHeader(); err == nil {
			limit, ok := BulkLimit(b.Kind)
			if b.T != BulkHeader || !ok || b.Size < 0 || b.Size > limit {
				t.Fatalf("CheckHeader passed %+v", b)
			}
		}
	})
}

// FuzzBulkTransfer plays a transfer at a receiver: a declared size, then
// chunks cut from the payload, then an end frame. The sum never counts
// past the declared size, and Check passes exactly when every declared
// byte arrived and the end names their SHA-256.
func FuzzBulkTransfer(f *testing.F) {
	f.Add(int64(5), []byte("hello"), []byte{2, 3}, true)
	f.Add(int64(0), []byte{}, []byte{}, true)
	f.Add(int64(4), []byte("hello"), []byte{5}, true)
	f.Add(int64(9), []byte("hello"), []byte{1, 1}, false)
	f.Add(int64(-1), []byte("x"), []byte{1}, true)
	f.Fuzz(func(t *testing.T, size int64, payload, cuts []byte, honest bool) {
		sum := NewBulkSum(size)
		var passed []byte
		rest := payload
		for _, c := range cuts {
			n := min(int(c), len(rest))
			if err := sum.Add(rest[:n]); err != nil {
				if !errors.Is(err, ErrBulkOverrun) {
					t.Fatalf("Add: %v", err)
				}
				if int64(len(passed)+n) <= size {
					t.Fatalf("refused %d bytes after %d of %d", n, len(passed), size)
				}
				continue
			}
			passed = append(passed, rest[:n]...)
			rest = rest[n:]
		}
		if size >= 0 && int64(len(passed)) > size {
			t.Fatalf("counted %d bytes of %d", len(passed), size)
		}
		h := sha256.Sum256(passed)
		end := Bulk{T: BulkEnd, SHA256: hex.EncodeToString(h[:])}
		if !honest {
			end.SHA256 = strings.Repeat("0", 64)
		}
		err := sum.Check(end)
		whole := int64(len(passed)) >= size
		if (err == nil) != (whole && honest) {
			t.Fatalf("Check with %d of %d bytes, honest %v: %v", len(passed), size, honest, err)
		}
	})
}

// FuzzBulkGrant takes what a sink took, in steps: every credit grants a
// window past what was taken, and credits only grow.
func FuzzBulkGrant(f *testing.F) {
	f.Add([]byte{1, 255, 128})
	f.Add(bytes.Repeat([]byte{255}, 64))
	f.Fuzz(func(t *testing.T, steps []byte) {
		var g BulkGrant
		var taken, last int64
		for _, s := range steps {
			n := int(s) << 12
			taken += int64(n)
			c, ok := g.Take(n)
			if !ok {
				continue
			}
			if c.T != BulkCredit || c.Credit != taken+BulkWindow || c.Credit <= last {
				t.Fatalf("credit %+v after %d taken, the last %d", c, taken, last)
			}
			last = c.Credit
		}
	})
}

// FuzzDecodeClip feeds a clipboard transfer's payload: what DecodeClip
// takes is within MaxClip, its text UTF-8, and it round-trips through
// EncodeClip.
func FuzzDecodeClip(f *testing.F) {
	for _, c := range []Clip{
		{Text: "hello"},
		{Text: "a", HTML: "<b>a</b>", PNG: []byte("\x89PNG\r\n\x1a\n")},
		{HTML: "<i>x</i>"},
		{PNG: []byte{1, 2, 3}},
	} {
		b, err := EncodeClip(c)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add([]byte{10, 't', 'e', 'x', 't', '/', 'p', 'l', 'a', 'i', 'n', 0, 0, 0, 1, 0xff})
	f.Add([]byte{3, 'x', '/', 'y', 0, 0, 0, 1, 'z'})
	f.Add([]byte{3, 'x', '/', 'y', 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := DecodeClip(data)
		if err != nil {
			if !errors.Is(err, ErrClip) {
				t.Fatalf("DecodeClip failed with %v, not ErrClip", err)
			}
			return
		}
		if c.Size() > MaxClip || c.Size() > len(data) {
			t.Fatalf("decoded %d bytes from %d", c.Size(), len(data))
		}
		b, err := EncodeClip(c)
		if err != nil {
			t.Fatalf("EncodeClip refuses what DecodeClip took: %v", err)
		}
		again, err := DecodeClip(b)
		if err != nil || again.Text != c.Text || again.HTML != c.HTML || !bytes.Equal(again.PNG, c.PNG) {
			t.Fatalf("round trip gave %+v, %v", again, err)
		}
	})
}

// FuzzParseTileBatch feeds a tile transfer's payload: what ParseTileBatch
// takes holds 1 to MaxTileBatch tiles and a PNG that declares their
// atlas, and Marshal gives back its bytes exactly.
func FuzzParseTileBatch(f *testing.F) {
	f.Add(TileBatch{RTP: 0xdeadbeef, Gen: 7, Tiles: []int{0, 31, 1 << 20}, PNG: atlasHead(192, 64)}.Marshal())
	f.Add(TileBatch{Tiles: make([]int, 17), PNG: atlasHead(1024, 128)}.Marshal())
	f.Add(TileBatch{Tiles: make([]int, MaxTileBatch), PNG: atlasHead(1024, 1024)}.Marshal())
	f.Add(TileBatch{Tiles: []int{0}, PNG: atlasHead(65535, 65535)}.Marshal())
	f.Add(TileBatch{}.Marshal())
	f.Add([]byte{0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, p []byte) {
		b, err := ParseTileBatch(p)
		if err != nil {
			if !errors.Is(err, ErrTileBatch) {
				t.Fatalf("ParseTileBatch failed with %v, not ErrTileBatch", err)
			}
			return
		}
		if n := len(b.Tiles); n == 0 || n > MaxTileBatch {
			t.Fatalf("took a batch of %d tiles", n)
		}
		if !bytes.HasPrefix(b.PNG, pngSignature) {
			t.Fatal("took a batch whose atlas is not a PNG")
		}
		if out := b.Marshal(); !bytes.Equal(out, p) {
			t.Fatalf("Marshal gave % x, from % x", out, p)
		}
	})
}

// FuzzParseJoinCode feeds what a guest passes to robin gui --join and
// presents at the door: a code ParseJoinCode takes names a socket and a
// uid, and is exactly what String writes of it.
func FuzzParseJoinCode(f *testing.F) {
	for _, s := range []string{
		"a1b2c3d4-1000-abcdefghijklmnopqrstuvwxyz",
		"s1-0-x", "s1--1-x", "s1-01-x", "s1-1000-", "-1000-x", "s1-1000-x-y",
		"S1-1000-x", "s1-+1-x", "s1-99999999999999999999-x", "s1-1000-x\n",
		strings.Repeat("a", 120) + "-1-b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseJoinCode(s)
		if err != nil {
			if !errors.Is(err, ErrJoinCode) || c != (JoinCode{}) {
				t.Fatalf("refused with %+v, %v", c, err)
			}
			return
		}
		if c.String() != s {
			t.Fatalf("%q parses to %+v, which writes %q", s, c, c.String())
		}
		if err := c.CheckTarget(); err != nil || !inAlphabet(c.Secret) {
			t.Fatalf("took %+v", c)
		}
		if strings.ContainsAny(ShareSocket(c.Session), "/\x00") {
			t.Fatalf("session %q names an odd socket", c.Session)
		}
	})
}
