package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/workdir"
)

const (
	defaultRebalanceCostCeiling = 1.10
)

type QueueRebalanceOptions struct {
	Apply bool
	// CostCeilingOverride applies to all candidates when > 0.
	CostCeilingOverride float64
	// Restrict source/destination instances when non-empty.
	InstanceScope map[int64]struct{}
	// Restrict candidate jobs when non-empty.
	JobScope map[int64]struct{}
	// Operation controls oplog op name for applied moves.
	Operation string
	// R2Client optionally reuses a prebuilt client for apply mode.
	R2Client *r2.Client
}

type QueueRebalanceMove struct {
	JobID          int64
	FromInstanceID int64
	ToInstanceID   int64
	CostRatio      float64
	Reason         string
}

type QueueRebalanceResult struct {
	Moves []QueueRebalanceMove
}

type rebalanceInstanceState struct {
	launch    *db.Launch
	capacity  campaign.InstanceCapacity
	running   int
	queued    []*db.Job
	queuedIdx map[int64]int
}

type rebalanceJobPolicy struct {
	enabled     bool
	costCeiling float64
	localDir    string
	image       string
}

func RebalanceQueuedJobsAcrossInstances(ctx context.Context, database *sql.DB, opts QueueRebalanceOptions) (QueueRebalanceResult, error) {
	if database == nil {
		return QueueRebalanceResult{}, nil
	}
	instanceState, err := buildRebalanceInstanceState(database)
	if err != nil {
		return QueueRebalanceResult{}, err
	}
	if len(instanceState) == 0 {
		return QueueRebalanceResult{}, nil
	}

	candidates, err := listRebalanceCandidates(database, opts.JobScope)
	if err != nil {
		return QueueRebalanceResult{}, err
	}
	if len(candidates) == 0 {
		return QueueRebalanceResult{}, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return queuedOrderLess(candidates[i], candidates[j])
	})

	policies := make(map[int64]rebalanceJobPolicy, len(candidates))
	for _, job := range candidates {
		if job == nil {
			continue
		}
		policies[job.ID] = resolveRebalancePolicy(job, opts.CostCeilingOverride)
	}

	jobLocation := make(map[int64]int64, len(candidates))
	for _, job := range candidates {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		jobLocation[job.ID] = *job.LaunchID
	}

	var r2Client *r2.Client
	if opts.Apply {
		r2Client = opts.R2Client
		if r2Client == nil {
			cfg, err := config.Load()
			if err != nil {
				return QueueRebalanceResult{}, fmt.Errorf("load config for rebalance: %w", err)
			}
			r2Client, err = BuildR2Client(cfg)
			if err != nil {
				return QueueRebalanceResult{}, fmt.Errorf("build R2 client for rebalance: %w", err)
			}
		}
	}

	result := QueueRebalanceResult{Moves: make([]QueueRebalanceMove, 0)}
	alreadyMoved := make(map[int64]struct{}, len(candidates))

	for _, job := range candidates {
		if job == nil {
			continue
		}
		if _, seen := alreadyMoved[job.ID]; seen {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
			continue
		}
		srcID, ok := jobLocation[job.ID]
		if !ok || srcID <= 0 {
			continue
		}
		srcState, ok := instanceState[srcID]
		if !ok || srcState == nil {
			continue
		}
		if len(opts.InstanceScope) > 0 {
			if _, scoped := opts.InstanceScope[srcID]; !scoped {
				continue
			}
		}
		srcIdx, found := srcState.queuedIdx[job.ID]
		if !found || srcIdx < 0 {
			continue
		}
		// Source has an idle slot for this job now; no move needed.
		if sourceHasIdleSlotForJob(srcState, srcIdx) {
			continue
		}

		policy := policies[job.ID]
		if !policy.enabled {
			continue
		}
		bestDst, ratio, ok := pickBestRebalanceDestination(job, policy, srcID, instanceState, opts.InstanceScope)
		if !ok || bestDst == nil {
			continue
		}

		move := QueueRebalanceMove{
			JobID:          job.ID,
			FromInstanceID: srcID,
			ToInstanceID:   bestDst.launch.ID,
			CostRatio:      ratio,
			Reason:         describeRebalanceReason(srcState.launch, bestDst.launch, ratio),
		}

		if opts.Apply {
			if err := applyRebalanceMove(ctx, database, r2Client, move); err != nil {
				return result, err
			}
			appendPlacementReason(database, move.JobID, rebalancePlacementReason(move))
			op := strings.TrimSpace(opts.Operation)
			if op == "" {
				op = "rebalance"
			}
			oplog.Log(op, oplog.WithDetailf("moved=1 src=%s dst=%s ratio=%.2f",
				ids.FormatInstanceID(move.FromInstanceID),
				ids.FormatInstanceID(move.ToInstanceID),
				move.CostRatio))
		}

		result.Moves = append(result.Moves, move)
		alreadyMoved[job.ID] = struct{}{}
		rebalanceStateAfterMove(srcState, bestDst, job.ID)
		jobLocation[job.ID] = move.ToInstanceID
	}

	if opts.Apply && len(result.Moves) > 0 {
		op := strings.TrimSpace(opts.Operation)
		if op == "" {
			op = "rebalance"
		}
		oplog.Log(op, oplog.WithDetailf("moved=%d", len(result.Moves)))
	}
	return result, nil
}

