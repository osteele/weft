package syncorch

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/ssh"
)

type HostSyncResult struct {
	Updated     int
	Reached     int
	Unreachable []string // hosts that produced a hard connection failure (offline)
	Slow        []string // hosts that produced a non-connection error or hit our deadline (alive but unresponsive within budget)
	Warnings    []string
	Completed   bool
}

// hostSyncFn is the per-host sync entry point. Overridden in tests.
var hostSyncFn = syncHostWithTimeoutDetailed

func SyncHosts(database *sql.DB, opts SyncOptions) HostSyncResult {
	hosts := uniqueHosts(opts.Hosts)
	if len(hosts) == 0 {
		var err error
		hosts, err = db.ListUniqueActiveHosts(database)
		if err != nil || len(hosts) == 0 {
			return HostSyncResult{Completed: true}
		}
		hosts = uniqueHosts(hosts)
	}
	if len(hosts) == 0 {
		return HostSyncResult{Completed: true}
	}

	sshTimeout := opts.SSHTimeout
	if sshTimeout <= 0 {
		sshTimeout = FastSSHTimeout
	}
	hostTimeout := opts.HostTimeout
	if hostTimeout <= 0 {
		if opts.StartQueueRunner {
			hostTimeout = NormalHostTimeout
		} else {
			hostTimeout = FastHostTimeout
		}
	}

	result := HostSyncResult{Completed: true}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, host := range hosts {
		host := host
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan struct {
				res ops.HostSyncResult
				err error
			}, 1)
			go func() {
				res, err := hostSyncFn(database, host, sshTimeout, hostTimeout, opts.StartQueueRunner, opts.EnsureQueueRunner)
				done <- struct {
					res ops.HostSyncResult
					err error
				}{res: res, err: err}
			}()

			select {
			case out := <-done:
				mu.Lock()
				defer mu.Unlock()
				if out.err != nil {
					result.Completed = false
					if ssh.IsConnectionError(out.err.Error()) {
						result.Unreachable = append(result.Unreachable, host)
					} else {
						result.Slow = append(result.Slow, host)
						if opts.Verbose {
							result.Warnings = append(result.Warnings, fmt.Sprintf("Warning: quick sync %s failed: %v", host, out.err))
						}
					}
					return
				}
				result.Updated += out.res.Updated
				result.Reached++
				result.Warnings = append(result.Warnings, hostSyncWarnings(host, out.res)...)
			case <-time.After(hostTimeout):
				mu.Lock()
				result.Completed = false
				result.Slow = append(result.Slow, host)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return result
}

func syncHostWithTimeoutDetailed(database *sql.DB, host string, timeout, sourceTimeout time.Duration, startQueueRunner bool, ensureQueueRunner func(string) (bool, error)) (ops.HostSyncResult, error) {
	onQueueStart := func(string) (bool, error) { return false, nil }
	if startQueueRunner && ensureQueueRunner != nil {
		onQueueStart = ensureQueueRunner
	}
	return ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:       timeout,
		SourceTimeout: sourceTimeout,
		SkipSamples:   true,
		UseBatchSync:  true,
		NoQueueStart:  !startQueueRunner,
		Logger:        ops.NewSilentSyncLogger(),
	}, onQueueStart)
}

func hostSyncWarnings(host string, result ops.HostSyncResult) []string {
	var warnings []string
	if result.QueueDispatchError != "" {
		warnings = append(warnings, fmt.Sprintf("Warning: queued jobs were not dispatched on %s: %s", host, result.QueueDispatchError))
	}
	if result.QueueRunnerError != "" {
		warnings = append(warnings, fmt.Sprintf("Warning: queue runner error on %s: %s", host, result.QueueRunnerError))
	}
	return warnings
}

func uniqueHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}
