package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
)

const (
	overloadProbeTimeout       = 5 * time.Second
	overloadRunningMaxElapsed  = 10 * time.Minute
	overloadRunningNoPredLimit = 2 * time.Minute
	overloadRunningMaxFraction = 0.15
	overloadRunningMovesPerRun = 1
)

var autoPilotCollectOverloadMetrics = placement.CollectMetrics
var autoPilotUnplaceOverloadedJob = ops.UnplaceQueuedJob
var autoPilotMoveOverloadedJobToNewInstance = MoveQueuedJobToNewInstance

func drainOverloadedInventoryHosts(ctx context.Context, database *sql.DB, cfg *config.Config, scoped map[int64]struct{}, movingJobs map[int64]struct{}) (int, error) {
	hostNames, err := placement.LoadHostNames()
	if err != nil {
		return 0, fmt.Errorf("load host names for overload drain: %w", err)
	}
	if len(hostNames) == 0 {
		return 0, nil
	}
	metrics := autoPilotCollectOverloadMetrics(database, hostNames, overloadProbeTimeout)
	opts := placement.DefaultHostLoadOptions()
	overloaded := make(map[string]placement.HostLoadAssessment)
	for _, host := range hostNames {
		assessment := placement.AssessHostLoad(database, host, metrics[host], opts)
		if assessment.State != placement.HostLoadOverloaded {
			continue
		}
		overloaded[host] = assessment
	}
	if len(overloaded) == 0 {
		return 0, nil
	}

	statuses := []string{db.StatusQueued, db.StatusRunning, db.StatusStarting, db.StatusPaused}
	jobs, err := db.ListJobsByStatuses(database, statuses, "", "", 0, nil, "unprocessed")
	if err != nil {
		return 0, fmt.Errorf("list overload drain candidates: %w", err)
	}

	moved := 0
	runningMoves := 0
	for _, job := range jobs {
		if ctx != nil && ctx.Err() != nil {
			return moved, ctx.Err()
		}
		if !overloadDrainCandidate(job, scoped, movingJobs, overloaded) {
			continue
		}
		assessment := overloaded[strings.TrimSpace(job.Host)]
		reason := overloadDrainReason(job.Host, assessment)
		status := job.EffectiveStatus()
		if status == db.StatusQueued {
			n, err := drainQueuedOverloadedJob(database, cfg, job, reason, overloaded, metrics)
			if err != nil {
				return moved, err
			}
			moved += n
			continue
		}
		if runningMoves >= overloadRunningMovesPerRun || !overloadRunningEvacuationAllowed(job, time.Now()) {
			continue
		}
		result, err := autoPilotMoveOverloadedJobToNewInstance(database, job.ID, true)
		if err != nil {
			appendPlacementReason(database, job.ID, reason+"; running evacuation skipped: "+err.Error())
			oplog.LogJob("auto_pilot.overload_drain.skip_running", job.ID, job.Host, oplog.WithError(err), oplog.WithDetail(reason))
			continue
		}
		target := strings.TrimSpace(result.TargetDesc)
		if target == "" && result.InstanceID > 0 {
			target = ids.FormatInstanceID(result.InstanceID)
		}
		recordOverloadMove(database, job, reason, target)
		moved++
		runningMoves++
	}
	if moved > 0 {
		oplog.Log("auto_pilot.overload_drain", oplog.WithDetailf("jobs=%d overloaded_hosts=%d", moved, len(overloaded)))
	}
	return moved, nil
}

func overloadDrainCandidate(job *db.Job, scoped map[int64]struct{}, movingJobs map[int64]struct{}, overloaded map[string]placement.HostLoadAssessment) bool {
	if job == nil || !job.HasInventoryHost() {
		return false
	}
	if len(scoped) > 0 {
		if _, ok := scoped[job.ID]; !ok {
			return false
		}
	}
	if _, moving := movingJobs[job.ID]; moving {
		return false
	}
	if _, ok := overloaded[strings.TrimSpace(job.Host)]; !ok {
		return false
	}
	if jobPinnedToCurrentHost(job) {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusQueued:
		return true
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return !job.HasTag(db.TagInventory)
	default:
		return false
	}
}