func buildRebalanceInstanceState(database *sql.DB) (map[int64]*rebalanceInstanceState, error) {
	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return nil, fmt.Errorf("list running launches: %w", err)
	}
	out := make(map[int64]*rebalanceInstanceState, len(launches))
	for _, launch := range launches {
		if launch == nil {
			continue
		}
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, launch.ID)
		if err != nil {
			continue
		}
		running := countActiveRunningJobs(jobs)
		cap, ok := campaign.NewInstanceCapacity(launch, running)
		if !ok {
			continue
		}
		state := &rebalanceInstanceState{
			launch:    launch,
			capacity:  cap,
			running:   running,
			queued:    make([]*db.Job, 0),
			queuedIdx: map[int64]int{},
		}
		for _, job := range jobs {
			if job == nil {
				continue
			}
			status := job.EffectiveStatus()
			if status == db.StatusQueued || status == db.StatusPendingPlacement {
				state.queued = append(state.queued, job)
			}
		}
		sort.Slice(state.queued, func(i, j int) bool {
			return queuedOrderLess(state.queued[i], state.queued[j])
		})
		for idx, job := range state.queued {
			state.queuedIdx[job.ID] = idx
		}
		out[launch.ID] = state
	}
	return out, nil
}

func listRebalanceCandidates(database *sql.DB, jobScope map[int64]struct{}) ([]*db.Job, error) {
	statuses := []string{db.StatusQueued, db.StatusPendingPlacement}
	jobs, err := db.ListJobsByStatuses(database, statuses, "", "", 0, nil, "")
	if err != nil {
		return nil, fmt.Errorf("list queued jobs for rebalance: %w", err)
	}
	out := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		if len(jobScope) > 0 {
			if _, ok := jobScope[job.ID]; !ok {
				continue
			}
		}
		out = append(out, job)
	}
	return out, nil
}

func queuedOrderLess(a, b *db.Job) bool {
	if a == nil || b == nil {
		return a != nil
	}
	aOrder := queuedOrderKey(a)
	bOrder := queuedOrderKey(b)
	if aOrder != bOrder {
		return aOrder < bOrder
	}
	return a.ID < b.ID
}

func queuedOrderKey(job *db.Job) int64 {
	if job == nil {
		return 0
	}
	if job.QueuedAt > 0 {
		return job.QueuedAt
	}
	if job.CreatedAt > 0 {
		return job.CreatedAt
	}
	return job.ID
}

func sourceHasIdleSlotForJob(src *rebalanceInstanceState, srcQueueIndex int) bool {
	if src == nil {
		return false
	}
	slots := instanceSlots(src.launch)
	if slots <= 0 {
		return false
	}
	return src.running+srcQueueIndex < slots
}

