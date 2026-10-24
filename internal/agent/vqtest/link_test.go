package vqtest

import (
	"net"
	"testing"
	"time"
)

func TestLinkNames(t *testing.T) {
	for _, tc := range []struct {
		l    Link
		want string
	}{
		{Link{Name: "fast"}, "fast"},
		{Link{Delay: 10 * time.Millisecond}, "unshaped, 20ms RTT, 0.0% loss"},
		{Link{Rate: 4_000_000, Queue: 100 * time.Millisecond, Delay: 30 * time.Millisecond, Loss: 0.01},
			"4 Mbit/s 100ms queue, 60ms RTT, 1.0% loss"},
	} {
		if got := tc.l.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestLinkBuilds(t *testing.T) {
	for _, l := range []Link{{}, {Rate: 1_000_000, Queue: 50 * time.Millisecond, Delay: time.Millisecond, Loss: 0.5}} {
		n, err := l.Build()
		if err != nil {
			t.Fatal(err)
		}
		if (n.TBF != nil) != (l.Rate > 0) {
			t.Errorf("%v: TBF %v", l, n.TBF)
		}
		n.Close()
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
	count := func(from, to interface {
		WriteTo([]byte, net.Addr) (int, error)
		ReadFrom([]byte) (int, net.Addr, error)
		SetReadDeadline(time.Time) error
		LocalAddr() net.Addr
	}) int {
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
