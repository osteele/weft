package orchestration

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/placement"
)

// recordAutoPilotReuseDecision persists why a job was assigned to an existing
// instance. The co-location count (RunningJobCount) is the key signal: a job
// queued behind others on a single-GPU instance runs serially, which is what
// `weft job diagnose` needs to explain. Best-effort — failures are logged,
// never fatal to a placement pass.
func recordAutoPilotReuseDecision(database *sql.DB, job *db.Job, inst campaign.InstanceCapacity, rejected []blockreason.ReuseRejection) {
	if database == nil || job == nil || inst.Instance == nil {
		return
	}
	target := ids.FormatInstanceID(inst.Instance.ID)
	why := "reused running instance (GPU free)"
	if inst.RunningJobCount > 0 {
		why = fmt.Sprintf("reused running instance (queued behind %d job(s) on its GPU)", inst.RunningJobCount)
	}
	recordAutoPilotPlacementDecision(database, job.ID, "autopilot_reuse", "reuse-instance", target,
		db.PlacementDecisionDetails{
			Instance:        target,
			GPU:             inst.Instance.DisplayGPUBrief(),
			ColocatedBehind: inst.RunningJobCount,
			Why:             why,
			Rejected:        reuseRejectionStrings(rejected),
		})
}

// recordAutoPilotLaunchDecisions reads back each freshly launched instance and
// records, per job placed on it, why a new instance was launched rather than
// reusing a running one. Best-effort.
func recordAutoPilotLaunchDecisions(database *sql.DB, instanceIDs []int64) {
	if database == nil {
		return
	}
	for _, instanceID := range instanceIDs {
		if instanceID <= 0 {
			continue
		}
		inst, err := db.GetLaunch(database, instanceID)
		if err != nil || inst == nil {
			continue
		}
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID)
		if err != nil {
			continue
		}
		target := ids.FormatInstanceID(instanceID)
		details := db.PlacementDecisionDetails{
			Instance:    target,
			GPU:         inst.DisplayGPUBrief(),
			CostPerHour: formatRateCents(inst.CostPerHourCents) + "/hr",
			Why:         "no reusable instance matched; launched a new instance",
		}
		for _, job := range jobs {
			if job == nil || job.ID <= 0 {
				continue
			}
			recordAutoPilotPlacementDecision(database, job.ID, "autopilot_launch", "launch-instance", target, details)
		}
	}
}

func recordAutoPilotPlacementDecision(database *sql.DB, jobID int64, operation, selectedKind, target string, details db.PlacementDecisionDetails) {
	var attemptPtr *int64
	if attemptID, err := db.GetLatestAttemptID(database, jobID); err == nil && attemptID > 0 {
		attemptPtr = &attemptID
	}
	if _, err := db.RecordPlacementDecision(database, db.PlacementDecision{
		JobID:                  &jobID,
		AttemptID:              attemptPtr,
		DecisionKind:           "acted",
		Operation:              operation,
		PlacementPolicyVersion: placement.PolicyVersion,
		SelectedKind:           selectedKind,
		SelectedTarget:         target,
		Details:                details,
	}, nil); err != nil {
		slog.Warn("failed to record autopilot placement decision",
			"job_id", jobID, "operation", operation, "error", err)
	}
}

func reuseRejectionStrings(rejected []blockreason.ReuseRejection) []string {
	if len(rejected) == 0 {
		return nil
	}
	out := make([]string, 0, len(rejected))
	for _, r := range rejected {
		if r.Instance == "" && r.Reason == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s", r.Instance, r.Reason))
	}
	return out
}
