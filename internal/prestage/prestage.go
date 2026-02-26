// Package prestage transfers missing input data to a target host before
// dispatching a job, so jobs don't fail or run slowly due to missing data.
package prestage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
)

// OpPreStage is the oplog operation for pre-staging transfers.
const OpPreStage = "prestage.transfer"

// Transfer describes a single data transfer needed before dispatch.
type Transfer struct {
	Asset      dataloc.DataAsset
	SourceHost string
	SizeBytes  int64
	RemotePath string // path on the source host
}

// Plan describes all transfers needed to prepare a target host for a job.
type Plan struct {
	Host      string
	Transfers []Transfer
}

// TotalBytes returns the total bytes to transfer.
func (p *Plan) TotalBytes() int64 {
	var total int64
	for _, t := range p.Transfers {
		total += t.SizeBytes
	}
	return total
}

// BuildPlan determines which inputs are missing on the target host and
// identifies source hosts that have them. Returns a plan with zero
// transfers if all data is already local.
func BuildPlan(db *sql.DB, targetHost string, inputs []string) (*Plan, error) {
	plan := &Plan{Host: targetHost}

	for _, ref := range inputs {
		asset, ok := dataloc.ParseAssetRef(ref)
		if !ok {
			continue
		}

		entries, err := dataloc.FindAssetHosts(db, asset)
		if err != nil {
			continue
		}

		// Check if target already has it
		isLocal := false
		for _, e := range entries {
			if e.Host == targetHost {
				isLocal = true
				break
			}
		}
		if isLocal {
			continue
		}

		// Find best source (prefer largest known copy for integrity)
		var bestSource *dataloc.HostDataEntry
		for i := range entries {
			e := &entries[i]
			if e.Host == targetHost {
				continue
			}
			if e.Path == "" {
				continue
			}
			if bestSource == nil || e.SizeBytes > bestSource.SizeBytes {
				bestSource = e
			}
		}

		if bestSource == nil {
			// No source with a known path — skip
			continue
		}

		plan.Transfers = append(plan.Transfers, Transfer{
			Asset:      asset,
			SourceHost: bestSource.Host,
			SizeBytes:  bestSource.SizeBytes,
			RemotePath: bestSource.Path,
		})
	}

	return plan, nil
}

// Execute runs all transfers in the plan via rsync over SSH.
// Each transfer rsyncs from sourceHost to targetHost.
// Returns nil if all transfers succeed; returns the first error encountered
// but attempts all transfers (best-effort).
func Execute(plan *Plan, timeout time.Duration) error {
	if len(plan.Transfers) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var firstErr error
	for _, t := range plan.Transfers {
		start := time.Now()
		err := runTransfer(ctx, t, plan.Host)
		elapsed := time.Since(start)

		if err != nil {
			oplog.Log(OpPreStage,
				oplog.WithHost(plan.Host),
				oplog.WithDetailf("FAILED %s from %s (%d bytes)", t.Asset, t.SourceHost, t.SizeBytes),
				oplog.WithError(err),
				oplog.WithDuration(elapsed),
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("transfer %s from %s to %s: %w", t.Asset, t.SourceHost, plan.Host, err)
			}
			continue
		}

		oplog.Log(OpPreStage,
			oplog.WithHost(plan.Host),
			oplog.WithDetailf("%s from %s (%d bytes)", t.Asset, t.SourceHost, t.SizeBytes),
			oplog.WithDuration(elapsed),
		)
	}

	return firstErr
}

// runTransfer executes a single rsync from sourceHost:remotePath to targetHost:remotePath.
func runTransfer(ctx context.Context, t Transfer, targetHost string) error {
	// rsync from source to target via the coordinator (SSH hop)
	// Format: ssh sourceHost "rsync -az <path> targetHost:<path>"
	cmd := fmt.Sprintf("rsync -az --timeout=300 %s %s:%s",
		t.RemotePath,
		targetHost,
		t.RemotePath,
	)
	_, stderr, err := ssh.RunWithContext(ctx, t.SourceHost, cmd)
	if err != nil {
		return fmt.Errorf("rsync: %s: %w", stderr, err)
	}
	return nil
}
