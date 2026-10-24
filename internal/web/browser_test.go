package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestBrowserSmoke loads the client in system Chrome through URL, answers its
// offer with a Pion peer standing in for the agent, streams it H.264 that
// Chrome encoded beforehand, and checks what crosses the websocket and the
// data channel. It needs `npm ci` in testdata/browser. Set
// ROBIN_SCREENSHOTS to keep the screenshots it takes.
func TestBrowserSmoke(t *testing.T) {
	dir := filepath.Join("testdata", "browser")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "playwright-core")); err != nil {
		t.Skip("browser smoke test needs `npm ci` in internal/web/testdata/browser")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("browser smoke test needs node on PATH")
	}
	shots := os.Getenv("ROBIN_SCREENSHOTS")
	if shots == "" {
		shots = t.TempDir()
	}

	frames := filepath.Join(t.TempDir(), "frames.json")
	s, _ := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

	var (
		mu       sync.Mutex
		offers   []string
		controls []protocol.Control
		agents   sync.WaitGroup
	)
	agents.Add(1)
	go func() {
		defer agents.Done()
		for {
			b, err := s.Accept(ctx)
			if err != nil {
				return
			}
			agents.Add(1)
			go func() {
				defer agents.Done()
				err := playAgent(ctx, b, frames,
					func(sdp string) { mu.Lock(); offers = append(offers, sdp); mu.Unlock() },
					func(c protocol.Control) { mu.Lock(); controls = append(controls, c); mu.Unlock() })
				if err != nil {
					t.Errorf("agent stand-in: %v", err)
				}
			}()
		}
	}()

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, node, "smoke.mjs", s.URL(), shots, frames)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	cancel()
	agents.Wait()
	t.Logf("smoke.mjs:\n%s", out.String())
	if err != nil {
		t.Fatalf("smoke.mjs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Two color schemes, one ICE restart, one reload.
	if len(offers) != 4 {
		t.Fatalf("got %d offers, want 4", len(offers))
	}
	for i, sdp := range offers {
		if err := checkOffer(sdp); err != nil {
			t.Errorf("offer %d: %v", i, err)
		}
	}
	if ufrag(offers[0]) == ufrag(offers[1]) {
		t.Errorf("restart offer kept ice-ufrag %q", ufrag(offers[0]))
	}
	checkControls(t, controls)
}

// checkOffer requires the first video payload type to be H.264
// packetization-mode=1 Constrained Baseline, and a data channel m-line.
func checkOffer(sdp string) error {
	m := regexp.MustCompile(`(?m)^m=video \S+ \S+ (\d+)`).FindStringSubmatch(sdp)
	if m == nil {
		return fmt.Errorf("no m=video line")
	}
	pt := m[1]
	if !regexp.MustCompile(`(?m)^a=rtpmap:` + pt + ` H264/90000`).MatchString(sdp) {
		return fmt.Errorf("first video payload type %s is not H264", pt)
	}
	fmtp := regexp.MustCompile(`(?m)^a=fmtp:` + pt + ` (.*)$`).FindStringSubmatch(sdp)
	if fmtp == nil || !strings.Contains(fmtp[1], "packetization-mode=1") || !strings.Contains(fmtp[1], "profile-level-id=42e01f") {
		return fmt.Errorf("first video payload type %s has fmtp %q", pt, fmtp)
	}
	if !regexp.MustCompile(`(?m)^m=application \S+ \S+ webrtc-datachannel`).MatchString(sdp) {
		return fmt.Errorf("no data channel m-line")
	}
	return nil
}

func ufrag(sdp string) string {
	m := regexp.MustCompile(`(?m)^a=ice-ufrag:(\S+)`).FindStringSubmatch(sdp)
	if m == nil {
		return ""
	}
	return m[1]
}

func checkControls(t *testing.T, got []protocol.Control) {
	t.Helper()
	has := func(want protocol.Control) bool {
		for _, c := range got {
			if c == want {
				return true
			}
		}
		return false
	}
	for _, want := range []protocol.Control{
		{T: protocol.ControlResize, W: 1280, H: 800},
		{T: protocol.ControlResize, W: 1000, H: 700},
		{T: protocol.ControlKey, Key: "a", Code: "KeyA", Down: true},
		{T: protocol.ControlKey, Key: "a", Code: "KeyA"},
		{T: protocol.ControlClipboard, Text: "from the local hook"},
		{T: protocol.ControlRelease},
		{T: protocol.ControlMove, X: 640, Y: 360},
		{T: protocol.ControlButton, X: 640, Y: 360, Button: 0, Down: true},
		{T: protocol.ControlButton, X: 640, Y: 360, Button: 0},
		{T: protocol.ControlButton, X: 1278, Y: 718, Button: 2, Down: true},
		{T: protocol.ControlButton, X: 1278, Y: 718, Button: 2},
		{T: protocol.ControlWheel, DY: 120},
	} {
		if !has(want) {
			t.Errorf("no %+v on the data channel", want)
		}
	}
	for _, c := range got {
		if c.T == protocol.ControlMove && c.Y == 0 {
			t.Errorf("pointer in the letterbox was sent: %+v", c)
		}
	}

	// A paste sends the clipboard before the V that triggers it.
	clip, v := -1, -1
	for i, c := range got {
		if c.T == protocol.ControlClipboard && c.Text == "pasted locally" && clip < 0 {
			clip = i
		}
		if c.T == protocol.ControlKey && c.Code == "KeyV" && c.Down && v < 0 {
			v = i
		}
	}
	if clip < 0 || v < 0 || clip > v {
		t.Errorf("paste: clipboard at %d, V down at %d; want clipboard first", clip, v)
	}
	// The hidden text box holds the focus, and what is pasted must not be
	// typed into it as well; nothing here is typed as text at all.
	for _, c := range got {
		if c.T == protocol.ControlText {
			t.Errorf("the page sent text %q", c.Text)
		}
	}
}

// playAgent answers the page's offers with a Pion peer, the way the agent
// would, streams the frames file and Opus silence once connected, and
// reports what the page sends. Clipboard text "echo" from the page is
// answered with remote clipboard text.
func playAgent(ctx context.Context, b *Browser, frames string, onOffer func(string), onControl func(protocol.Control)) error {
	defer b.Close()
	macros := &protocol.Macros{Host: "build-box", List: []protocol.Macro{{
		ID: "term", Name: "Terminal", Steps: []protocol.MacroStep{{Keys: []protocol.MacroKey{
			{Key: "Control", Code: "ControlLeft"}, {Key: "Alt", Code: "AltLeft"}, {Key: "t", Code: "KeyT"},
		}}},
	}, {
		// Long enough to overflow a phone, unless the page cuts it short.
		ID: "nginx", Name: "Restart the nginx service on the build box", Host: "build-box", Steps: []protocol.MacroStep{
			{Text: "sudo systemctl restart nginx.service && journalctl -u nginx.service -n 50 --no-pager\n"},
		},
	}}}
	if err := b.Send(protocol.Message{Type: protocol.TypeConfig, Config: &protocol.Config{ICEServers: []protocol.ICEServer{}, Macros: macros}}); err != nil {
		return err
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()

	// Candidates wait until the answer is out, as the agent's must.
	var (
		mu       sync.Mutex
		answered bool
		held     []*webrtc.ICECandidate
	)
	sendCandidate := func(c *webrtc.ICECandidate) {
		m := protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}}
		if c != nil {
			j := c.ToJSON()
			m.Candidate = &protocol.Candidate{Candidate: j.Candidate, SDPMid: j.SDPMid, SDPMLineIndex: j.SDPMLineIndex, UsernameFragment: j.UsernameFragment}
		}
		b.Send(m)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		mu.Lock()
		defer mu.Unlock()
		if !answered {
			held = append(held, c)
			return
		}
		sendCandidate(c)
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		// Heartbeat like the real agent, or the page treats the link as stalled.
		dc.OnOpen(func() {
			go func() {
				hb, _ := json.Marshal(protocol.Control{T: protocol.ControlHeartbeat})
				for dc.ReadyState() == webrtc.DataChannelStateOpen {
					dc.SendText(string(hb))
					time.Sleep(time.Second)
				}
			}()
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var c protocol.Control
			if json.Unmarshal(msg.Data, &c) != nil {
				return
			}
			onControl(c)
			if c.T == protocol.ControlClipboard && c.Text == "echo" {
				p, _ := json.Marshal(protocol.Control{T: protocol.ControlClipboard, Text: "from the remote"})
				dc.SendText(string(p))
			}
		})
	})

	for {
		m, err := b.Recv(ctx)
		if err != nil {
			return nil // the page went away
		}
		switch m.Type {
		case protocol.TypeOffer:
			onOffer(m.SDP)
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
				return err
			}
			if len(pc.GetSenders()) == 0 {
				track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
					MimeType:    webrtc.MimeTypeH264,
					ClockRate:   90000,
					SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				}, "video", "robin")
				if err != nil {
					return err
				}
				if _, err := pc.AddTrack(track); err != nil {
					return err
				}
				sound, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
					MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
				}, "audio", "robin")
				if err != nil {
					return err
				}
				if _, err := pc.AddTrack(sound); err != nil {
					return err
				}
				var once sync.Once
				pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
					if s == webrtc.PeerConnectionStateConnected {
						once.Do(func() {
							go stream(ctx, b.Done(), track, frames)
							go hum(ctx, b.Done(), sound)
						})
					}
				})
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return err
			}
			mu.Lock()
			answered = false
			mu.Unlock()
			if err := pc.SetLocalDescription(answer); err != nil {
				return err
			}
			if err := b.Send(protocol.Message{Type: protocol.TypeAnswer, SDP: answer.SDP}); err != nil {
				return err
			}
			mu.Lock()
			answered = true
			for _, c := range held {
				sendCandidate(c)
			}
			held = nil
			mu.Unlock()
		case protocol.TypeCandidate:
			if m.Candidate == nil || m.Candidate.Candidate == "" {
				continue
			}
			pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate:        m.Candidate.Candidate,
				SDPMid:           m.Candidate.SDPMid,
				SDPMLineIndex:    m.Candidate.SDPMLineIndex,
				UsernameFragment: m.Candidate.UsernameFragment,
			})
		}
	}
}

// hum sends what an idle desktop sounds like: Opus silence every 20 ms.
func hum(ctx context.Context, done <-chan struct{}, track *webrtc.TrackLocalStaticSample) {
	const frame = 20 * time.Millisecond
	tick := time.NewTicker(frame)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			track.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: frame})
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// stream loops the base64 Annex B access units in path at 30 fps.
func stream(ctx context.Context, done <-chan struct{}, track *webrtc.TrackLocalStaticSample, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var encoded []string
	if json.Unmarshal(raw, &encoded) != nil {
		return
	}
	frames := make([][]byte, len(encoded))
	for i, e := range encoded {
		frames[i], _ = base64.StdEncoding.DecodeString(e)
	}
	const interval = time.Second / 30
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-tick.C:
			track.WriteSample(media.Sample{Data: frames[i%len(frames)], Duration: interval})
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}
