package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

func fetchDiskFailureReportFromR2(r2Client *r2.Client, instanceID int64) (data []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("R2 fetch panicked", "component", "disk-postmortem", "instance", instanceID, "panic", r)
			data = nil
			err = nil
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r2Client.GetObject(ctx, r2keys.InstanceDiskFailure(instanceID))
}

// diskFailureReport mirrors the agent's disk failure report structure
// for the fields we need during sync-side reconciliation.
type diskFailureReport struct {
	HFCacheModels []hfCacheEntry `json:"hf_cache_models"`
}

type hfCacheEntry struct {
	AssetID   string `json:"asset_id"`
	SizeBytes int64  `json:"size_bytes"`
}

// ProcessDiskFailureReport fetches the disk failure report from R2 for an
// instance that failed with disk-full, extracts undeclared HF models, and
// stores them as observed_inputs on the affected jobs.
func ProcessDiskFailureReport(r2Client *r2.Client, instanceID int64, database *sql.DB) {
	if r2Client == nil || database == nil {
		return
	}

	data, err := fetchDiskFailureReportFromR2(r2Client, instanceID)
	if err != nil || len(data) == 0 {
		return
	}

	var report diskFailureReport
	if err := json.Unmarshal(data, &report); err != nil {
		slog.Warn("failed to parse disk failure report", "component", "disk-postmortem", "instance", instanceID, "error", err)
		return
	}
	if len(report.HFCacheModels) == 0 {
		return
	}

	// Get jobs associated with this instance
	jobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		slog.Warn("failed to get jobs for instance", "component", "disk-postmortem", "instance", instanceID, "error", err)
		return
	}

	// Build set of all observed HF model asset IDs from the cache scan
	observedSet := make(map[string]bool)
	for _, entry := range report.HFCacheModels {
		if strings.HasPrefix(entry.AssetID, "hf:") {
			observedSet[entry.AssetID] = true
		}
	}

	// For each job, find undeclared models (observed - declared)
	for _, job := range jobs {
		declaredSet := make(map[string]bool)
		for _, input := range job.Inputs {
			declaredSet[input] = true
		}

		var undeclared []string
		for assetID := range observedSet {
			if !declaredSet[assetID] {
				undeclared = append(undeclared, assetID)
			}
		}

		if len(undeclared) == 0 {
			continue
		}

		if err := db.SetJobObservedInputs(database, job.ID, undeclared); err != nil {
			slog.Warn("failed to set observed inputs for job", "component", "disk-postmortem", "job_id", job.ID, "error", err)
		} else {
			slog.Info("job has undeclared HF models", "component", "disk-postmortem", "job_id", job.ID, "count", len(undeclared), "models", undeclared)
		}
	}
}