func destinationHasIdleSlot(dst *rebalanceInstanceState) bool {
	if dst == nil {
		return false
	}
	slots := instanceSlots(dst.launch)
	if slots <= 0 {
		return false
	}
	return dst.running+len(dst.queued) < slots
}

func instanceSlots(launch *db.Launch) int {
	if launch == nil || launch.NumGPUs <= 0 {
		return 1
	}
	return launch.NumGPUs
}

func pickBestRebalanceDestination(
	job *db.Job,
	policy rebalanceJobPolicy,
	srcID int64,
	states map[int64]*rebalanceInstanceState,
	instanceScope map[int64]struct{},
) (*rebalanceInstanceState, float64, bool) {
	if job == nil {
		return nil, 0, false
	}
	src := states[srcID]
	if src == nil || src.launch == nil {
		return nil, 0, false
	}
	srcCost := src.launch.CostPerHourCents
	if srcCost < 0 {
		return nil, 0, false
	}

	var best *rebalanceInstanceState
	bestRatio := 0.0
	for id, dst := range states {
		if id == srcID || dst == nil || dst.launch == nil {
			continue
		}
		if len(instanceScope) > 0 {
			if _, ok := instanceScope[id]; !ok {
				continue
			}
		}
		if !destinationHasIdleSlot(dst) {
			continue
		}
		ok, _ := campaign.MatchJobToInstance(job, dst.capacity)
		if !ok {
			continue
		}
		if !campaign.ImagesCompatible(policy.image, strings.TrimSpace(dst.launch.DockerImage)) {
			continue
		}
		ratio, ratioOK := costRatioWithinCeiling(srcCost, dst.launch.CostPerHourCents, policy.costCeiling)
		if !ratioOK {
			continue
		}
		if best == nil {
			best = dst
			bestRatio = ratio
			continue
		}
		if dst.launch.CostPerHourCents < best.launch.CostPerHourCents {
			best = dst
			bestRatio = ratio
			continue
		}
		if dst.launch.CostPerHourCents == best.launch.CostPerHourCents &&
			remainingRentalSeconds(dst.launch) > remainingRentalSeconds(best.launch) {
			best = dst
			bestRatio = ratio
		}
	}
	return best, bestRatio, best != nil
}

func resolveRebalancePolicy(job *db.Job, costCeilingOverride float64) rebalanceJobPolicy {
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	enabled := config.ProjectAutoPilotRebalanceEnabled(localDir)
	ceiling := config.ProjectAutoPilotRebalanceCostCeiling(localDir)
	if costCeilingOverride > 0 {
		ceiling = costCeilingOverride
	}
	if ceiling <= 0 {
		ceiling = defaultRebalanceCostCeiling
	}
	return rebalanceJobPolicy{
		enabled:     enabled,
		costCeiling: ceiling,
		localDir:    localDir,
		image:       campaign.ResolveJobImage(localDir, job.Command),
	}
}

func costRatioWithinCeiling(srcCostCents, dstCostCents int, ceiling float64) (float64, bool) {
	if srcCostCents < 0 || dstCostCents < 0 {
		return 0, false
	}
	if ceiling <= 0 {
		ceiling = defaultRebalanceCostCeiling
	}
	if srcCostCents == 0 {
		if dstCostCents == 0 {
			return 1.0, true
		}
		return math.Inf(1), false
	}
	ratio := float64(dstCostCents) / float64(srcCostCents)
	return ratio, ratio <= ceiling
}

func remainingRentalSeconds(launch *db.Launch) int64 {
	if launch == nil || launch.MaxTimeSeconds <= 0 {
		return 0
	}
	base := int64(0)
	switch {
	case launch.ProviderRunningAt != nil && *launch.ProviderRunningAt > 0:
		base = *launch.ProviderRunningAt
	case launch.LaunchedAt != nil && *launch.LaunchedAt > 0:
		base = *launch.LaunchedAt
	default:
		return 0
	}
	endAt := base + int64(launch.MaxTimeSeconds)
	remain := endAt - time.Now().Unix()
	if remain < 0 {
		return 0
	}
	return remain
}

