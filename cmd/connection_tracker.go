package cmd

import "fmt"

// hostConnectionTracker tracks connection states per host to avoid noisy logs.
type hostConnectionTracker struct {
	down map[string]bool
}

func newHostConnectionTracker() *hostConnectionTracker {
	return &hostConnectionTracker{
		down: make(map[string]bool),
	}
}

func (t *hostConnectionTracker) MarkDown(host string) {
	if t == nil || host == "" {
		return
	}
	if t.down[host] {
		return
	}
	t.down[host] = true
	fmt.Printf("Connection to %s is unavailable. Polling will continue...\n", host)
}

func (t *hostConnectionTracker) MarkUp(host string, shouldAnnounce bool) {
	if t == nil || host == "" {
		return
	}
	if !t.down[host] {
		return
	}
	delete(t.down, host)
	if shouldAnnounce {
		fmt.Printf("Connection to %s restored. Resuming monitoring.\n", host)
	}
}
