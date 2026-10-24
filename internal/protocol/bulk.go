package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
)

// BulkChannel is the label of the data channels that carry bulk transfers:
// one channel per transfer, opened by whichever side sends.
//
// On it the sender writes a BulkHeader frame, the payload as binary chunks
// of at most BulkChunkSize bytes, and a BulkEnd frame with the payload's
// SHA-256. The receiver answers with one verdict frame, BulkDone or
// BulkError, and closes the channel. It may refuse at any point by sending
// BulkError early. Either side closing the channel before the verdict
// cancels the transfer. Every bulk channel is ordered and reliable, the
// default for a data channel: the frames mean nothing out of order.
//
// The receiver also paces the sender with BulkCredit frames. The sender
// may send BulkWindow bytes of payload before the first, and never more
// than the latest credit allows; the receiver grants more as its sink
// takes the bytes.
const BulkChannel = "bulk"

// BulkChunkSize bounds every frame on a bulk channel, text or binary. It
// keeps a chunk well under the 256 KiB message size browsers accept.
const BulkChunkSize = 64 << 10

// BulkWindow is how many bytes of payload a receiver holds room for beyond
// what its sink has taken. Every transfer shares one SCTP association with
// the control channel, and a sink that stalls holds back no more than
// this, where it would otherwise fill the association's receive buffer
// and stall the keyboard with it.
const BulkWindow = 512 << 10

// BulkType identifies a text frame on a bulk channel. Binary frames are
// always chunks.
type BulkType string

// Bulk frame types, in the order a transfer sends them.
const (
	// BulkHeader opens the transfer, from the sender.
	BulkHeader BulkType = "header"
	// BulkEnd follows the last chunk, from the sender.
	BulkEnd BulkType = "end"
	// BulkDone is the receiver's verdict when every byte arrived intact and
	// was kept.
	BulkDone BulkType = "done"
	// BulkError is the receiver's verdict when it refused or lost the
	// transfer, with the reason.
	BulkError BulkType = "error"
	// BulkCredit is the receiver letting the sender go on, any time
	// before its verdict.
	BulkCredit BulkType = "credit"
)

