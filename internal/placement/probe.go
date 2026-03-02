package placement

import (
	"sync"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// ProbeHosts checks which hosts are reachable via SSH in parallel.
// Returns a map of host name → reachable (true if SSH "true" succeeds).
func ProbeHosts(hosts []string, timeout time.Duration) map[string]bool {
	result := make(map[string]bool, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			_, _, err := ssh.RunWithTimeout(h, "true", timeout)
			mu.Lock()
			result[h] = err == nil
			mu.Unlock()
		}(host)
	}

	wg.Wait()
	return result
}
