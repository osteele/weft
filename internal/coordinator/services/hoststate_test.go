package services

import (
	"log/slog"
	"os"
	"sort"
	"sync"
	"testing"
)

func newTestHostStateManager() *HostStateManager {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return NewHostStateManager(logger)
}

func TestHostStateManager(t *testing.T) {
	t.Run("new manager has no hosts", func(t *testing.T) {
		m := newTestHostStateManager()
		if len(m.AllHosts()) != 0 {
			t.Errorf("AllHosts() = %v, want empty", m.AllHosts())
		}
		if len(m.OnlineHosts()) != 0 {
			t.Errorf("OnlineHosts() = %v, want empty", m.OnlineHosts())
		}
	})

	t.Run("MarkOffline creates entry and sets offline", func(t *testing.T) {
		m := newTestHostStateManager()
		m.MarkOffline("host1")

		if m.IsOnline("host1") {
			t.Error("host1 should be offline after MarkOffline")
		}
		hosts := m.AllHosts()
		if len(hosts) != 1 || hosts[0] != "host1" {
			t.Errorf("AllHosts() = %v, want [host1]", hosts)
		}
	})

	t.Run("IsOnline returns false for unknown host", func(t *testing.T) {
		m := newTestHostStateManager()
		if m.IsOnline("unknown") {
			t.Error("unknown host should not be online")
		}
	})

	t.Run("OnlineHosts returns only online hosts", func(t *testing.T) {
		m := newTestHostStateManager()

		// Manually set some hosts online/offline
		m.mu.Lock()
		m.hosts["host1"] = &HostState{Name: "host1", Online: true}
		m.hosts["host2"] = &HostState{Name: "host2", Online: false}
		m.hosts["host3"] = &HostState{Name: "host3", Online: true}
		m.mu.Unlock()

		online := m.OnlineHosts()
		sort.Strings(online)
		if len(online) != 2 {
			t.Fatalf("OnlineHosts() = %v, want 2 hosts", online)
		}
		if online[0] != "host1" || online[1] != "host3" {
			t.Errorf("OnlineHosts() = %v, want [host1, host3]", online)
		}
	})

	t.Run("AllHosts returns all tracked hosts", func(t *testing.T) {
		m := newTestHostStateManager()
		m.MarkOffline("a")
		m.MarkOffline("b")
		m.MarkOffline("c")

		all := m.AllHosts()
		sort.Strings(all)
		if len(all) != 3 {
			t.Fatalf("AllHosts() = %v, want 3 hosts", all)
		}
	})

	t.Run("Snapshot returns copy of all states", func(t *testing.T) {
		m := newTestHostStateManager()
		m.mu.Lock()
		m.hosts["host1"] = &HostState{Name: "host1", Online: true}
		m.hosts["host2"] = &HostState{Name: "host2", Online: false}
		m.mu.Unlock()

		snap := m.Snapshot()
		if len(snap) != 2 {
			t.Fatalf("Snapshot() has %d entries, want 2", len(snap))
		}
		if !snap["host1"].Online {
			t.Error("snapshot host1 should be online")
		}
		if snap["host2"].Online {
			t.Error("snapshot host2 should be offline")
		}

		// Mutating snapshot should not affect manager
		snap["host1"] = HostState{Name: "host1", Online: false}
		if !m.IsOnline("host1") {
			t.Error("mutating snapshot should not affect manager")
		}
	})
}

func TestHostStateManagerConcurrency(t *testing.T) {
	m := newTestHostStateManager()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := "host"
			if i%2 == 0 {
				m.MarkOffline(host)
			}
			m.IsOnline(host)
			m.OnlineHosts()
			m.AllHosts()
			m.Snapshot()
		}(i)
	}

	wg.Wait()
}
