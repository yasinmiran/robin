package peer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestBulkRefusalReasonIsBounded has the browser refuse a transfer with a
// reason as long as a text frame can carry: what reaches the caller, and
// from there robin-send's terminal and the agent's log, is cut to the
// bound the agent keeps for its own reasons.
func TestBulkRefusalReasonIsBounded(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	bp.raw.Store(true)
	result := make(chan error, 1)
	go func() {
		result <- bp.agent.SendBulk(context.Background(), protocol.Bulk{Kind: testKind, ID: "long", Size: 10}, bytes.NewReader(make([]byte, 10)))
	}()
	var far *webrtc.DataChannel
	select {
	case far = <-bp.browserChannels:
	case <-time.After(10 * time.Second):
		t.Fatal("the channel never reached the browser")
	}
	waitOpen(t, far)
	sendJSON(t, far, protocol.Bulk{T: protocol.BulkError, Error: strings.Repeat("x", 60<<10)})
	select {
	case err := <-result:
		var r *BulkRefusedError
		if !errors.As(err, &r) {
			t.Fatalf("sender: %v, want a refusal", err)
		}
		if len(r.Reason) == 0 || len(r.Reason) > maxBulkReason {
			t.Errorf("the refusal's reason holds %d bytes, want 1 to %d", len(r.Reason), maxBulkReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sender never finished")
	}
}
