package remediation

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/prestage"
	remotesync "github.com/osteele/weft/internal/sync"
)

// RemediationContext holds the dependencies needed for remediation.
type RemediationContext struct {
	DB         *sql.DB
	Job        *db.Job
	LogContent string
	Logger     *slog.Logger
	Config     *config.Config
}

// RemediationResult describes what action was taken.
type RemediationResult struct {
	Diagnosis *ErrorDiagnosis
	Retried   bool
	Action    string // human-readable action taken
}

// AttemptRemediation diagnoses a failed job's log and, if possible, remediates
// and retries it. Returns nil if no known error pattern is found.
func AttemptRemediation(ctx RemediationContext) *RemediationResult {
	diagnosis := DiagnoseFailedAttemptFromLog(ctx.LogContent, "post")
	if diagnosis == nil {
		return nil
	}

	// Enrich gpu_oom diagnoses with the host's GPU capacity
	if diagnosis.Pattern == "gpu_oom" && ctx.Job.Host != "" {
		if capGB := inventory.HostMaxGPUMemoryGB(ctx.Job.Host); capGB > 0 {
			diagnosis.GPUCapacityGB = capGB
		}
	}

	result := &RemediationResult{Diagnosis: diagnosis}

	diagJSON, err := MarshalDiagnosis(diagnosis)
	if err != nil {
		ctx.Logger.Warn("failed to marshal diagnosis", "job_id", ctx.Job.ID, "error", err)
		return result
	}

	// Don't retry if already retried
	if ctx.Job.RetryCount >= 1 {
		result.Action = "diagnosis only (retry limit reached)"
		storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
		return result
	}

	switch diagnosis.Category {
	case "data":
		result.Action, result.Retried = remediateData(ctx, diagnosis, diagJSON)
	case "code":
		result.Action, result.Retried = remediateCode(ctx, diagnosis, diagJSON)
	default:
		result.Action = "diagnosis only (not remediable)"
		storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
	}

	return result
}

// remediateData handles data-related errors: pre-stage missing data and retry.
// Returns the action description and whether a retry was attempted.
func remediateData(ctx RemediationContext, diagnosis *ErrorDiagnosis, diagJSON string) (string, bool) {
	if !diagnosis.Remediable {
		storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
		return "diagnosis only (not remediable)", false
	}

	host := ctx.Job.Host

	switch diagnosis.Pattern {
	case "missing_hf_model", "missing_hf_dataset":
		if len(diagnosis.MissingAssets) == 0 {
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return "diagnosis only (no asset refs extracted)", false
		}

		plan, err := prestage.BuildPlan(ctx.DB, host, inventory.HostHFCacheDir(host), diagnosis.MissingAssets)
		if err != nil {
			ctx.Logger.Warn("failed to build prestage plan", "job_id", ctx.Job.ID, "error", err)
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return fmt.Sprintf("diagnosis only (prestage plan failed: %v)", err), false
		}

		if err := prestage.Execute(ctx.DB, plan, 10*time.Minute); err != nil {
			ctx.Logger.Warn("failed to execute prestage", "job_id", ctx.Job.ID, "error", err)
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return fmt.Sprintf("diagnosis only (prestage failed: %v)", err), false
		}

		return retryJob(ctx, diagJSON, fmt.Sprintf("pre-staged %v and retried", diagnosis.MissingAssets))

	case "missing_file":
		if ctx.Job.WorkingDir != "" && !remotesync.IsLocalHost(host) {
			if err := remotesync.SyncSources(host, ctx.Job.WorkingDir, ctx.Job.WorkingDir); err != nil {
				ctx.Logger.Warn("failed to re-sync sources", "job_id", ctx.Job.ID, "error", err)
			}
		}
		return retryJob(ctx, diagJSON, "re-synced sources and retried")

	case "hf_transient_network":
		// The asset exists and is reachable; only the live fetch was
		// interrupted by a transient reset. Re-queue and retry — a later pass
		// after the blip clears succeeds. No pre-staging (the asset is
		// reachable) and no host clear (that would re-route an on-prem job
		// through the rental path); declaring the input is the permanent fix.
		return retryJob(ctx, diagJSON, "transient HF network reset; re-queued and retried")
	}

	storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
	return "diagnosis only (unknown data pattern)", false
}

// remediateCode handles code errors: invoke coding agent if configured.
// Returns the action description and whether a retry was attempted.
func remediateCode(ctx RemediationContext, diagnosis *ErrorDiagnosis, diagJSON string) (string, bool) {
	if ctx.Config != nil && ctx.Config.Remediation.CodingAgent != "" {
		agentResult, err := InvokeCodingAgent(ctx.Config.Remediation, ctx.Job, diagnosis)
		if err != nil {
			ctx.Logger.Warn("coding agent failed", "job_id", ctx.Job.ID, "error", err)
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return fmt.Sprintf("diagnosis only (agent error: %v)", err), false
		}

		if agentResult.Success && agentResult.Patch != "" {
			host := ctx.Job.Host
			if ctx.Job.WorkingDir != "" && !remotesync.IsLocalHost(host) {
				if err := remotesync.SyncSources(host, ctx.Job.WorkingDir, ctx.Job.WorkingDir); err != nil {
					ctx.Logger.Warn("failed to re-sync after agent", "job_id", ctx.Job.ID, "error", err)
				}
			}
			return retryJob(ctx, diagJSON, "coding agent patched and retried")
		}

		storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
		return "diagnosis only (agent did not produce a fix)", false
	}

	storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
	return "diagnosis only (no coding agent configured)", false
}

// retryJob requeues the failed job (same ID) and updates the diagnosis/retry count.
// Returns the action description and whether the retry succeeded.
func retryJob(ctx RemediationContext, diagJSON string, action string) (string, bool) {
	newRetryCount := ctx.Job.RetryCount + 1

	_, err := ops.RequeueJob(ctx.DB, ctx.Job, ops.ExecuteOptions{})
	if err != nil {
		ctx.Logger.Warn("failed to requeue job", "job_id", ctx.Job.ID, "error", err)
		storeDiagnosis(ctx, diagJSON, newRetryCount)
		return fmt.Sprintf("diagnosis found but retry failed: %v", err), false
	}

	// Store diagnosis after requeue: RequeueByID clears error_diagnosis,
	// so we must write it back. The job won't start instantly (requires
	// remote queue poll), so this is safe in practice.
	storeDiagnosis(ctx, diagJSON, newRetryCount)
	return action, true
}

// storeDiagnosis persists the diagnosis to the database, if available.
func storeDiagnosis(ctx RemediationContext, diagJSON string, retryCount int) {
	if ctx.DB == nil {
		return
	}
	if err := db.UpdateErrorDiagnosis(ctx.DB, ctx.Job.ID, diagJSON, retryCount); err != nil {
		ctx.Logger.Warn("failed to update diagnosis", "job_id", ctx.Job.ID, "error", err)
	}
}
