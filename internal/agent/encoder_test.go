//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestSessionReportsBrokenEncoder checks that an encoder whose host tools
// fail is reported before the desktop starts, not at the first frame.
func TestSessionReportsBrokenEncoder(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cli := protocol.NewConn(outR, inW, protocol.DefaultPingInterval, 0)
	defer cli.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), inR, outW, Options{
			RunRoot: t.TempDir(),
			Encoders: map[string]NewEncoderFunc{
				protocol.EncoderVAAPI: func(int, int, int, int) (Encoder, error) {
					return nil, errors.New("no render node")
				},
			},
		})
		outW.Close()
	}()
	cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version, Encoder: protocol.EncoderVAAPI}})
	m, err := cli.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != protocol.TypeError || !strings.Contains(m.Error, "vaapi") || !strings.Contains(m.Error, "no render node") {
		t.Fatalf("got %+v", m)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("session ended cleanly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session still running")
	}
}
