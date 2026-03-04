// Package services provides composable background services extracted from
// the coordinator daemon. Each service can be used standalone (e.g., embedded
// in the TUI monitor) or composed into the full coordinator.
package services

import (
	"log"
	"sync"
	"time"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
)

// HostState tracks the online/offline state of a remote host.
type HostState struct {
	Name       string
	Online     bool
	LastProbe  time.Time
	LastOnline time.Time
}

// HostStateManager maintains the liveness state of all known hosts.
// It is goroutine-safe and shared between services and schedulers.
type HostStateManager struct {
	mu     sync.Mutex
	hosts  map[string]*HostState
	logger *log.Logger
}

// NewHostStateManager creates a new host state manager.
func NewHostStateManager(logger *log.Logger) *HostStateManager {
	return &HostStateManager{
		hosts:  make(map[string]*HostState),
		logger: logger,
	}
}

// ProbeHost checks if a host is reachable via SSH. Returns true if online.
func (m *HostStateManager) ProbeHost(host string) bool {
	_, _, err := ssh.RunWithTimeout(host, "true", 5*time.Second)
	online := err == nil

	m.mu.Lock()
	hs, ok := m.hosts[host]
	if !ok {
		hs = &HostState{Name: host}
		m.hosts[host] = hs
	}
	wasOnline := hs.Online
	hs.Online = online
	hs.LastProbe = time.Now()
	if online {
		hs.LastOnline = time.Now()
	}
	m.mu.Unlock()

	if online && !wasOnline {
		m.logger.Printf("host %s came online", host)
		oplog.Log(oplog.OpHostConnect, oplog.WithHost(host))
	} else if !online && wasOnline {
		m.logger.Printf("host %s went offline", host)
		oplog.Log(oplog.OpHostTimeout, oplog.WithHost(host))
	}

	return online
}

// MarkOffline updates a host's state to offline.
func (m *HostStateManager) MarkOffline(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs, ok := m.hosts[host]
	if !ok {
		hs = &HostState{Name: host}
		m.hosts[host] = hs
	}
	hs.Online = false
	hs.LastProbe = time.Now()
}

// IsOnline returns whether a host is considered online.
func (m *HostStateManager) IsOnline(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs, ok := m.hosts[host]
	return ok && hs.Online
}

// OnlineHosts returns a list of hosts that are currently online.
func (m *HostStateManager) OnlineHosts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var hosts []string
	for name, hs := range m.hosts {
		if hs.Online {
			hosts = append(hosts, name)
		}
	}
	return hosts
}

// AllHosts returns a list of all tracked host names.
func (m *HostStateManager) AllHosts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	hosts := make([]string, 0, len(m.hosts))
	for name := range m.hosts {
		hosts = append(hosts, name)
	}
	return hosts
}

// Snapshot returns a copy of all host states.
func (m *HostStateManager) Snapshot() map[string]HostState {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]HostState, len(m.hosts))
	for name, hs := range m.hosts {
		result[name] = *hs
	}
	return result
}

// SeedFromInventory populates the host state map from the embedded inventory.
func (m *HostStateManager) SeedFromInventory() {
	hosts, err := inventory.LoadEmbeddedHosts()
	if err != nil {
		m.logger.Printf("load inventory: %v", err)
		return
	}

	m.mu.Lock()
	for _, h := range hosts {
		if _, ok := m.hosts[h.Name]; !ok {
			m.hosts[h.Name] = &HostState{Name: h.Name}
		}
	}
	m.mu.Unlock()
}
