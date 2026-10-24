package vqtest

import (
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLinkString(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Link
		want string
	}{
		{"named", Link{Name: "fast", Rate: 50_000_000, Queue: 50 * time.Millisecond, Delay: 15 * time.Millisecond, Jitter: time.Millisecond, Loss: 0.001},
			"fast: 50 Mbit/s 50ms queue, 30ms RTT, 1ms jitter, 0.1% loss"},
		{"unshaped", Link{Delay: 10 * time.Millisecond}, "unshaped, 20ms RTT, 0.0% loss"},
		{"shaped", Link{Rate: 4_000_000, Queue: 100 * time.Millisecond, Delay: 30 * time.Millisecond, Loss: 0.01},
			"4 Mbit/s 100ms queue, 60ms RTT, 1.0% loss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLinkCloseStopsItsQueue builds and closes links, shaped and not,
// and checks no bottleneck goroutine outlives its link.
func TestLinkCloseStopsItsQueue(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Link
	}{
		{"unshaped", Link{}},
		{"shaped", Link{Rate: 1_000_000, Queue: 50 * time.Millisecond, Delay: time.Millisecond, Loss: 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := queuesRunning()
			n, err := tc.l.Build()
			if err != nil {
				t.Fatal(err)
			}
			n.Close()
			if got := queuesRunning() - before; got != 0 {
				t.Fatalf("%d bottleneck goroutines left running", got)
			}
		})
	}
}

// queuesRunning counts the goroutines inside a vnet bottleneck's loop.
func queuesRunning() int {
	buf := make([]byte, 1<<20)
	for {
		if n := runtime.Stack(buf, true); n < len(buf) {
			return strings.Count(string(buf[:n]), "vnet.(*Queue).run(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestLinkDropsItsShare sends packets toward the browser over a link that
// loses half of them, and back the other way, which loses none.
func TestLinkDropsItsShare(t *testing.T) {
	n, err := Link{Loss: 0.5}.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	agent, err := n.Agent.ListenPacket("udp4", agentIP+":5000")
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	browser, err := n.Browser.ListenPacket("udp4", browserIP+":5000")
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	count := func(from, to net.PacketConn) int {
		for range 200 {
			if _, err := from.WriteTo([]byte{1}, to.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
		got := 0
		buf := make([]byte, 16)
		for {
			to.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, _, err := to.ReadFrom(buf); err != nil {
				return got
			}
			got++
		}
	}
	if got := count(agent, browser); got < 60 || got > 140 {
		t.Errorf("%d of 200 packets reached the browser over a link losing half", got)
	}
	if got := count(browser, agent); got != 200 {
		t.Errorf("%d of 200 packets reached the agent, which loses none", got)
	}
}
