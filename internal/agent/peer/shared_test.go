package peer

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestPeersShareOneTrack binds one video and one audio track to two
// peers: a frame written once arrives at both browsers with the same
// timestamp, each under an SSRC of its own, and a peer that closes leaves
// the other receiving.
func TestPeersShareOneTrack(t *testing.T) {
	video, err := NewVideoTrack()
	if err != nil {
		t.Fatal(err)
	}
	sound, err := NewAudioTrack()
	if err != nil {
		t.Fatal(err)
	}
	type side struct {
		p     *Peer
		b     *browser
		cands chan protocol.Candidate
		up    chan struct{}
	}
	open := func() *side {
		s := &side{cands: make(chan protocol.Candidate, 32), up: make(chan struct{})}
		var once sync.Once
		s.p, err = New(Config{
			MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000, IncludeLoopback: true,
			Audio: true, Video: video, AudioTrack: sound,
		}, Handlers{
			Candidate: func(c protocol.Candidate) { s.cands <- c },
			State: func(st webrtc.PeerConnectionState) {
				if st == webrtc.PeerConnectionStateConnected {
					once.Do(func() { close(s.up) })
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.p.Close() })
		s.b = newBrowser(t)
		if _, err := s.b.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
		connect(t, s.b, s.p, s.cands)
		select {
		case <-s.up:
		case <-time.After(15 * time.Second):
			t.Fatal("peer never connected")
		}
		return s
	}
	a, b := open(), open()

	stop, wrote := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(wrote)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			au := []byte{0, 0, 0, 1, 0x65, 0, 0, 0x84, 0x21}
			binary.BigEndian.PutUint16(au[5:], uint16(i))
			video.Write(au, 33*time.Millisecond)
			sound.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
			select {
			case <-tick.C:
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-wrote
	}()

	// tracks waits for a browser's video and audio tracks.
	tracks := func(s *side) (video *webrtc.TrackRemote) {
		t.Helper()
		audio := false
		for video == nil || !audio {
			select {
			case r := <-s.b.track:
				if r.Kind() == webrtc.RTPCodecTypeVideo {
					video = r
				} else {
					audio = true
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("tracks: video %v, audio %v", video != nil, audio)
			}
		}
		return video
	}
	// frames reads video frames off a track: their numbers and timestamps,
	// and the SSRC they came under.
	frames := func(tr *webrtc.TrackRemote, n int) (map[uint16]uint32, uint32) {
		t.Helper()
		got := map[uint16]uint32{}
		deadline := time.Now().Add(15 * time.Second)
		for len(got) < n {
			tr.SetReadDeadline(deadline)
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				t.Fatalf("read rtp after %d frames: %v", len(got), err)
			}
			if len(pkt.Payload) >= 3 && pkt.Payload[0]&0x1f == 5 {
				got[binary.BigEndian.Uint16(pkt.Payload[1:])] = pkt.Timestamp
			}
		}
		return got, uint32(tr.SSRC())
	}
	trA, trB := tracks(a), tracks(b)
	gotA, ssrcA := frames(trA, 40)
	gotB, ssrcB := frames(trB, 40)
	if ssrcA == ssrcB {
		t.Errorf("both peers send under SSRC %d", ssrcA)
	}
	same := 0
	for i, ts := range gotA {
		if tb, ok := gotB[i]; ok {
			if tb != ts {
				t.Fatalf("frame %d carries %d to one browser and %d to the other", i, ts, tb)
			}
			same++
		}
	}
	if same == 0 {
		t.Fatal("no frame reached both browsers")
	}

	a.p.Close()
	if more, _ := frames(trB, 20); len(more) < 20 {
		t.Fatal("the second browser stopped receiving when the first peer closed")
	}
}
