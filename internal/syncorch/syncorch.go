package syncorch

import (
	"database/sql"
	"sync"
	"time"

	"github.com/osteele/weft/internal/config"
)

type CloudMode string

const (
	CloudDisabled  CloudMode = "disabled"
	CloudBounded   CloudMode = "bounded"
	CloudUnbounded CloudMode = "unbounded"
)

type SyncOptions struct {
	Hosts             []string
	SSHTimeout        time.Duration
	HostTimeout       time.Duration
	CloudMode         CloudMode
	CloudTimeout      time.Duration
	Verbose           bool
	Full              bool
	StartQueueRunner  bool
	EnsureQueueRunner func(string) (bool, error)

	// Optional cloud overrides (used by worker/web).
	Clients    []any
	R2Client   any
	Reconciler any
	LeaseScope string
	LeaseOwner string
	LeaseTTL   time.Duration
}

type SyncResult struct {
	HostsUpdated     int
	HostsReached     int
	HostsUnreachable []string
	CloudUpdated     int
	Warnings         []string
	AllCompleted     bool
}

func SyncAll(database *sql.DB, cfg *config.Config, opts SyncOptions) SyncResult {
	var hostResult HostSyncResult
	var cloudResult CloudSyncResult
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		hostResult = SyncHosts(database, opts)
	}()

	if opts.CloudMode != CloudDisabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cloudOpts := CloudSyncOptions{
				Timeout:     opts.CloudTimeout,
				Verbose:     opts.Verbose,
				Reconciler:  opts.Reconciler,
				Clients:     opts.Clients,
				R2Client:    opts.R2Client,
				LeaseScope:  opts.LeaseScope,
				LeaseOwner:  opts.LeaseOwner,
				LeaseTTL:    opts.LeaseTTL,
				SyncResults: true,
			}
			if opts.CloudMode == CloudUnbounded {
				cloudOpts.Timeout = 0
			}
			cloudResult = SyncCloud(cfg, database, cloudOpts)
		}()
	}

	wg.Wait()

	result := SyncResult{
		HostsUpdated:     hostResult.Updated,
		HostsReached:     hostResult.Reached,
		HostsUnreachable: hostResult.Unreachable,
		CloudUpdated:     cloudResult.Updated,
		Warnings:         append([]string{}, hostResult.Warnings...),
		AllCompleted:     hostResult.Completed,
	}
	result.Warnings = append(result.Warnings, cloudResult.Warnings...)
	if opts.CloudMode != CloudDisabled && !cloudResult.Completed {
		result.AllCompleted = false
	}
	return result
}
