package coordinator

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/prestage"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// prestageInputs runs pre-staging for the intent's inputs on the target host.
// Logs transfers but does not block dispatch on failure (graceful degradation).
func prestageInputs(database *sql.DB, i *intent.Intent, host string, logger *log.Logger) {
	if len(i.Job.Inputs) == 0 {
		return
	}

	plan, err := prestage.BuildPlan(database, host, i.Job.Inputs)
	if err != nil {
		logger.Printf("prestage plan for %s: %v", i.IntentID, err)
		return
	}
	if len(plan.Transfers) == 0 {
		return
	}

	logger.Printf("pre-staging %d inputs (%d bytes) to %s for intent %s",
		len(plan.Transfers), plan.TotalBytes(), host, i.IntentID)

	if err := prestage.Execute(database, plan, 10*time.Minute); err != nil {
		logger.Printf("prestage failed for %s (continuing with dispatch): %v", i.IntentID, err)
	}
}

// dispatchIntent creates a job record and appends it to the remote queue.
// Returns the job ID and any error.
func dispatchIntent(database *sql.DB, i *intent.Intent, host string) (int64, error) {
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  i.Job.Dir,
		Command:     i.Job.Cmd,
		Description: i.Job.Desc,
		EnvVars:     i.Job.Env,
		Tags:        i.Job.Tags,
		GPUClass:    i.Job.Constraints.GPUClass,
		GPUMemGB:    i.Job.GPUMemGB,
		DepSpec:     i.Job.DepSpec,
		Inputs:      i.Job.Inputs,
		Outputs:     i.Job.Outputs,
	}

	result, err := ops.QueueJob(database, params, ops.DefaultOptions())
	if err != nil {
		return 0, fmt.Errorf("queue job on %s: %w", host, err)
	}
	return result.JobID, nil
}

// resolveHost determines which host should run the job described by the intent.
// If the intent specifies a host constraint, that host is used directly.
// Otherwise, placement scoring selects the best host, using the predictor
// for duration and resource estimates when configured.
func resolveHost(database *sql.DB, i *intent.Intent, cfg *config.Config) (string, []string, error) {
	if i.Job.Constraints.Host != "" {
		return i.Job.Constraints.Host, []string{"explicit host"}, nil
	}

	constraints := placement.Constraints{
		GPUClass: i.Job.Constraints.GPUClass,
		GPUMemGB: i.Job.Constraints.GPUMemGB,
		Inputs:   i.Job.Inputs,
		Command:  i.Job.Cmd,
		Project:  workdir.ProjectName(i.Job.Dir),
		Tags:     i.Job.Tags,
	}

	predict := placement.BuildJobPredictorFromConfig(cfg, constraints)

	result, err := placement.BestHostWithPredictor(database, constraints, nil, predict)
	if err != nil {
		return "", nil, fmt.Errorf("placement: %w", err)
	}

	// Log placement decision
	oplog.Log(oplog.OpPlacementDecided,
		oplog.WithHost(result.Host),
		oplog.WithDetail(placement.FormatPlacementDetail(result)))

	return result.Host, result.Reasons, nil
}

// syncSources rsyncs the intent's working directory from the coordinator to the
// target host. Failures are logged but do not block dispatch.
func syncSources(i *intent.Intent, host string, logger *log.Logger) {
	dir := i.Job.Dir
	if dir == "" {
		return
	}

	localDir := srcsync.ExpandTildeDir(dir)
	if localDir == "" {
		logger.Printf("sync sources: cannot resolve dir %s", dir)
		return
	}

	// Check that the directory exists on the coordinator
	if _, err := os.Stat(localDir); os.IsNotExist(err) {
		logger.Printf("sync sources: %s not found on coordinator, skipping", localDir)
		return
	}

	if err := srcsync.SyncSourcesToHost(host, localDir, dir, i.Job.Inputs); err != nil {
		logger.Printf("sync sources to %s failed for %s: %v", host, dir, err)
	}
}