func rebalanceStateAfterMove(src, dst *rebalanceInstanceState, jobID int64) {
	if src != nil {
		src.removeQueuedJob(jobID)
	}
	if dst != nil {
		dst.appendQueuedJob(&db.Job{ID: jobID})
	}
}

func (s *rebalanceInstanceState) removeQueuedJob(jobID int64) {
	if s == nil || len(s.queued) == 0 {
		return
	}
	idx, ok := s.queuedIdx[jobID]
	if !ok || idx < 0 || idx >= len(s.queued) {
		return
	}
	s.queued = append(s.queued[:idx], s.queued[idx+1:]...)
	s.reindexQueued()
}

func (s *rebalanceInstanceState) appendQueuedJob(job *db.Job) {
	if s == nil || job == nil {
		return
	}
	s.queued = append(s.queued, job)
	s.reindexQueued()
}

func (s *rebalanceInstanceState) reindexQueued() {
	if s == nil {
		return
	}
	s.queuedIdx = make(map[int64]int, len(s.queued))
	for idx, job := range s.queued {
		if job != nil {
			s.queuedIdx[job.ID] = idx
		}
	}
}

func applyRebalanceMove(ctx context.Context, database *sql.DB, r2Client *r2.Client, move QueueRebalanceMove) error {
	job, err := db.GetJobByID(database, move.JobID)
	if err != nil {
		return fmt.Errorf("load rebalance job %s: %w", formatRebalanceJobID(move.JobID), err)
	}
	if job == nil {
		return fmt.Errorf("load rebalance job %s: not found", formatRebalanceJobID(move.JobID))
	}
	status := job.EffectiveStatus()
	if status != db.StatusQueued && status != db.StatusPendingPlacement {
		return nil
	}

	if _, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
		return fmt.Errorf("unplace job %s for rebalance: %w", formatRebalanceJobID(move.JobID), err)
	}
	refreshed, err := db.GetJobByID(database, move.JobID)
	if err != nil {
		return fmt.Errorf("reload rebalance job %s: %w", formatRebalanceJobID(move.JobID), err)
	}
	if refreshed == nil {
		return fmt.Errorf("reload rebalance job %s: not found", formatRebalanceJobID(move.JobID))
	}
	if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, move.ToInstanceID, []*db.Job{refreshed}); err != nil {
		return fmt.Errorf("submit rebalance job %s to %s: %w",
			formatRebalanceJobID(move.JobID), ids.FormatInstanceID(move.ToInstanceID), err)
	}
	return nil
}

func appendPlacementReason(database *sql.DB, jobID int64, reason string) {
	if database == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		return
	}
	reasons := append([]string(nil), job.PlacementReasons...)
	reasons = append(reasons, reason)
	_ = db.SetJobPlacementReasons(database, jobID, reasons)
}

func rebalancePlacementReason(move QueueRebalanceMove) string {
	return fmt.Sprintf(
		"rebalanced from %s → %s (cost ratio %.2f)",
		ids.FormatInstanceID(move.FromInstanceID),
		ids.FormatInstanceID(move.ToInstanceID),
		move.CostRatio,
	)
}

func describeRebalanceReason(src, dst *db.Launch, ratio float64) string {
	parts := []string{"dst idle"}
	if src != nil && dst != nil && strings.EqualFold(strings.TrimSpace(src.GPUClass), strings.TrimSpace(dst.GPUClass)) {
		parts = append(parts, "same class")
	} else {
		parts = append(parts, "class upgrade within cap")
	}
	switch {
	case ratio < 0.995:
		parts = append(parts, "cheaper")
	case ratio > 1.005:
		parts = append(parts, "cost increase within cap")
	default:
		parts = append(parts, "same $")
	}
	return strings.Join(parts, ", ")
}

func countActiveRunningJobs(jobs []*db.Job) int {
	count := 0
	for _, j := range jobs {
		if j == nil {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			count++
		}
	}
	return count
}

func formatRebalanceJobID(id int64) string {
	return fmt.Sprintf("wj%d", id)
}
