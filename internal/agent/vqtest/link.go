package vqtest

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
)

// Link shapes the virtual path from the agent to the browser.
type Link struct {
	Name string
	// Rate in bits per second toward the browser; 0 leaves it unshaped.
	Rate int
	// Queue is how much the bottleneck buffers before it drops, as time
	// at Rate.
	Queue time.Duration
	// Delay is one way, so the round trip is twice it; Jitter adds up to
	// that much more to each packet, which can reorder them.
	Delay, Jitter time.Duration
	// Loss is the share of packets toward the browser dropped at random.
	Loss float64
}

// String describes the link's shape, after its name when it has one.
func (l Link) String() string {
	rate := "unshaped"
	if l.Rate > 0 {
		rate = fmt.Sprintf("%g Mbit/s %v queue", float64(l.Rate)/1e6, l.Queue)
	}
	shape := fmt.Sprintf("%s, %v RTT", rate, 2*l.Delay)
	if l.Jitter > 0 {
		shape += fmt.Sprintf(", %v jitter", l.Jitter)
	}
	shape += fmt.Sprintf(", %.1f%% loss", 100*l.Loss)
	if l.Name != "" {
		return l.Name + ": " + shape
	}
	return shape
}

// Net is a built link: the agent's and the browser's network stacks.
type Net struct {
	Agent, Browser *vnet.Net
	wan            *vnet.Router
	queue          *vnet.Queue // the bottleneck toward the browser, nil when unshaped
}

// Close stops the link.
func (n *Net) Close() {
	n.wan.Stop()
	if n.queue != nil {
		n.queue.Close()
	}
}

const (
	agentIP   = "10.0.0.1"
	browserIP = "10.0.0.2"
)

// Build starts the link.
func (l Link) Build() (_ *Net, err error) {
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR: "10.0.0.0/24", MinDelay: l.Delay, MaxJitter: l.Jitter, LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return nil, err
	}
	n := &Net{wan: wan}
	defer func() {
		if err != nil {
			n.Close()
		}
	}()
	if n.Agent, err = vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{agentIP}}); err != nil {
		return nil, err
	}
	if n.Browser, err = vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{browserIP}}); err != nil {
		return nil, err
	}
	var browser vnet.NIC = n.Browser
	if l.Rate > 0 {
		tbf := vnet.NewTBFQueue(l.Rate, 16_000, int64(float64(l.Rate)/8*l.Queue.Seconds()))
		if n.queue, err = vnet.NewQueue(n.Browser, tbf); err != nil {
			return nil, err
		}
		browser = n.queue
	}
	for _, nic := range []vnet.NIC{n.Agent, browser} {
		if err = wan.AddNet(nic); err != nil {
			return nil, err
		}
	}
	if l.Loss > 0 {
		var mu sync.Mutex
		r := rand.New(rand.NewPCG(1, 2))
		wan.AddChunkFilter(func(c vnet.Chunk) bool {
			if a, ok := c.DestinationAddr().(*net.UDPAddr); !ok || a.IP.String() != browserIP {
				return true
			}
			mu.Lock()
			defer mu.Unlock()
			return r.Float64() >= l.Loss
		})
	}
	if err = wan.Start(); err != nil {
		return nil, err
	}
	return n, nil
}
