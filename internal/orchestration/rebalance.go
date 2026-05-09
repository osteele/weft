package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/workdir"
)

const (
	defaultRebalanceCostCeiling = 1.10
)

var estimateRebalanceDurationsDetailed = estimate.EstimateJobDurationsDetailed

type QueueRebalanceOptions struct {
	Apply bool
	// Strategy selects the score profile: cheap, balanced, or fast.
	// Empty uses the first enabled candidate project's config, then "fast".
	Strategy string
	// ScoreEpsilon overrides the relative score improvement threshold when > 0.
	ScoreEpsilon float64
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
	// MovingJobs, if non-nil, is used in lieu of querying the move_intents
	// table. Callers that already loaded the open-intent set (e.g. the
	// autopilot pass) thread it through to avoid repeating the query.
	MovingJobs map[int64]struct{}
}

type QueueRebalanceMove struct {
	JobID           int64
	FromInstanceID  int64
	ToInstanceID    int64
	CostRatio       float64
	CostCeiling     float64
	MeanDelta       float64
	LowerDelta      float64
	UpperDelta      float64
	PriorityDelta   float64
	ThroughputDelta float64
	CostDelta       float64
	ProfileID       string
	Reason          string
}

type QueueRebalanceResult struct {
	Moves []QueueRebalanceMove
}

type rebalanceInstanceState struct {
	launch      *db.Launch
	capacity    campaign.InstanceCapacity
	running     int
	runningJobs []*db.Job
	queued      []*db.Job
	queuedIdx   map[int64]int
}

type rebalanceJobPolicy struct {
	enabled       bool
	costCeiling   float64
	budgetCeiling float64
	strategy      string
	epsilon       float64
	localDir      string
	image         string
}

type rebalancePlan struct {
	instances map[int64]*rebalanceInstanceState
}

type runtimeBook map[int64]estimate.DurationPrediction

type rebalanceObjective struct {
	PriorityCompletion float64
	TotalCompletion    float64
	TotalCost          float64
	Makespan           float64
	Score              float64
}

type durationPoint int

