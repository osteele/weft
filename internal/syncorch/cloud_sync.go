package syncorch

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudproviders"
	"github.com/osteele/weft/internal/cloudsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/r2"
)

type CloudSyncOptions struct {
	// Context bounds the work. If nil, context.Background() is used. When
	// Timeout is also set, an inner ctx with that deadline is derived.
	Context     context.Context
	Timeout     time.Duration
	Verbose     bool
	Reconciler  any
	Clients     []any
	R2Client    any
	SyncResults bool

	LeaseScope string
	LeaseOwner string
	LeaseTTL   time.Duration
}

type CloudSyncResult struct {
	Updated         int
	ReconcileResult *campaign.ReconcileResult
	Completed       bool
	Warnings        []string
}

var (
	leaseOwnerOnce sync.Once
	leaseOwnerVal  string
)

func SyncCloud(cfg *config.Config, database *sql.DB, opts CloudSyncOptions) CloudSyncResult {
	parent := opts.Context
	if parent == nil {
		parent = context.Background()
	}
	if opts.Timeout <= 0 {
		result := syncCloud(parent, cfg, database, opts)
		result.Completed = true
		return result
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	done := make(chan CloudSyncResult, 1)
	go func() {
		defer cancel()
		done <- syncCloud(ctx, cfg, database, opts)
	}()
	select {
	case result := <-done:
		result.Completed = true
		return result
	case <-ctx.Done():
		// Caller is unblocked immediately; the worker drains in the background
		// once it observes ctx cancellation.
		return CloudSyncResult{
			Completed: false,
			Warnings:  []string{degraded.CloudSyncTimedOutWaitingForDB(opts.Timeout.String())},
		}
	}
}

func syncCloud(ctx context.Context, cfg *config.Config, database *sql.DB, opts CloudSyncOptions) CloudSyncResult {
	if cfg == nil {
		cfg = &config.Config{}
	}
	reconciler, _ := opts.Reconciler.(*campaign.Reconciler)
	if reconciler == nil {
		reconciler = campaign.NewReconciler()
	}

	clients := cloudClients(opts.Clients)
	r2Client, _ := opts.R2Client.(*r2.Client)
	if len(clients) == 0 {
		discovery := cloudproviders.Discover(cfg)
		clients = discovery.Clients
	}
	if r2Client == nil && cfg.Vastai.R2.Bucket != "" && cfg.Vastai.R2.AccessKeyID != "" {
		c, err := r2.New(r2.Config{
			AccountID:       cfg.Vastai.R2.AccountID,
			AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
			SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
			Bucket:          cfg.Vastai.R2.Bucket,
		})
		if err == nil {
			r2Client = c
		} else if opts.Verbose {
			fmt.Fprintf(os.Stderr, "Warning: R2 client: %v\n", err)
		}
	}

	if opts.LeaseScope != "" {
		owner := opts.LeaseOwner
		if owner == "" {
			owner = cloudSyncOwner()
		}
		ttl := opts.LeaseTTL
		if ttl <= 0 {
			ttl = 180 * time.Second
		}
		ok, err := db.AcquireAutoLease(database, opts.LeaseScope, owner, ttl)
		if err != nil {
			warnings := []string{fmt.Sprintf("cloud sync lease error: %v", err)}
			res := syncCloudWithClients(ctx, database, reconciler, nil, nil, opts, cfg)
			res.Warnings = append(res.Warnings, warnings...)
			return res
		}
		if !ok {
			return syncCloudWithClients(ctx, database, reconciler, nil, nil, opts, cfg)
		}
		defer func() {
			_ = db.ReleaseAutoLease(database, opts.LeaseScope, owner)
		}()
	}

	return syncCloudWithClients(ctx, database, reconciler, clients, r2Client, opts, cfg)
}

func syncCloudWithClients(ctx context.Context, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, opts CloudSyncOptions, cfg *config.Config) CloudSyncResult {
	syncResults := func(context.Context) int { return 0 }
	if opts.SyncResults {
		syncResults = func(ctx context.Context) int { return SyncCloudJobResults(ctx, cfg, database, opts.Verbose) }
	}
	result := cloudsync.SyncState(ctx, database, reconciler, clients, r2Client, syncResults)
	if opts.Verbose && result.ReconcileResult != nil && result.ReconcileResult.Reconciled > 0 {
		fmt.Printf("Reconciled %d cloud instance(s)\n", result.ReconcileResult.Reconciled)
	}
	return CloudSyncResult{Updated: result.Updated, ReconcileResult: result.ReconcileResult, Completed: true}
}

func cloudClients(in []any) []cloud.Client {
	if len(in) == 0 {
		return nil
	}
	out := make([]cloud.Client, 0, len(in))
	for _, v := range in {
		client, ok := v.(cloud.Client)
		if ok {
			out = append(out, client)
		}
	}
	return out
}

func cloudSyncOwner() string {
	leaseOwnerOnce.Do(func() {
		host, _ := os.Hostname()
		if host == "" {
			host = "unknown-host"
		}
		leaseOwnerVal = fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().UnixNano())
	})
	return leaseOwnerVal
}