func jobPinnedToCurrentHost(job *db.Job) bool {
	if job == nil {
		return true
	}
	if job.CLIResourceOverrides != nil && strings.TrimSpace(job.CLIResourceOverrides.Host) != "" {
		return true
	}
	// Older queued host assignments did not persist whether --host was explicit.
	// Treat them as pinned unless placement telemetry proves auto-placement.
	return job.PlacementMeta == nil
}

func drainQueuedOverloadedJob(database *sql.DB, cfg *config.Config, job *db.Job, reason string, overloaded map[string]placement.HostLoadAssessment, metrics map[string]*placement.HostMetrics) (int, error) {
	if host := alternateOnPremHost(database, cfg, job, overloaded, metrics); host != "" {
		n, err := MoveJobsToHost(database, []*db.Job{job}, host, JobMoveCallbacks{})
		if err != nil {
			return 0, fmt.Errorf("move %s from overloaded host %s to %s: %w", ids.FormatJobID(job.ID), job.Host, host, err)
		}
		recordOverloadMove(database, job, reason, host)
		return n, nil
	}
	if job.HasTag(db.TagInventory) {
		appendPlacementReason(database, job.ID, reason+"; waiting for non-overloaded on-prem host")
		return 0, nil
	}
	if _, err := autoPilotUnplaceOverloadedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
		return 0, fmt.Errorf("unplace %s from overloaded host %s: %w", ids.FormatJobID(job.ID), job.Host, err)
	}
	recordOverloadMove(database, job, reason, "unplaced")
	return 1, nil
}

func alternateOnPremHost(database *sql.DB, cfg *config.Config, job *db.Job, overloaded map[string]placement.HostLoadAssessment, metrics map[string]*placement.HostMetrics) string {
	if job == nil {
		return ""
	}
	constraints := placement.ConstraintsFromJob(job)
	predict := placement.BuildJobPredictorFromConfig(cfg, constraints)
	plan, err := placement.Evaluate(placement.EvaluateRequest{
		Constraints: constraints,
		Predictor:   predict,
		Sources:     []placement.CandidateSource{&placement.OnPremSource{Metrics: metrics}},
		Database:    database,
	})
	if err != nil || plan == nil {
		return ""
	}
	pick := plan.Fast
	if pick == nil {
		pick = plan.Cheap
	}
	if pick == nil || pick.Kind != placement.CandidateOnPrem || pick.OnPrem == nil {
		return ""
	}
	host := strings.TrimSpace(pick.OnPrem.Host)
	if host == "" || strings.EqualFold(host, strings.TrimSpace(job.Host)) {
		return ""
	}
	if overloaded[host].State == placement.HostLoadOverloaded {
		return ""
	}
	return host
}

func overloadRunningEvacuationAllowed(job *db.Job, now time.Time) bool {
	if job == nil || job.StartTime <= 0 || job.PlacementMeta == nil {
		return false
	}
	elapsed := now.Sub(time.Unix(job.StartTime, 0))
	if elapsed <= 0 || elapsed > overloadRunningMaxElapsed {
		return false
	}
	pred := job.PlacementMeta.PredictedDurationS
	if pred == nil || *pred <= 0 {
		return elapsed <= overloadRunningNoPredLimit
	}
	return elapsed.Seconds() <= *pred*overloadRunningMaxFraction
}

func overloadDrainReason(host string, assessment placement.HostLoadAssessment) string {
	reason := strings.TrimSpace(assessment.Reason)
	if reason == "" {
		return fmt.Sprintf("overload drain: %s overloaded", host)
	}
	return fmt.Sprintf("overload drain: %s overloaded (%s)", host, reason)
}

func recordOverloadMove(database *sql.DB, job *db.Job, reason, target string) {
	if job == nil {
		return
	}
	detail := reason
	if strings.TrimSpace(target) != "" {
		detail += "; moved to " + strings.TrimSpace(target)
	}
	appendPlacementReason(database, job.ID, detail)
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchAutoReplanned,
		JobID:     job.ID,
		Detail:    detail,
	})
	oplog.LogJob("auto_pilot.overload_drain", job.ID, job.Host, oplog.WithDetail(detail))
}
