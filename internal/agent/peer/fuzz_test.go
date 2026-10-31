package peer

import (
	"bytes"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// FuzzBulkSenderReceive feeds a sending end the frames a page's receiver
// may send, while the payload goes out: lines of a script, each a text
// frame ("s") or a binary one ("b"), then the bytes sent before the next.
// The credit only grows and never past a window beyond what was sent, and
// the transfer gets one verdict, done or error, whatever came.
func FuzzBulkSenderReceive(f *testing.F) {
	for _, s := range []string{
		"s{\"t\":\"credit\",\"credit\":786432}\x00s{\"t\":\"done\"}",
		"s{\"t\":\"credit\",\"credit\":99999999999}\x00s{\"t\":\"credit\",\"credit\":1}",
		"s{\"t\":\"error\",\"error\":\"disk full\"}\x00s{\"t\":\"done\"}",
		"b{\"t\":\"done\"}",
		"s{\"t\":\"header\",\"kind\":\"file\"}",
		"snot json\x00s{\"t\":\"done\"}",
		"s{\"t\":\"credit\",\"credit\":-5}",
	} {
		f.Add([]byte(s), []byte{1, 2, 3})
	}
	f.Fuzz(func(t *testing.T, script, sends []byte) {
		s := &bulkSender{verdict: make(chan protocol.Bulk, 1), more: make(chan struct{}, 1)}
		s.credit.Store(protocol.BulkWindow)
		verdicts := 0
		for i, frame := range bytes.Split(script, []byte{0}) {
			if len(frame) == 0 {
				continue
			}
			if i < len(sends) {
				s.sent.Add(int64(sends[i]) << 14)
			}
			before := s.credit.Load()
			s.receive(webrtc.DataChannelMessage{IsString: frame[0] == 's', Data: frame[1:]})
			if c := s.credit.Load(); c < before || c > max(before, s.sent.Load()+protocol.BulkWindow) {
				t.Fatalf("credit went from %d to %d with %d sent", before, c, s.sent.Load())
			}
			select {
			case v := <-s.verdict:
				verdicts++
				if v.T != protocol.BulkDone && v.T != protocol.BulkError {
					t.Fatalf("a verdict of %+v", v)
				}
				// The channel keeps the first; this one stands in for it.
				s.verdict <- v
			default:
			}
		}
		if verdicts > 0 && len(s.verdict) != 1 {
			t.Fatal("the verdict went missing")
		}
	})
}
