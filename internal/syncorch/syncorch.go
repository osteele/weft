package syncorch

import (
	"context"
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
	Context           context.Context
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
	HostsUnreachable []string // hosts that produced a hard connection failure (offline)
	HostsSlow        []string // hosts whose sync hit a non-connection error or our deadline (alive but slow)
	CloudUpdated     int
	Warnings         []string
	AllCompleted     bool
}

func SyncAll(database *sql.DB, cfg *config.Config, opts SyncOptions) SyncResult {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	opts.Context = ctx
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
				Context:     ctx,
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
		HostsSlow:        hostResult.Slow,
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