const (
	durationPointMean durationPoint = iota
	durationPointLower
	durationPointUpper
)

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
	plan := &rebalancePlan{instances: instanceState}

	candidates, err := listRebalanceCandidates(database, opts.JobScope)
	if err != nil {
		return QueueRebalanceResult{}, err
	}
	if len(candidates) == 0 {
		return QueueRebalanceResult{}, nil
	}
	movingJobs := opts.MovingJobs
	if movingJobs == nil {
		movingJobs, err = db.JobIDsWithOpenMoveIntents(database)
		if err != nil {
			return QueueRebalanceResult{}, err
		}
	}
	if len(movingJobs) > 0 {
		filtered := candidates[:0]
		for _, job := range candidates {
			if job == nil {
				continue
			}
			if _, m := movingJobs[job.ID]; m {
				continue
			}
			filtered = append(filtered, job)
		}
		candidates = filtered
		if len(candidates) == 0 {
			return QueueRebalanceResult{}, nil
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return queuedOrderLess(candidates[i], candidates[j])
	})

	policies := make(map[int64]rebalanceJobPolicy, len(candidates))
	selectedStrategy := strings.TrimSpace(opts.Strategy)
	scoreEpsilon := opts.ScoreEpsilon
	for _, job := range candidates {
		if job == nil {
			continue
		}
		policy := resolveRebalancePolicy(job, opts.CostCeilingOverride)
		policies[job.ID] = policy
		if selectedStrategy == "" && policy.enabled {
			selectedStrategy = policy.strategy
		}
		if scoreEpsilon <= 0 && policy.enabled {
			scoreEpsilon = policy.epsilon
		}
	}
	if selectedStrategy == "" {
		selectedStrategy = "fast"
	}
	if scoreEpsilon <= 0 {
		scoreEpsilon = 0.01
	}
	profile := bidding.ScoreProfileForRebalanceStrategy(selectedStrategy)
	runtimes := loadRuntimePredictions(database, plan.allJobs())
	baseMean, baseLower, baseUpper := plan.score(profile, runtimes)

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
		srcState, srcID, ok := plan.instanceForJob(job.ID)
		if !ok || srcID <= 0 {
			continue
		}
		if srcState == nil {
			continue
		}
		if len(opts.InstanceScope) > 0 {
			if _, scoped := opts.InstanceScope[srcID]; !scoped {
				continue
			}
		}
		if _, found := srcState.queuedIdx[job.ID]; !found {
			continue
		}

		policy := policies[job.ID]
		if !policy.enabled {
			continue
		}
		bestDst, ratio, bestMean, bestLower, bestUpper, bestObj, ok := pickBestRebalanceDestination(
			job, policy, srcID, plan, profile, runtimes, baseMean, scoreEpsilon, opts.InstanceScope,
		)
		if !ok || bestDst == nil {
			continue
		}
		meanDelta := bestMean - baseMean
		lowerDelta := bestLower - baseLower
		upperDelta := bestUpper - baseUpper
		baseObj := plan.objective(profile, runtimes, durationPointMean)

		move := QueueRebalanceMove{
			JobID:           job.ID,
			FromInstanceID:  srcID,
			ToInstanceID:    bestDst.launch.ID,
			CostRatio:       ratio,
			CostCeiling:     policy.budgetCeiling,
			MeanDelta:       meanDelta,
			LowerDelta:      lowerDelta,
			UpperDelta:      upperDelta,
			PriorityDelta:   bestObj.PriorityCompletion - baseObj.PriorityCompletion,
			ThroughputDelta: bestObj.TotalCompletion - baseObj.TotalCompletion,
			CostDelta:       bestObj.TotalCost - baseObj.TotalCost,
			ProfileID:       profile.ID,
			Reason:          describeRebalanceReason(srcState.launch, bestDst.launch, ratio, meanDelta, profile.ID),
		}
		logRebalanceDecision(move)

		if opts.Apply {
			if err := applyRebalanceMove(ctx, database, r2Client, move); err != nil {
				return result, err
			}
			appendPlacementReason(database, move.JobID, rebalancePlacementReason(move))
			op := strings.TrimSpace(opts.Operation)
			if op == "" {
				op = "rebalance"
			}
			oplog.Log(op, oplog.WithDetailf(
				"moved=1 src=%s dst=%s ratio=%.2f profile_id=%s priority_delta=%.6f throughput_delta=%.6f cost_delta=%.6f mean_delta=%.6f lower_delta=%.6f upper_delta=%.6f",
				ids.FormatInstanceID(move.FromInstanceID),
				ids.FormatInstanceID(move.ToInstanceID),
				move.CostRatio,
				move.ProfileID,
				move.PriorityDelta,
				move.ThroughputDelta,
				move.CostDelta,
				move.MeanDelta,
				move.LowerDelta,
				move.UpperDelta,
			))
		}

		result.Moves = append(result.Moves, move)
		alreadyMoved[job.ID] = struct{}{}
		plan = plan.withMove(job, srcID, move.ToInstanceID)
		baseMean, baseLower, baseUpper = bestMean, bestLower, bestUpper
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
			launch:      launch,
			capacity:    cap,
			running:     running,
			runningJobs: make([]*db.Job, 0, running),
			queued:      make([]*db.Job, 0),
			queuedIdx:   map[int64]int{},
		}
		for _, job := range jobs {
			if job == nil {
				continue
			}
			status := job.EffectiveStatus()
			switch status {
			case db.StatusRunning, db.StatusStarting:
				state.runningJobs = append(state.runningJobs, job)
			case db.StatusQueued, db.StatusPendingPlacement:
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
	return db.SchedulingLess(a, b)
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

func instanceSlots(launch *db.Launch) int {
	if launch == nil || launch.NumGPUs <= 0 {
		return 1
	}
	return launch.NumGPUs
}

func (p *rebalancePlan) allJobs() []*db.Job {
	if p == nil {
		return nil
	}
	seen := map[int64]struct{}{}
	out := make([]*db.Job, 0)
	for _, state := range p.instances {
		if state == nil {
			continue
		}
		for _, job := range state.runningJobs {
			if job == nil {
				continue
			}
			if _, ok := seen[job.ID]; ok {
				continue
			}
			seen[job.ID] = struct{}{}
			out = append(out, job)
		}
		for _, job := range state.queued {
			if job == nil {
				continue
			}
			if _, ok := seen[job.ID]; ok {
				continue
			}
			seen[job.ID] = struct{}{}
			out = append(out, job)
		}
	}
	return out
}

func (p *rebalancePlan) instanceForJob(jobID int64) (*rebalanceInstanceState, int64, bool) {
	if p == nil {
		return nil, 0, false
	}
	for id, state := range p.instances {
		if state == nil {
			continue
		}
		if _, ok := state.queuedIdx[jobID]; ok {
			return state, id, true
		}
	}
	return nil, 0, false
}

func (p *rebalancePlan) withMove(job *db.Job, srcID, dstID int64) *rebalancePlan {
	next := p.clone()
	if next == nil {
		return &rebalancePlan{instances: map[int64]*rebalanceInstanceState{}}
	}
	if src := next.instances[srcID]; src != nil {
		src.removeQueuedJob(job.ID)
	}
	if dst := next.instances[dstID]; dst != nil {
		dst.appendQueuedJob(job)
	}
	return next
}

func (p *rebalancePlan) clone() *rebalancePlan {
	if p == nil {
		return nil
	}
	out := &rebalancePlan{instances: make(map[int64]*rebalanceInstanceState, len(p.instances))}
	for id, state := range p.instances {
		out.instances[id] = state.clone()
	}
	return out
}

func (s *rebalanceInstanceState) clone() *rebalanceInstanceState {
	if s == nil {
		return nil
	}
	out := *s
	out.runningJobs = append([]*db.Job(nil), s.runningJobs...)
	out.queued = append([]*db.Job(nil), s.queued...)
	out.reindexQueued()
	return &out
}

func (p *rebalancePlan) score(profile bidding.ScoreProfile, runtimes runtimeBook) (mean, lower, upper float64) {
	return p.scorePoint(profile, runtimes, durationPointMean),
		p.scorePoint(profile, runtimes, durationPointLower),
		p.scorePoint(profile, runtimes, durationPointUpper)
}

func (p *rebalancePlan) scorePoint(profile bidding.ScoreProfile, runtimes runtimeBook, point durationPoint) float64 {
	weights := profile.Weights()
	totalCost, makespan := p.costAndMakespan(runtimes, point)
	return weights.Cost*totalCost + weights.Time*makespan
}

func (p *rebalancePlan) objective(profile bidding.ScoreProfile, runtimes runtimeBook, point durationPoint) rebalanceObjective {
	priorityCompletion, totalCompletion := p.completionSums(runtimes, point)
	totalCost, makespan := p.costAndMakespan(runtimes, point)
	return rebalanceObjective{
		PriorityCompletion: priorityCompletion,
		TotalCompletion:    totalCompletion,
		TotalCost:          totalCost,
		Makespan:           makespan,
		Score:              p.scorePoint(profile, runtimes, point),
	}
}

func (p *rebalancePlan) costAndMakespan(runtimes runtimeBook, point durationPoint) (totalCost, makespan float64) {
	if p == nil {
		return 0, 0
	}
	for _, state := range p.instances {
		if state == nil || state.launch == nil {
			continue
		}
		costPerHour := float64(state.launch.CostPerHourCents) / 100.0
		drain := instanceQueueDrainHours(state, runtimes, point)
		totalCost += costPerHour * drain
		if drain > makespan {
			makespan = drain
		}
	}
	return totalCost, makespan
}

func (p *rebalancePlan) completionSums(runtimes runtimeBook, point durationPoint) (priorityCompletion, totalCompletion float64) {
	if p == nil {
		return 0, 0
	}
	for _, state := range p.instances {
		p, total := instanceCompletionSums(state, runtimes, point)
		priorityCompletion += p
		totalCompletion += total
	}
	return priorityCompletion, totalCompletion
}

func instanceCompletionSums(state *rebalanceInstanceState, runtimes runtimeBook, point durationPoint) (priorityCompletion, totalCompletion float64) {
	if state == nil {
		return 0, 0
	}
	slots := instanceSlots(state.launch)
	slotTimes := make([]float64, slots)
	for idx, job := range state.runningJobs {
		remaining := predictionDurationHours(job, runtimes, point, true)
		if idx < len(slotTimes) {
			slotTimes[idx] = remaining
		} else {
			slotTimes = append(slotTimes, remaining)
		}
		totalCompletion += remaining
		if job != nil && job.Priority > 0 {
			priorityCompletion += remaining
		}
	}
	for _, job := range state.queued {
		idx := firstFreeSlot(slotTimes)
		slotTimes[idx] += predictionDurationHours(job, runtimes, point, false)
		completion := slotTimes[idx]
		totalCompletion += completion
		if job != nil && job.Priority > 0 {
			priorityCompletion += completion
		}
	}
	return priorityCompletion, totalCompletion
}

func instanceQueueDrainHours(state *rebalanceInstanceState, runtimes runtimeBook, point durationPoint) float64 {
	if state == nil {
		return 0
	}
	slots := instanceSlots(state.launch)
	slotTimes := make([]float64, slots)
	for idx, job := range state.runningJobs {
		remaining := predictionDurationHours(job, runtimes, point, true)
		if idx < len(slotTimes) {
			slotTimes[idx] = remaining
		} else {
			slotTimes = append(slotTimes, remaining)
		}
	}
	for _, job := range state.queued {
		idx := firstFreeSlot(slotTimes)
		slotTimes[idx] += predictionDurationHours(job, runtimes, point, false)
	}
	return maxFloat64(slotTimes)
}

func firstFreeSlot(slotTimes []float64) int {
	best := 0
	for idx := 1; idx < len(slotTimes); idx++ {
		if slotTimes[idx] < slotTimes[best] {
			best = idx
		}
	}
	return best
}

func maxFloat64(values []float64) float64 {
	out := 0.0
	for _, v := range values {
		if v > out {
			out = v
		}
	}
	return out
}

func predictionDurationHours(job *db.Job, runtimes runtimeBook, point durationPoint, running bool) float64 {
	d := predictionDuration(job, runtimes, point)
	if running {
		elapsed := runningElapsed(job)
		d -= elapsed
		if d < time.Minute {
			d = time.Minute
		}
	}
	if d <= 0 {
		d = estimate.DefaultJobDuration.Mean
	}
	return d.Hours()
}

func predictionDuration(job *db.Job, runtimes runtimeBook, point durationPoint) time.Duration {
	pred := estimate.DurationPrediction{Estimate: estimate.DefaultJobDuration}
	if job != nil && runtimes != nil {
		if found, ok := runtimes[job.ID]; ok {
			pred = found
		}
	}
	switch point {
	case durationPointLower:
		if pred.Estimate.Lower > 0 {
			return pred.Estimate.Lower
		}
	case durationPointUpper:
		if pred.Estimate.Upper > 0 {
			return pred.Estimate.Upper
		}
	}
	if pred.Estimate.Mean > 0 {
		return pred.Estimate.Mean
	}
	return estimate.DefaultJobDuration.Mean
}

func runningElapsed(job *db.Job) time.Duration {
	if job == nil || job.StartTime <= 0 {
		return 0
	}
	elapsed := time.Since(time.Unix(job.StartTime, 0))
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func loadRuntimePredictions(database *sql.DB, jobs []*db.Job) runtimeBook {
	out := make(runtimeBook, len(jobs))
	if len(jobs) == 0 {
		return out
	}
	cfg, err := config.Load()
	if err != nil {
		return out
	}
	predCfg := buildPredictorConfig(cfg)
	batch := make([]predictor.BatchJob, 0, len(jobs))
	seen := map[int64]struct{}{}
	for _, job := range jobs {
		if job == nil || job.ID == 0 {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		batch = append(batch, predictor.BatchJob{
			ID:         job.ID,
			Command:    job.Command,
			Host:       job.Host,
			Project:    job.Project,
			GPUClass:   job.GPUClass,
			WorkingDir: job.WorkingDir,
		})
	}
	predictions := estimateRebalanceDurationsDetailed(&predCfg, batch)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if predictions != nil {
			if pred, ok := predictions[job.ID]; ok {
				out[job.ID] = pred
				continue
			}
		}
		out[job.ID] = estimate.DurationPrediction{Estimate: estimate.DefaultJobDuration}
	}
	return out
}

func pickBestRebalanceDestination(
	job *db.Job,
	policy rebalanceJobPolicy,
	srcID int64,
	plan *rebalancePlan,
	profile bidding.ScoreProfile,
	runtimes runtimeBook,
	baseScore float64,
	epsilon float64,
	instanceScope map[int64]struct{},
) (*rebalanceInstanceState, float64, float64, float64, float64, rebalanceObjective, bool) {
	if job == nil {
		return nil, 0, 0, 0, 0, rebalanceObjective{}, false
	}
	src := plan.instances[srcID]
	if src == nil || src.launch == nil {
		return nil, 0, 0, 0, 0, rebalanceObjective{}, false
	}
	srcCost := src.launch.CostPerHourCents
	if srcCost < 0 {
		return nil, 0, 0, 0, 0, rebalanceObjective{}, false
	}

	var best *rebalanceInstanceState
	bestRatio := 0.0
	bestMean := baseScore
	bestLower := 0.0
	bestUpper := 0.0
	bestObj := rebalanceObjective{}
	baseObj := plan.objective(profile, runtimes, durationPointMean)
	for id, dst := range plan.instances {
		if id == srcID || dst == nil || dst.launch == nil {
			continue
		}
		if len(instanceScope) > 0 {
			if _, ok := instanceScope[id]; !ok {
				continue
			}
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
		candidate := plan.withMove(job, srcID, id)
		mean, lower, upper := candidate.score(profile, runtimes)
		candidateObj := candidate.objective(profile, runtimes, durationPointMean)
		scalarImproves := mean < baseScore*(1-epsilon)
		throughputImproves := rebalanceObjectiveImproves(baseObj, candidateObj, epsilon)
		if !throughputImproves && !scalarImproves {
			continue
		}
		if best == nil || rebalanceObjectiveLess(candidateObj, bestObj, epsilon) {
			best = dst
			bestRatio = ratio
			bestMean = mean
			bestLower = lower
			bestUpper = upper
			bestObj = candidateObj
			continue
		}
		if rebalanceObjectivesEquivalent(candidateObj, bestObj, epsilon) && mean < bestMean {
			best = dst
			bestRatio = ratio
			bestMean = mean
			bestLower = lower
			bestUpper = upper
			bestObj = candidateObj
			continue
		}
		if rebalanceObjectivesEquivalent(candidateObj, bestObj, epsilon) && nearlyEqual(mean, bestMean) &&
			dst.launch.CostPerHourCents < best.launch.CostPerHourCents {
			best = dst
			bestRatio = ratio
			bestMean = mean
			bestLower = lower
			bestUpper = upper
			bestObj = candidateObj
			continue
		}
		if rebalanceObjectivesEquivalent(candidateObj, bestObj, epsilon) && nearlyEqual(mean, bestMean) &&
			dst.launch.CostPerHourCents == best.launch.CostPerHourCents &&
			remainingRentalSeconds(dst.launch) > remainingRentalSeconds(best.launch) {
			best = dst
			bestRatio = ratio
			bestMean = mean
			bestLower = lower
			bestUpper = upper
			bestObj = candidateObj
		}
	}
	return best, bestRatio, bestMean, bestLower, bestUpper, bestObj, best != nil
}

func rebalanceObjectiveImproves(base, candidate rebalanceObjective, epsilon float64) bool {
	return candidate.PriorityCompletion < base.PriorityCompletion*(1-epsilon) ||
		(nearlyEqualWithEpsilon(candidate.PriorityCompletion, base.PriorityCompletion, epsilon) &&
			candidate.TotalCompletion < base.TotalCompletion*(1-epsilon)) ||
		(nearlyEqualWithEpsilon(candidate.PriorityCompletion, base.PriorityCompletion, epsilon) &&
			nearlyEqualWithEpsilon(candidate.TotalCompletion, base.TotalCompletion, epsilon) &&
			candidate.Score < base.Score*(1-epsilon))
}

func rebalanceObjectiveLess(a, b rebalanceObjective, epsilon float64) bool {
	if !nearlyEqualWithEpsilon(a.PriorityCompletion, b.PriorityCompletion, epsilon) {
		return a.PriorityCompletion < b.PriorityCompletion
	}
	if !nearlyEqualWithEpsilon(a.TotalCompletion, b.TotalCompletion, epsilon) {
		return a.TotalCompletion < b.TotalCompletion
	}
	if !nearlyEqualWithEpsilon(a.TotalCost, b.TotalCost, epsilon) {
		return a.TotalCost < b.TotalCost
	}
	if !nearlyEqualWithEpsilon(a.Score, b.Score, epsilon) {
		return a.Score < b.Score
	}
	return a.Makespan < b.Makespan
}

func rebalanceObjectivesEquivalent(a, b rebalanceObjective, epsilon float64) bool {
	return nearlyEqualWithEpsilon(a.PriorityCompletion, b.PriorityCompletion, epsilon) &&
		nearlyEqualWithEpsilon(a.TotalCompletion, b.TotalCompletion, epsilon) &&
		nearlyEqualWithEpsilon(a.TotalCost, b.TotalCost, epsilon) &&
		nearlyEqualWithEpsilon(a.Score, b.Score, epsilon)
}

func nearlyEqualWithEpsilon(a, b, epsilon float64) bool {
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= scale*math.Max(epsilon, 1e-9)
}

func resolveRebalancePolicy(job *db.Job, costCeilingOverride float64) rebalanceJobPolicy {
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	enabled := config.ProjectAutoPilotRebalanceEnabled(localDir)
	projectCeiling := config.ProjectAutoPilotRebalanceCostCeiling(localDir)
	ceiling := projectCeiling
	if costCeilingOverride > 0 {
		ceiling = costCeilingOverride
	}
	if ceiling <= 0 {
		ceiling = defaultRebalanceCostCeiling
	}
	if projectCeiling <= 0 {
		projectCeiling = defaultRebalanceCostCeiling
	}
	return rebalanceJobPolicy{
		enabled:       enabled,
		costCeiling:   ceiling,
		budgetCeiling: projectCeiling,
		strategy:      config.ProjectAutoPilotRebalanceStrategy(localDir),
		epsilon:       config.ProjectAutoPilotRebalanceScoreEpsilon(localDir),
		localDir:      localDir,
		image:         campaign.ResolveJobImage(localDir, job.Command),
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

func describeRebalanceReason(src, dst *db.Launch, ratio, meanDelta float64, profileID string) string {
	parts := []string{fmt.Sprintf("score %.3f", meanDelta)}
	if profileID != "" {
		parts = append(parts, "profile "+profileID)
	}
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

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-3
}

func logRebalanceDecision(move QueueRebalanceMove) {
	oplog.Log("rebalance.decision", oplog.WithDetailf(
		"job=%s src=%s dst=%s ratio=%.2f profile_id=%s priority_delta=%.6f throughput_delta=%.6f cost_delta=%.6f mean_delta=%.6f lower_delta=%.6f upper_delta=%.6f",
		ids.FormatJobID(move.JobID),
		ids.FormatInstanceID(move.FromInstanceID),
		ids.FormatInstanceID(move.ToInstanceID),
		move.CostRatio,
		move.ProfileID,
		move.PriorityDelta,
		move.ThroughputDelta,
		move.CostDelta,
		move.MeanDelta,
		move.LowerDelta,
		move.UpperDelta,
	))
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
	return ids.FormatJobID(id)
}
