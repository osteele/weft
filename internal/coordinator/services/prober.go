package services

import (
	"context"
	"sync"
	"time"
)

// HostProber periodically probes all known hosts for SSH connectivity.
type HostProber struct {
	hostState *HostStateManager
	interval  time.Duration
}

// NewHostProber creates a new host prober service.
func NewHostProber(hostState *HostStateManager, interval time.Duration) *HostProber {
	return &HostProber{
		hostState: hostState,
		interval:  interval,
	}
}

// Start runs the prober loop until the context is cancelled.
func (p *HostProber) Start(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.ProbeAllParallel()
		}
	}
}

// ProbeAll probes all known hosts for connectivity.
func (p *HostProber) ProbeAll() {
	hosts := p.hostState.AllHosts()
	for _, host := range hosts {
		p.hostState.ProbeHost(host)
	}
}

// ProbeAllParallel probes all known hosts concurrently.
func (p *HostProber) ProbeAllParallel() {
	hosts := p.hostState.AllHosts()
	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			p.hostState.ProbeHost(h)
		}(host)
	}
	wg.Wait()
}
