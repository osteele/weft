package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/placement"
)

const rentalToOnPremImprovementEpsilon = 0.01

var autoPilotLoadRentalOnPremHostNames = placement.LoadHostNames
var autoPilotCollectRentalOnPremMetrics = placement.CollectMetrics

func rebalanceQueuedRentalJobsToOnPrem(ctx context.Context, database *sql.DB, cfg *config.Config, scoped map[int64]struct{}, movingJobs map[int64]struct{}) (int, error) {
	result, err := RebalanceQueuedRentalJobsToOnPrem(ctx, database, cfg, QueueRebalanceOptions{
		Apply:      true,
		JobScope:   scoped,
		MovingJobs: movingJobs,
		Operation:  "auto_pilot.rebalance_onprem",
	})
	if err != nil {
		return 0, err
	}
	return len(result.Moves), nil
}

func RebalanceQueuedRentalJobsToOnPrem(ctx context.Context, database *sql.DB, cfg *config.Config, opts QueueRebalanceOptions) (QueueRebalanceResult, error) {
	if database == nil {
		return QueueRebalanceResult{}, nil
	}
	emitRebalanceProgress(opts.Progress, "Loading on-prem host state…")
	hostNames, err := autoPilotLoadRentalOnPremHostNames()
	if err != nil || len(hostNames) == 0 {
		return QueueRebalanceResult{}, err
	}
	metrics := autoPilotCollectRentalOnPremMetrics(database, hostNames, 5*time.Second)
	if len(metrics) == 0 {
		return QueueRebalanceResult{}, nil
	}

	instanceState, err := buildRebalanceInstanceState(database)
	if err != nil {
		return QueueRebalanceResult{}, err
	}
	if len(instanceState) == 0 {
		return QueueRebalanceResult{}, nil
	}
	rentalPlan := &rebalancePlan{instances: instanceState}
	runtimes := loadRuntimePredictions(database, rentalPlan.allJobs(), nil)

	jobs, err := db.ListJobsByStatuses(database, []string{db.StatusQueued, db.StatusPendingPlacement}, "", "", 0, nil, "unprocessed")
	if err != nil {
		return QueueRebalanceResult{}, fmt.Errorf("list queued rental jobs for on-prem rebalance: %w", err)
	}

	emitRebalanceProgress(opts.Progress, fmt.Sprintf("Scoring %d queued rental job(s) against on-prem hosts…", len(jobs)))
	result := QueueRebalanceResult{Moves: make([]QueueRebalanceMove, 0)}
	for _, job := range jobs {
		if ctx != nil && ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !rentalToOnPremCandidate(job, opts.JobScope, opts.MovingJobs) {
			continue
		}
		if len(opts.InstanceScope) > 0 {
			if job.LaunchID == nil {
				continue
			}
			if _, ok := opts.InstanceScope[*job.LaunchID]; !ok {
				continue
			}
		}
		sourceEst, ok := rentalQueuedCompletionEstimate(rentalPlan, job, runtimes)
		if !ok || sourceEst.Mean <= 0 {
			continue
		}
		constraints := placement.ConstraintsFromJob(job)
		predict := placement.BuildJobPredictorFromConfig(cfg, constraints)
		plan, err := placement.Evaluate(placement.EvaluateRequest{
			Constraints: constraints,
			Predictor:   predict,
			Sources:     []placement.CandidateSource{&placement.OnPremSource{Metrics: metrics}},
			Database:    database,
		})
		if err != nil || plan == nil || plan.Unplaced {
			continue
		}
		pick := plan.Fast
		if pick == nil {
			pick = plan.Cheap
		}
		if pick == nil || pick.Kind != placement.CandidateOnPrem || pick.OnPrem == nil {
			continue
		}
		if !rebalanceEstimateImproves(sourceEst, pick.EstTime) {
			continue
		}
		move := QueueRebalanceMove{
			JobID:          job.ID,
			FromInstanceID: *job.LaunchID,
			ToHost:         pick.OnPrem.Host,
			CostRatio:      0,
			MeanDelta:      sourceEst.Mean.Hours() - pick.EstTime.Mean.Hours(),
			LowerDelta:     sourceEst.Lower.Hours() - pick.EstTime.Lower.Hours(),
			UpperDelta:     sourceEst.Upper.Hours() - pick.EstTime.Upper.Hours(),
		}
		detail := fmt.Sprintf("rental-to-onprem rebalance: %s -> %s (rental %.0fm, on-prem %.0fm)",
			ids.FormatInstanceID(*job.LaunchID),
			pick.OnPrem.Host,
			sourceEst.Mean.Minutes(),
			pick.EstTime.Mean.Minutes())
		move.Reason = detail
		if opts.Apply {
			if _, err := MoveJobsToHost(database, []*db.Job{job}, pick.OnPrem.Host, JobMoveCallbacks{}); err != nil {
				return result, fmt.Errorf("move %s from rental to %s: %w", ids.FormatJobID(job.ID), pick.OnPrem.Host, err)
			}
			appendPlacementReason(database, job.ID, detail)
			op := strings.TrimSpace(opts.Operation)
			if op == "" {
				op = "rebalance_onprem"
			}
			oplog.LogJob(op, job.ID, pick.OnPrem.Host, oplog.WithDetail(detail))
		}
		result.Moves = append(result.Moves, move)
	}
	if opts.Apply && len(result.Moves) > 0 {
		op := strings.TrimSpace(opts.Operation)
		if op == "" {
			op = "rebalance_onprem"
		}
		oplog.Log(op, oplog.WithDetailf("moved=%d", len(result.Moves)))
	}
	return result, nil
}

