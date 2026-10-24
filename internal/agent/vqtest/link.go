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

func (l Link) String() string {
	if l.Name != "" {
		return l.Name
	}
	rate := "unshaped"
	if l.Rate > 0 {
		rate = fmt.Sprintf("%g Mbit/s %v queue", float64(l.Rate)/1e6, l.Queue)
	}
	return fmt.Sprintf("%s, %v RTT, %.1f%% loss", rate, 2*l.Delay, 100*l.Loss)
}

// Net is a built link: the agent's and the browser's network stacks.
type Net struct {
	Agent, Browser *vnet.Net
	// TBF is the bottleneck toward the browser, nil when unshaped; tests
	// may change its rate mid-run.
	TBF  *vnet.TBFQueue
	stop func()
}

// Close stops the link.
func (n *Net) Close() { n.stop() }

const (
	agentIP   = "10.0.0.1"
	browserIP = "10.0.0.2"
)

// Build starts the link.
func (l Link) Build() (*Net, error) {
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR: "10.0.0.0/24", MinDelay: l.Delay, MaxJitter: l.Jitter, LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return nil, err
	}
	n := &Net{stop: func() { wan.Stop() }}
	if n.Agent, err = vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{agentIP}}); err != nil {
		return nil, err
	}
	if n.Browser, err = vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{browserIP}}); err != nil {
		return nil, err
	}
	var browser vnet.NIC = n.Browser
	if l.Rate > 0 {
		n.TBF = vnet.NewTBFQueue(l.Rate, 16_000, int64(float64(l.Rate)/8*l.Queue.Seconds()))
		q, err := vnet.NewQueue(n.Browser, n.TBF)
		if err != nil {
			return nil, err
		}
		browser = q
	}
	if err := wan.AddNet(n.Agent); err != nil {
		return nil, err
	}
	if err := wan.AddNet(browser); err != nil {
		return nil, err
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
	if err := wan.Start(); err != nil {
		return nil, err
	}
	return n, nil
}