// Bulk is one text frame of a bulk transfer. Only the fields that belong to
// T are set.
type Bulk struct {
	T BulkType `json:"t"`

	// Header. Kind says what the payload is for and picks the receiver's
	// handler; ID, Name and MIME mean whatever that kind says. Size is the
	// payload's length in bytes.
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	MIME string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`

	// End: the payload's SHA-256 in lowercase hex.
	SHA256 string `json:"sha256,omitempty"`

	// Error: why the receiver refused or failed the transfer.
	Error string `json:"error,omitempty"`

	// Credit: how many bytes of payload the sender may have sent in all.
	Credit int64 `json:"credit,omitempty"`
}

// Reasons a bulk transfer fails. Each is wrapped with the detail.
var (
	ErrBulkFrame     = errors.New("protocol: malformed bulk frame")
	ErrBulkKind      = errors.New("protocol: unknown bulk kind")
	ErrBulkTooLarge  = errors.New("protocol: bulk transfer too large")
	ErrBulkOverrun   = errors.New("protocol: bulk data past the declared size")
	ErrBulkTruncated = errors.New("protocol: bulk transfer ended early")
	ErrBulkHash      = errors.New("protocol: bulk transfer SHA-256 mismatch")
)

// bulkLimits is written only from init, so it needs no lock.
var bulkLimits = map[string]int64{}

// RegisterBulkKind declares a kind of bulk transfer and the most bytes one
// may carry. Features call it from init in their own file, and only from
// init; a receiver refuses any kind nobody registered, and a sender cannot
// send one. It panics on an empty kind, a negative limit or a kind
// registered twice.
func RegisterBulkKind(kind string, limit int64) {
	if kind == "" || limit < 0 {
		panic(fmt.Sprintf("protocol: bad bulk kind %q with limit %d", kind, limit))
	}
	if _, dup := bulkLimits[kind]; dup {
		panic(fmt.Sprintf("protocol: bulk kind %q registered twice", kind))
	}
	bulkLimits[kind] = limit
}

// BulkLimit reports the most bytes a transfer of kind may carry, and
// whether anyone registered the kind.
func BulkLimit(kind string) (int64, bool) {
	limit, ok := bulkLimits[kind]
	return limit, ok
}

// ParseBulk decodes a text frame from a bulk channel.
func ParseBulk(data []byte) (Bulk, error) {
	if len(data) > BulkChunkSize {
		return Bulk{}, fmt.Errorf("%w: %d bytes", ErrBulkFrame, len(data))
	}
	var b Bulk
	if err := json.Unmarshal(data, &b); err != nil {
		return Bulk{}, fmt.Errorf("%w: %v", ErrBulkFrame, err)
	}
	return b, nil
}

// CheckHeader reports whether b may open a transfer: a header of a
// registered kind whose size is within the kind's limit.
func (b Bulk) CheckHeader() error {
	if b.T != BulkHeader {
		return fmt.Errorf("%w: %q where a header belongs", ErrBulkFrame, b.T)
	}
	if b.Size < 0 {
		return fmt.Errorf("%w: size %d", ErrBulkFrame, b.Size)
	}
	limit, ok := BulkLimit(b.Kind)
	if !ok {
		return fmt.Errorf("%w %q", ErrBulkKind, b.Kind)
	}
	if b.Size > limit {
		return fmt.Errorf("%w: %d bytes of %s, the limit is %d", ErrBulkTooLarge, b.Size, b.Kind, limit)
	}
	return nil
}

// BulkSum counts and hashes the chunks of one transfer as they pass, on
// either side of the channel. The zero value is the sum of a transfer that
// declared no bytes.
type BulkSum struct {
	size, n int64
	h       hash.Hash
}

// NewBulkSum starts the sum of a transfer that declared size bytes.
func NewBulkSum(size int64) *BulkSum {
	return &BulkSum{size: size}
}

func (s *BulkSum) hash() hash.Hash {
	if s.h == nil {
		s.h = sha256.New()
	}
	return s.h
}

// Add takes the next chunk. It refuses a chunk larger than BulkChunkSize and
// one that runs past the declared size.
func (s *BulkSum) Add(chunk []byte) error {
	if len(chunk) > BulkChunkSize {
		return fmt.Errorf("%w: chunk of %d bytes", ErrBulkFrame, len(chunk))
	}
	if int64(len(chunk)) > s.size-s.n {
		return fmt.Errorf("%w: %d bytes declared", ErrBulkOverrun, s.size)
	}
	s.n += int64(len(chunk))
	s.hash().Write(chunk)
	return nil
}

// End returns the frame that closes the transfer, once every declared byte
// has passed.
func (s *BulkSum) End() (Bulk, error) {
	if s.n < s.size {
		return Bulk{}, fmt.Errorf("%w: %d of %d bytes", ErrBulkTruncated, s.n, s.size)
	}
	return Bulk{T: BulkEnd, SHA256: hex.EncodeToString(s.hash().Sum(nil))}, nil
}

// Check compares the sender's end frame with the bytes that passed.
func (s *BulkSum) Check(end Bulk) error {
	if end.T != BulkEnd {
		return fmt.Errorf("%w: %q where the end belongs", ErrBulkFrame, end.T)
	}
	want, err := s.End()
	if err != nil {
		return err
	}
	if end.SHA256 != want.SHA256 {
		return ErrBulkHash
	}
	return nil
}

// BulkGrant is the receiving end's side of the credit: what its sink has
// taken, and how much of that it had taken at the last credit. The zero
// value is the start of a transfer, which the sender may send BulkWindow
// bytes of before any credit.
type BulkGrant struct {
	taken, last int64
}

// Take counts n bytes the sink took, and returns the credit frame to send
// once half a window has been taken since the last.
func (g *BulkGrant) Take(n int) (Bulk, bool) {
	g.taken += int64(n)
	if g.taken-g.last < BulkWindow/2 {
		return Bulk{}, false
	}
	g.last = g.taken
	return Bulk{T: BulkCredit, Credit: g.taken + BulkWindow}, true
}