func rentalToOnPremCandidate(job *db.Job, scoped map[int64]struct{}, movingJobs map[int64]struct{}) bool {
	if job == nil || !job.IsRentalJob() || job.LaunchID == nil || *job.LaunchID <= 0 {
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
	if job.CLIResourceOverrides != nil && strings.TrimSpace(job.CLIResourceOverrides.Host) != "" {
		return false
	}
	constraints := placement.ConstraintsFromJob(job)
	return !db.HasRentalTag(constraints.Tags) && constraints.Provider == ""
}

func rebalanceEstimateImproves(source, candidate estimate.Estimate) bool {
	if source.Mean <= 0 || candidate.Mean <= 0 {
		return false
	}
	return candidate.Mean < time.Duration(float64(source.Mean)*(1-rentalToOnPremImprovementEpsilon))
}

func rentalQueuedCompletionEstimate(plan *rebalancePlan, job *db.Job, runtimes runtimeBook) (estimate.Estimate, bool) {
	mean, ok := rentalQueuedCompletionHours(plan, job, runtimes, durationPointMean)
	if !ok {
		return estimate.Estimate{}, false
	}
	lower, _ := rentalQueuedCompletionHours(plan, job, runtimes, durationPointLower)
	upper, _ := rentalQueuedCompletionHours(plan, job, runtimes, durationPointUpper)
	if lower <= 0 {
		lower = mean
	}
	if upper <= 0 {
		upper = mean
	}
	return estimate.FromSeconds(mean*3600, lower*3600, upper*3600), true
}

func rentalQueuedCompletionHours(plan *rebalancePlan, job *db.Job, runtimes runtimeBook, point durationPoint) (float64, bool) {
	if plan == nil || job == nil {
		return 0, false
	}
	state, _, ok := plan.instanceForJob(job.ID)
	if !ok || state == nil {
		return 0, false
	}
	slots := instanceSlots(state.launch)
	slotTimes := make([]float64, slots)
	for idx, running := range state.runningJobs {
		remaining := predictionDurationHours(running, runtimes, point, true)
		if idx < len(slotTimes) {
			slotTimes[idx] = remaining
		} else {
			slotTimes = append(slotTimes, remaining)
		}
	}
	for _, queued := range state.queued {
		idx := firstFreeSlot(slotTimes)
		slotTimes[idx] += predictionDurationHours(queued, runtimes, point, false)
		if queued != nil && queued.ID == job.ID {
			return slotTimes[idx], true
		}
	}
	return 0, false
}
