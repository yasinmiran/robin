package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// testKind is the one bulk kind these tests may send.
const (
	testKind  = "test"
	testLimit = 1 << 20
)

func init() { RegisterBulkKind(testKind, testLimit) }

func TestBulkWireShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame Bulk
		want  string
	}{
		{"header", Bulk{T: BulkHeader, Kind: "file", ID: "7", Name: "a.txt", MIME: "text/plain", Size: 3},
			`{"t":"header","kind":"file","id":"7","name":"a.txt","mime":"text/plain","size":3}`},
		{"empty header", Bulk{T: BulkHeader, Kind: "file"}, `{"t":"header","kind":"file"}`},
		{"end", Bulk{T: BulkEnd, SHA256: "ab"}, `{"t":"end","sha256":"ab"}`},
		{"done", Bulk{T: BulkDone}, `{"t":"done"}`},
		{"error", Bulk{T: BulkError, Error: "no"}, `{"t":"error","error":"no"}`},
		{"credit", Bulk{T: BulkCredit, Credit: 1 << 20}, `{"t":"credit","credit":1048576}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("%+v marshals as %s, want %s", tc.frame, b, tc.want)
			}
			back, err := ParseBulk(b)
			if err != nil || back != tc.frame {
				t.Errorf("ParseBulk(%s) = %+v, %v", b, back, err)
			}
		})
	}
}

func TestParseBulkRefusesJunk(t *testing.T) {
	for name, data := range map[string][]byte{
		"not json":   []byte("header"),
		"wrong type": []byte(`{"t":"header","size":"big"}`),
		"oversized":  []byte(`{"t":"header","name":"` + strings.Repeat("x", BulkChunkSize) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBulk(data); !errors.Is(err, ErrBulkFrame) {
				t.Errorf("%v, want ErrBulkFrame", err)
			}
		})
	}
}

func TestBulkRegistry(t *testing.T) {
	if limit, ok := BulkLimit(testKind); !ok || limit != testLimit {
		t.Fatalf("BulkLimit(%q) = %d, %v", testKind, limit, ok)
	}
	if _, ok := BulkLimit("never registered"); ok {
		t.Fatal("an unregistered kind has a limit")
	}
	for name, register := range map[string]func(){
		"twice":          func() { RegisterBulkKind(testKind, 1) },
		"empty kind":     func() { RegisterBulkKind("", 1) },
		"negative limit": func() { RegisterBulkKind("negative", -1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			register()
		}()
	}
	if _, ok := BulkLimit("negative"); ok {
		t.Error("a refused registration was kept")
	}
}

func TestCheckHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Bulk
		want error
	}{
		{"empty payload", Bulk{T: BulkHeader, Kind: testKind}, nil},
		{"at the limit", Bulk{T: BulkHeader, Kind: testKind, Size: testLimit}, nil},
		{"past the limit", Bulk{T: BulkHeader, Kind: testKind, Size: testLimit + 1}, ErrBulkTooLarge},
		{"negative size", Bulk{T: BulkHeader, Kind: testKind, Size: -1}, ErrBulkFrame},
		{"unknown kind", Bulk{T: BulkHeader, Kind: "nobody", Size: 1}, ErrBulkKind},
		{"not a header", Bulk{T: BulkEnd, Kind: testKind}, ErrBulkFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.h.CheckHeader(); !errors.Is(err, tc.want) {
				t.Errorf("%v, want %v", err, tc.want)
			}
		})
	}
}

func TestBulkSumMatchesSHA256(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{0, 1, 63, 64, 65, BulkChunkSize, BulkChunkSize + 1, 3*BulkChunkSize + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(rng.Uint32())
			}
			sender, receiver := NewBulkSum(int64(size)), NewBulkSum(int64(size))
			for rest := payload; len(rest) > 0; {
				n := min(len(rest), 1+rng.IntN(BulkChunkSize))
				if err := sender.Add(rest[:n]); err != nil {
					t.Fatalf("sender: %v", err)
				}
				if err := receiver.Add(rest[:n]); err != nil {
					t.Fatalf("receiver: %v", err)
				}
				rest = rest[n:]
			}
			end, err := sender.End()
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(payload)
			if end.T != BulkEnd || end.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("end %+v, want sha256 %x", end, sum)
			}
			if err := receiver.Check(end); err != nil {
				t.Fatalf("receiver refuses a good end: %v", err)
			}
		})
	}
}

