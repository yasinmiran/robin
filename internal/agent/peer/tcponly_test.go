package peer

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
)

// TestTCPOnlyPairsThroughTheMuxWhateverTheBrowserNames: a browser that
// shows its host addresses (Chrome once the page has the microphone or the
// camera, or with its mDNS names off) names UDP candidates the agent can
// reach. Gathering UDP, the agent connects over them, and that pair
// outranks the tunnel's; TCP only, no UDP pair can form, and ICE takes the
// tunnel candidate, which leads to the mux's listener here. The browser
// hears the agent's candidates as the CLI relays them: all but the agent's
// TCP candidates, which give way to the tunnel's.
func TestTCPOnlyPairsThroughTheMuxWhateverTheBrowserNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tcpOnly bool
		want    webrtc.ICEProtocol
	}{
		{"udp too", false, webrtc.ICEProtocolUDP},
		{"tcp only", true, webrtc.ICEProtocolTCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: ln, ReadBufferSize: 64})
			t.Cleanup(func() { mux.Close() })

			cands := make(chan protocol.Candidate, 64)
			var mu sync.Mutex
			var sent []string
			connected := make(chan struct{})
			var once sync.Once
			p, err := New(Config{
				MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000,
				TCPMux: mux, TCPOnly: tc.tcpOnly, IncludeLoopback: true,
			}, Handlers{
				Candidate: func(c protocol.Candidate) {
					mu.Lock()
					sent = append(sent, c.Candidate)
					mu.Unlock()
					cands <- c
				},
				State: func(s webrtc.PeerConnectionState) {
					if s == webrtc.PeerConnectionStateConnected {
						once.Do(func() { close(connected) })
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })

			b := newExposedBrowser(t)
			gathered := webrtc.GatheringCompletePromise(b.pc)
			connect(t, b, p, asRelayed(cands, ln.Addr().(*net.TCPAddr).Port))
			select {
			case <-connected:
			case <-time.After(15 * time.Second):
				t.Fatal("peer never connected")
			}
			<-gathered
			if !namesReachableUDP(b.pc.LocalDescription().SDP) {
				t.Skip("this machine has no address beyond loopback and link-local for the browser to name")
			}
			pair, err := p.pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil || pair == nil {
				t.Fatalf("no selected pair: %v", err)
			}
			if pair.Local.Protocol != tc.want {
				t.Fatalf("the pair runs on %s, want %s: %s", pair.Local.Protocol, tc.want, pair)
			}
			if !tc.tcpOnly {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range sent {
				if f := strings.Fields(c); len(f) > 2 && !strings.EqualFold(f[2], "tcp") {
					t.Errorf("the agent gathered %q", c)
				}
			}
		})
	}
}

func TestTCPOnlyNeedsAMux(t *testing.T) {
	if p, err := New(Config{TCPOnly: true, MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000}, Handlers{}); err == nil {
		p.Close()
		t.Fatal("a TCP-only peer without a mux was made")
	}
}

// asRelayed passes on the agent's candidates as the CLI does under either
// transport that offers the tunnel: its TCP candidates dropped, and the
// tunnel's in their place, here at the mux's own port.
func asRelayed(agent <-chan protocol.Candidate, port int) <-chan protocol.Candidate {
	out := make(chan protocol.Candidate, cap(agent)+1)
	out <- protocol.Candidate{Candidate: relay.TunnelCandidate(port)}
	go func() {
		defer close(out)
		for c := range agent {
			if f := strings.Fields(c.Candidate); len(f) > 2 && strings.EqualFold(f[2], "tcp") {
				continue
			}
			out <- c
		}
	}()
	return out
}

// newExposedBrowser is newBrowser without mDNS names, gathering UDP on
// every address, loopback too, and dialling ICE-TCP, so it reaches a
// passive candidate on the host's loopback as the tunnel's other end does.
// Like a browser, it nominates once its checks have had a moment, not the
// first pair that answers.
func newExposedBrowser(t *testing.T) *browser {
	t.Helper()
	se := webrtc.SettingEngine{}
	se.SetHostAcceptanceMinWait(500 * time.Millisecond)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4})
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return newBrowser(t, webrtc.WithSettingEngine(se))
}

// namesReachableUDP reports whether sdp names a UDP host candidate the
// agent checks: one at an address that is not loopback or link-local.
func namesReachableUDP(sdp string) bool {
	for line := range strings.Lines(sdp) {
		c, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a=")
		if !ok || !strings.HasPrefix(c, "candidate:") {
			continue
		}
		if f := strings.Fields(c); len(f) > 7 && strings.EqualFold(f[2], "udp") && f[7] == "host" && !outOfReach(c) {
			return true
		}
	}
	return false
}
