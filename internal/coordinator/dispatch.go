package coordinator

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/prestage"
	srcsync "github.com/osteele/weft/internal/sync"
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

	if err := prestage.Execute(plan, 10*time.Minute); err != nil {
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
		Project:  projectFromIntentDir(i.Job.Dir),
	}

	predict := buildJobPredictorFromConfig(cfg, constraints)

	host, reasons, err := placement.BestHostWithPredictor(database, constraints, nil, predict)
	if err != nil {
		return "", nil, fmt.Errorf("placement: %w", err)
	}
	return host, reasons, nil
}

// buildJobPredictorFromConfig creates a JobPredictor from the app config and constraints.
func buildJobPredictorFromConfig(cfg *config.Config, c placement.Constraints) placement.JobPredictor {
	if cfg == nil {
		return nil
	}
	pcfg := predictor.BuildConfig(
		cfg.Predictor.ProjectPath,
		cfg.Predictor.ModelDir,
		cfg.Predictor.RetrainInterval,
		cfg.Predictor.DBPaths,
	)
	if !pcfg.Configured() || c.Command == "" {
		return nil
	}
	return placement.NewJobPredictor(func(host string) *placement.RawPrediction {
		result, err := predictor.Predict(pcfg, host, c.Project, c.GPUClass, c.Command)
		if err != nil {
			return nil
		}
		raw := &placement.RawPrediction{}
		if result.DurationS != nil {
			raw.DurationS = &placement.RawPredictionField{Mean: result.DurationS.Mean, Lower: result.DurationS.Lower, Upper: result.DurationS.Upper}
		}
		if result.PeakRSSKB != nil {
			raw.PeakRSSKB = &placement.RawPredictionField{Mean: result.PeakRSSKB.Mean, Lower: result.PeakRSSKB.Lower, Upper: result.PeakRSSKB.Upper}
		}
		if result.MaxGPUMemMiB != nil {
			raw.MaxGPUMemMiB = &placement.RawPredictionField{Mean: result.MaxGPUMemMiB.Mean, Lower: result.MaxGPUMemMiB.Lower, Upper: result.MaxGPUMemMiB.Upper}
		}
		return raw
	})
}

// projectFromIntentDir extracts a project name from an intent's working directory.
func projectFromIntentDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Base(dir)
}

// syncSources rsyncs the intent's working directory from the coordinator to the
// target host. Failures are logged but do not block dispatch.
func syncSources(i *intent.Intent, host string, logger *log.Logger) {
	dir := i.Job.Dir
	if dir == "" {
		return
	}

	// Expand ~ to coordinator's home directory for the local path
	localDir := dir
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			logger.Printf("sync sources: cannot resolve home dir: %v", err)
			return
		}
		localDir = filepath.Join(home, dir[2:])
	}

	// Check that the directory exists on the coordinator
	if _, err := os.Stat(localDir); os.IsNotExist(err) {
		logger.Printf("sync sources: %s not found on coordinator, skipping", localDir)
		return
	}

	if err := srcsync.SyncSources(host, localDir, dir); err != nil {
		logger.Printf("sync sources to %s for intent %s: %v (continuing)", host, i.IntentID, err)
	}

	// Sync extra file paths (from --input flags and .weft.yaml)
	extraPaths := collectIntentExtraPaths(i, localDir)
	if len(extraPaths) > 0 {
		if err := srcsync.SyncExtraPaths(host, extraPaths); err != nil {
			logger.Printf("sync extra paths to %s for intent %s: %v (continuing)", host, i.IntentID, err)
		}
	}
}

// collectIntentExtraPaths gathers file paths to sync from the intent's inputs
// and the project's .weft.yaml config.
func collectIntentExtraPaths(i *intent.Intent, localDir string) []string {
	_, filePaths := dataloc.ClassifyInputs(i.Job.Inputs)
	filePaths = append(filePaths, config.ProjectExtraPaths(localDir)...)
	return filePaths
}
