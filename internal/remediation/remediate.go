package remediation

import (
	"database/sql"
	"fmt"
	"log"
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
	Logger     *log.Logger
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
	diagnosis := DiagnoseFromLog(ctx.LogContent)
	if diagnosis == nil {
		return nil
	}

	result := &RemediationResult{Diagnosis: diagnosis}

	diagJSON, err := MarshalDiagnosis(diagnosis)
	if err != nil {
		ctx.Logger.Printf("marshal diagnosis for job %d: %v", ctx.Job.ID, err)
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
			ctx.Logger.Printf("build prestage plan for job %d: %v", ctx.Job.ID, err)
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return fmt.Sprintf("diagnosis only (prestage plan failed: %v)", err), false
		}

		if err := prestage.Execute(ctx.DB, plan, 10*time.Minute); err != nil {
			ctx.Logger.Printf("execute prestage for job %d: %v", ctx.Job.ID, err)
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return fmt.Sprintf("diagnosis only (prestage failed: %v)", err), false
		}

		return retryJob(ctx, diagJSON, fmt.Sprintf("pre-staged %v and retried", diagnosis.MissingAssets))

	case "missing_file":
		if ctx.Job.WorkingDir != "" && !remotesync.IsLocalHost(host) {
			if err := remotesync.SyncSources(host, ctx.Job.WorkingDir, ctx.Job.WorkingDir); err != nil {
				ctx.Logger.Printf("re-sync sources for job %d: %v", ctx.Job.ID, err)
			}
		}
		return retryJob(ctx, diagJSON, "re-synced sources and retried")
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
			ctx.Logger.Printf("coding agent for job %d: %v", ctx.Job.ID, err)
			storeDiagnosis(ctx, diagJSON, ctx.Job.RetryCount)
			return fmt.Sprintf("diagnosis only (agent error: %v)", err), false
		}

		if agentResult.Success && agentResult.Patch != "" {
			host := ctx.Job.Host
			if ctx.Job.WorkingDir != "" && !remotesync.IsLocalHost(host) {
				if err := remotesync.SyncSources(host, ctx.Job.WorkingDir, ctx.Job.WorkingDir); err != nil {
					ctx.Logger.Printf("re-sync after agent for job %d: %v", ctx.Job.ID, err)
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
		ctx.Logger.Printf("requeue job %d: %v", ctx.Job.ID, err)
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
		ctx.Logger.Printf("update diagnosis for job %d: %v", ctx.Job.ID, err)
	}
}