func TestBulkSumZeroValue(t *testing.T) {
	var s BulkSum
	if err := s.Add([]byte{1}); !errors.Is(err, ErrBulkOverrun) {
		t.Errorf("a byte into the zero sum: %v, want ErrBulkOverrun", err)
	}
	if err := s.Add(nil); err != nil {
		t.Errorf("an empty chunk into the zero sum: %v", err)
	}
	empty := sha256.Sum256(nil)
	end, err := s.End()
	if err != nil || end != (Bulk{T: BulkEnd, SHA256: hex.EncodeToString(empty[:])}) {
		t.Fatalf("zero sum ends with %+v, %v, want the SHA-256 of nothing", end, err)
	}
	var fresh BulkSum
	if err := fresh.Check(end); err != nil {
		t.Errorf("a fresh zero sum refuses the end of nothing: %v", err)
	}
}

func TestBulkSumRefuses(t *testing.T) {
	good := bytes.Repeat([]byte{7}, 100)
	sum := sha256.Sum256(good)
	goodEnd := Bulk{T: BulkEnd, SHA256: hex.EncodeToString(sum[:])}

	s := NewBulkSum(100)
	if err := s.Add(make([]byte, BulkChunkSize+1)); !errors.Is(err, ErrBulkFrame) {
		t.Errorf("oversized chunk: %v, want ErrBulkFrame", err)
	}
	if err := s.Add(make([]byte, 101)); !errors.Is(err, ErrBulkOverrun) {
		t.Errorf("chunk past the size: %v, want ErrBulkOverrun", err)
	}
	if err := s.Add(good[:60]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.End(); !errors.Is(err, ErrBulkTruncated) {
		t.Errorf("End after 60 of 100 bytes: %v, want ErrBulkTruncated", err)
	}
	if err := s.Check(goodEnd); !errors.Is(err, ErrBulkTruncated) {
		t.Errorf("Check after 60 of 100 bytes: %v, want ErrBulkTruncated", err)
	}
	if err := s.Add(good[60:]); err != nil {
		t.Fatal(err)
	}
	if err := s.Add([]byte{0}); !errors.Is(err, ErrBulkOverrun) {
		t.Errorf("one byte past the size: %v, want ErrBulkOverrun", err)
	}
	if err := s.Check(Bulk{T: BulkEnd, SHA256: strings.Repeat("0", 64)}); !errors.Is(err, ErrBulkHash) {
		t.Errorf("wrong hash: %v, want ErrBulkHash", err)
	}
	if err := s.Check(Bulk{T: BulkEnd, SHA256: strings.ToUpper(goodEnd.SHA256)}); !errors.Is(err, ErrBulkHash) {
		t.Errorf("uppercase hash: %v, want ErrBulkHash", err)
	}
	if err := s.Check(Bulk{T: BulkDone}); !errors.Is(err, ErrBulkFrame) {
		t.Errorf("verdict where the end belongs: %v, want ErrBulkFrame", err)
	}
	if err := s.Check(goodEnd); err != nil {
		t.Errorf("good end: %v", err)
	}
}

// TestBulkGrant: a window before any credit, and a window past what the
// sink took once it has taken half of one.
func TestBulkGrant(t *testing.T) {
	g := NewBulkGrant()
	if _, ok := g.Take(BulkWindow/2 - 1); ok {
		t.Fatal("credit before half a window was taken")
	}
	if c, ok := g.Take(1); !ok || c != (Bulk{T: BulkCredit, Credit: BulkWindow/2 + BulkWindow}) {
		t.Fatalf("credit %+v, %v", c, ok)
	}
	if _, ok := g.Take(BulkWindow/2 - 1); ok {
		t.Fatal("a second credit too soon")
	}
	if c, ok := g.Take(1); !ok || c.Credit != 2*BulkWindow {
		t.Fatalf("second credit %+v, %v", c, ok)
	}
}
