package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
)

type groupedAutoPilotPassTestOptions struct {
	realFillReusableInstances bool
	placeInventory            func(*sql.DB, *config.Config, []*db.Job) ([]*db.Job, int)
}

func blockedRelaunchResult(scope []int64, reason string) *campaign.RelaunchResult {
	reasons := make(map[int64]string, len(scope))
	for _, jobID := range scope {
		reasons[jobID] = reason
	}
	return &campaign.RelaunchResult{JobReasons: reasons}
}

func TestUniqueAutoPilotOfferSnapshotGroupsMatchesPlannerDerivedDiskKeys(t *testing.T) {
	groups := []campaign.InstanceGroup{{
		GPUClass:  "A40",
		GPUMemGB:  48,
		DiskGB:    141,
		JobDiskGB: map[int64]int{1: 70, 2: 72},
		Jobs: []*db.Job{
			{ID: 1, GPUClass: "A40", GPUMemGB: testIntPtr(48)},
			{ID: 2, GPUClass: "A40", GPUMemGB: testIntPtr(48)},
		},
	}}
	minReliability := 0.95
	prefetch := uniqueAutoPilotOfferSnapshotGroups(nil, groups, minReliability)
	planner := append([]campaign.InstanceGroup{}, groups...)
	planner = append(planner, campaign.MergeCompatibleGroupsWithDisk(groups, campaign.GroupDiskEstimator(nil))...)
	planner = append(planner, campaign.SplitToParallel(groups)...)

	prefetchKeys := groupOfferKeySet(prefetch, minReliability)
	plannerKeys := groupOfferKeySet(planner, minReliability)
	for key := range plannerKeys {
		if _, ok := prefetchKeys[key]; !ok {
			t.Fatalf("prefetch keys missing planner key %q; prefetch=%v planner=%v", key, prefetchKeys, plannerKeys)
		}
	}
}

func groupOfferKeySet(groups []campaign.InstanceGroup, minReliability float64) map[string]struct{} {
	out := map[string]struct{}{}
	for _, group := range groups {
		key := campaign.GroupRawOfferCacheKey(group, minReliability)
		if strings.TrimSpace(key) != "" {
			out[key] = struct{}{}
		}
	}
	return out
}

func testIntPtr(n int) *int { return &n }

func writeSparseTestFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse file %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close sparse file %s: %v", path, err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("truncate sparse file %s: %v", path, err)
	}
}

func TestGatedAutopilotClaimsSlotBeforeFetchingOffers(t *testing.T) {
	database := db.SetupTestDB(t)
	mem := 24
	jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
		WorkingDir: t.TempDir(),
		Command:    "python train.py",
		GPUClass:   "nvidia",
		GPUMemGB:   &mem,
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}

	originalClients := autoPilotBuildCloudClients
	originalBuildPlan := autoPilotBuildPlanWithOptions
	t.Cleanup(func() {
		autoPilotBuildCloudClients = originalClients
		autoPilotBuildPlanWithOptions = originalBuildPlan
		autoPilotOfferSnapshotCache.Lock()
		autoPilotOfferSnapshotCache.entries = make(map[string]autoPilotOfferSnapshotEntry)
		autoPilotOfferSnapshotCache.Unlock()
	})

	searchStarted := make(chan struct{})
	releaseSearch := make(chan struct{})
	autoPilotBuildCloudClients = func(*config.Config) ([]cloud.Client, error) {
		return []cloud.Client{&cloud.MockClient{
			ProviderVal: cloud.ProviderVastai,
			SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
				close(searchStarted)
				<-releaseSearch
				return []cloud.Offer{{
					Provider:    cloud.ProviderVastai,
					ProviderID:  "offer-1",
					GPUName:     "RTX 4090",
					GPUMemGB:    24,
					CostPerHour: 1,
				}}, nil
			},
		}}, nil
	}
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity, options campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		if !options.CachedOffersOnly {
			t.Fatal("gated pass did not force cached-only offer planning")
		}
		return campaign.AutoPlacementPlan{BlockedReasons: map[int64]string{jobID: "planner: test blocked"}}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := RunGroupedAutoPilotPassGated(context.Background(), database, nil, "slow-prefetch")
		done <- err
	}()

	select {
	case <-searchStarted:
	case <-time.After(time.Second):
		t.Fatal("SearchOffers did not start")
	}

	second := NewAutopilotRunner(database, "second-acquirer")
	if err := second.TryAcquire(); !errors.Is(err, ErrAutopilotBusy) {
		t.Fatalf("second TryAcquire while offer fetch is blocked = %v, want ErrAutopilotBusy", err)
	}
	close(releaseSearch)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gated pass error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gated pass did not finish after offer search released")
	}
}

type groupedAutoPilotPassTestOption func(*groupedAutoPilotPassTestOptions)

func withRealFillReusableInstances(opts *groupedAutoPilotPassTestOptions) {
	opts.realFillReusableInstances = true
}

func withPlaceInventory(fn func(*sql.DB, *config.Config, []*db.Job) ([]*db.Job, int)) groupedAutoPilotPassTestOption {
	return func(opts *groupedAutoPilotPassTestOptions) {
		opts.placeInventory = fn
	}
}

func runGroupedAutoPilotPassForTest(t *testing.T, ctx context.Context, database *sql.DB, scopedJobs []*db.Job, optionFns ...groupedAutoPilotPassTestOption) (*GroupedAutoPilotResult, error) {
	t.Helper()
	opts := groupedAutoPilotPassTestOptions{}
	for _, optionFn := range optionFns {
		if optionFn != nil {
			optionFn(&opts)
		}
	}
	origDrain := autoPilotDrainOverloadedInventoryHosts
	origOnPremRebalance := autoPilotRebalanceQueuedRentalJobsToOnPrem
	origInstanceRebalance := autoPilotRebalanceQueuedJobsAcrossInstances
	origFillReusable := autoPilotFillReusableInstances
	origCreditHealth := autoPilotCheckProviderCreditHealth
	origPlaceComputeIntensive := autoPilotPlaceComputeIntensive
	origPlaceInventory := autoPilotPlaceInventory
	t.Cleanup(func() {
		autoPilotDrainOverloadedInventoryHosts = origDrain
		autoPilotRebalanceQueuedRentalJobsToOnPrem = origOnPremRebalance
		autoPilotRebalanceQueuedJobsAcrossInstances = origInstanceRebalance
		autoPilotFillReusableInstances = origFillReusable
		autoPilotCheckProviderCreditHealth = origCreditHealth
		autoPilotPlaceComputeIntensive = origPlaceComputeIntensive
		autoPilotPlaceInventory = origPlaceInventory
	})
	autoPilotDrainOverloadedInventoryHosts = func(context.Context, *sql.DB, *config.Config, map[int64]struct{}, map[int64]struct{}) (int, error) {
		return 0, nil
	}
	autoPilotRebalanceQueuedRentalJobsToOnPrem = func(context.Context, *sql.DB, *config.Config, map[int64]struct{}, map[int64]struct{}) (int, error) {
		return 0, nil
	}
	autoPilotRebalanceQueuedJobsAcrossInstances = func(context.Context, *sql.DB, QueueRebalanceOptions) (QueueRebalanceResult, error) {
		return QueueRebalanceResult{}, nil
	}
	if !opts.realFillReusableInstances {
		autoPilotFillReusableInstances = func(context.Context, *sql.DB, *r2.Client, map[int64]struct{}, map[int64]struct{}, map[int64]string, map[int64]string, map[int64][]blockreason.ReuseRejection) (int, error) {
			return 0, nil
		}
	}
	autoPilotCheckProviderCreditHealth = func(*config.Config) []ProviderCreditStatus {
		return nil
	}
	autoPilotPlaceComputeIntensive = func(_ *sql.DB, _ *config.Config, jobs []*db.Job) ([]*db.Job, int) {
		return jobs, 0
	}
	// Inventory placement does live SSH metric collection; neutralize it by
	// default so inventory tests assert the "waiting" reason without probing
	// real hosts. Tests exercising successful assignment inject their own.
	if opts.placeInventory != nil {
		autoPilotPlaceInventory = opts.placeInventory
	} else {
		autoPilotPlaceInventory = func(_ *sql.DB, _ *config.Config, jobs []*db.Job) ([]*db.Job, int) {
			return jobs, 0
		}
	}
	return RunGroupedAutoPilotPass(ctx, database, scopedJobs)
}

func TestAutoReplanStuckInventoryJobsMovesSustainedDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", t.TempDir(), "python train.py", "blocked job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Now()
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	for _, at := range []time.Time{now.Add(-20 * time.Minute), now.Add(-12 * time.Minute)} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchDeferred,
			JobID:      jobID,
			Detail:     "source sync deferred (host unreachable): ssh timeout",
		}); err != nil {
			t.Fatalf("InsertLifecycleEvent: %v", err)
		}
	}

	originalUnplace := autoReplanUnplaceQueuedJob
	t.Cleanup(func() { autoReplanUnplaceQueuedJob = originalUnplace })
	autoReplanUnplaceQueuedJob = func(database *sql.DB, job *db.Job, _ ops.ExecuteOptions) (ops.Result, error) {
		if err := db.MoveQueuedJobToUnplaced(database, job.ID); err != nil {
			return ops.Result{}, err
		}
		return ops.Result{Success: true, JobID: job.ID}, nil
	}

	count, err := autoReplanStuckInventoryJobs(database, nil, nil)
	if err != nil {
		t.Fatalf("autoReplanStuckInventoryJobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.TargetKind() != db.JobTargetUnplaced {
		t.Fatalf("TargetKind = %s, want unplaced", job.TargetKind())
	}
	var eventCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?`,
		jobID, db.EventQueueDispatchAutoReplanned).Scan(&eventCount); err != nil {
		t.Fatalf("count lifecycle events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("auto-replan events = %d, want 1", eventCount)
	}
}

// TestRunGroupedAutoPilotPass_AutoReplanDisabledByDefault asserts that the
// AutoReplanStuckInventoryDispatch rule does NOT fire when the config flag is
// off (its default). The rule converts an inventory job into an unplaced
// (rental-eligible) job after 10 min of dispatch failures; that's a placement
// change without explicit user consent, which surprised users in practice
// (jobs pinned to studio/cool100 silently jumped to cloud after the inventory
// host had a transient SSH/HF issue). The gate lives in autopilot.go's
// RunGroupedAutoPilotPass; this test verifies it's wired correctly.
func TestRunGroupedAutoPilotPass_AutoReplanDisabledByDefault(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", t.TempDir(), "python train.py", "blocked job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Now()
	// Synthesize sustained dispatch failures > 10 min — the same condition
	// the existing TestAutoReplanStuckInventoryJobsMovesSustainedDispatchBlock
	// test uses to trigger a replan when calling the inner function directly.
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	for _, at := range []time.Time{now.Add(-20 * time.Minute), now.Add(-12 * time.Minute)} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchDeferred,
			JobID:      jobID,
			Detail:     "source sync deferred (host unreachable): ssh timeout",
		}); err != nil {
			t.Fatalf("InsertLifecycleEvent: %v", err)
		}
	}

	// Disable autopilot launch path so we isolate the replan gate.
	origBuild := autoPilotBuildPlan
	origRelaunch := autoPilotRelaunch
	origReplanGate := autoReplanConfigEnabled
	origUnplace := autoReplanUnplaceQueuedJob
	t.Cleanup(func() {
		autoPilotBuildPlan = origBuild
		autoPilotRelaunch = origRelaunch
		autoReplanConfigEnabled = origReplanGate
		autoReplanUnplaceQueuedJob = origUnplace
	})
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}
	unplaceCalls := 0
	autoReplanUnplaceQueuedJob = func(database *sql.DB, job *db.Job, _ ops.ExecuteOptions) (ops.Result, error) {
		unplaceCalls++
		return ops.Result{Success: true, JobID: job.ID}, nil
	}

	// Default — gate returns false.
	autoReplanConfigEnabled = func() bool { return false }
	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass (disabled): %v", err)
	}
	if unplaceCalls != 0 {
		t.Fatalf("disabled gate: unplaceCalls = %d, want 0 (job should stay pinned to cool30)", unplaceCalls)
	}
	if result.AutoReplanned != 0 {
		t.Fatalf("disabled gate: AutoReplanned = %d, want 0", result.AutoReplanned)
	}
	job, _ := db.GetJobByID(database, jobID)
	if job.TargetKind() != db.JobTargetInventoryHost {
		t.Fatalf("disabled gate: TargetKind = %s, want inventory_host", job.TargetKind())
	}

	// Opt-in — gate returns true, replan fires.
	autoReplanConfigEnabled = func() bool { return true }
	autoReplanUnplaceQueuedJob = func(database *sql.DB, job *db.Job, _ ops.ExecuteOptions) (ops.Result, error) {
		unplaceCalls++
		if err := db.MoveQueuedJobToUnplaced(database, job.ID); err != nil {
			return ops.Result{}, err
		}
		return ops.Result{Success: true, JobID: job.ID}, nil
	}
	if _, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil); err != nil {
		t.Fatalf("RunGroupedAutoPilotPass (enabled): %v", err)
	}
	if unplaceCalls != 1 {
		t.Fatalf("enabled gate: unplaceCalls = %d, want 1", unplaceCalls)
	}
}

func TestAutoPublishMissingCheckpointsFlagOnUnderThreshold(t *testing.T) {
	database := db.SetupTestDB(t)
	job := recordQueuedCheckpointJob(t, database, "trace", 1024)
	resetAutoPublishCheckpointAttempts(t)

	origPublish := autoPilotPublishCheckpointToR2
	t.Cleanup(func() { autoPilotPublishCheckpointToR2 = origPublish })
	calls := 0
	autoPilotPublishCheckpointToR2 = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ cloud.R2Config, asset dataloc.DataAsset) (campaign.CheckpointPublishResult, error) {
		calls++
		if asset.Ref() != "checkpoint:trace" {
			t.Fatalf("asset = %s", asset.Ref())
		}
		return campaign.CheckpointPublishResult{Uploaded: true}, nil
	}

	cfg := autoPublishCheckpointTestConfig(true, 1)
	published, reasons, err := autoPublishMissingCheckpoints(context.Background(), database, cfg, &r2.Client{}, []*db.Job{job})
	if err != nil {
		t.Fatalf("autoPublishMissingCheckpoints: %v", err)
	}
	if published != 1 || calls != 1 {
		t.Fatalf("published=%d calls=%d, want 1/1", published, calls)
	}
	if reason := reasons[job.ID]; reason != "" {
		t.Fatalf("reason = %q, want empty", reason)
	}
}

func TestAutoPublishMissingCheckpointsRequiresFlagAndThreshold(t *testing.T) {
	database := db.SetupTestDB(t)
	job := recordQueuedCheckpointJob(t, database, "trace", 2*1024*1024*1024)
	resetAutoPublishCheckpointAttempts(t)

	origPublish := autoPilotPublishCheckpointToR2
	t.Cleanup(func() { autoPilotPublishCheckpointToR2 = origPublish })
	calls := 0
	autoPilotPublishCheckpointToR2 = func(context.Context, *sql.DB, *r2.Client, cloud.R2Config, dataloc.DataAsset) (campaign.CheckpointPublishResult, error) {
		calls++
		return campaign.CheckpointPublishResult{Uploaded: true}, nil
	}

	disabled := autoPublishCheckpointTestConfig(false, 1)
	published, _, err := autoPublishMissingCheckpoints(context.Background(), database, disabled, &r2.Client{}, []*db.Job{job})
	if err != nil {
		t.Fatalf("disabled autoPublishMissingCheckpoints: %v", err)
	}
	if published != 0 || calls != 0 {
		t.Fatalf("disabled published=%d calls=%d, want 0/0", published, calls)
	}

	enabledSmallCap := autoPublishCheckpointTestConfig(true, 1)
	published, reasons, err := autoPublishMissingCheckpoints(context.Background(), database, enabledSmallCap, &r2.Client{}, []*db.Job{job})
	if err != nil {
		t.Fatalf("threshold autoPublishMissingCheckpoints: %v", err)
	}
	if published != 0 || calls != 0 {
		t.Fatalf("over cap published=%d calls=%d, want 0/0", published, calls)
	}
	if reason := reasons[job.ID]; !strings.Contains(reason, "above autopilot cap") {
		t.Fatalf("reason = %q, want cap reason", reason)
	}
}

func TestAutoPublishMissingCheckpointsConfirmedFailureDoesNotAbortPass(t *testing.T) {
	database := db.SetupTestDB(t)
	job := recordQueuedCheckpointJob(t, database, "trace", 1024)
	resetAutoPublishCheckpointAttempts(t)

	origPublish := autoPilotPublishCheckpointToR2
	t.Cleanup(func() { autoPilotPublishCheckpointToR2 = origPublish })
	autoPilotPublishCheckpointToR2 = func(context.Context, *sql.DB, *r2.Client, cloud.R2Config, dataloc.DataAsset) (campaign.CheckpointPublishResult, error) {
		return campaign.CheckpointPublishResult{}, fmt.Errorf("R2 PutObject: 500 internal error")
	}

	cfg := autoPublishCheckpointTestConfig(true, 1)
	published, reasons, err := autoPublishMissingCheckpoints(context.Background(), database, cfg, &r2.Client{}, []*db.Job{job})
	// A confirmed (non-deferred) upload failure must be recorded per-job, not
	// returned as a pass-aborting error.
	if err != nil {
		t.Fatalf("confirmed publish failure aborted the pass: %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if reason := reasons[job.ID]; !strings.Contains(reason, "auto-publish failed") {
		t.Fatalf("reason = %q, want auto-publish failed", reason)
	}
}

func TestAutoPublishMissingCheckpointsUnknownDefersWithoutFailingJob(t *testing.T) {
	database := db.SetupTestDB(t)
	job := recordQueuedCheckpointJob(t, database, "trace", 1024)
	resetAutoPublishCheckpointAttempts(t)

	origPublish := autoPilotPublishCheckpointToR2
	t.Cleanup(func() { autoPilotPublishCheckpointToR2 = origPublish })
	autoPilotPublishCheckpointToR2 = func(context.Context, *sql.DB, *r2.Client, cloud.R2Config, dataloc.DataAsset) (campaign.CheckpointPublishResult, error) {
		return campaign.CheckpointPublishResult{}, fmt.Errorf("%w: confirm checkpoint checkpoint:trace on cool30: ssh timeout", campaign.ErrCheckpointPublishDeferred)
	}

	cfg := autoPublishCheckpointTestConfig(true, 1)
	published, reasons, err := autoPublishMissingCheckpoints(context.Background(), database, cfg, &r2.Client{}, []*db.Job{job})
	if err != nil {
		t.Fatalf("unknown should defer, not return pass error: %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if reason := reasons[job.ID]; !strings.Contains(reason, "checkpoint auto-publish deferred") || !strings.Contains(reason, "ssh timeout") {
		t.Fatalf("reason = %q, want deferred ssh timeout", reason)
	}
	refreshed, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if refreshed.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("status = %s, want queued", refreshed.EffectiveStatus())
	}
}

func TestRunGroupedAutoPilotPassGatedPausedDoesNotAutoPublish(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.PauseAutopilot(database, "test", "manual"); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}

	origPublish := autoPilotPublishCheckpointToR2
	t.Cleanup(func() { autoPilotPublishCheckpointToR2 = origPublish })
	autoPilotPublishCheckpointToR2 = func(context.Context, *sql.DB, *r2.Client, cloud.R2Config, dataloc.DataAsset) (campaign.CheckpointPublishResult, error) {
		t.Fatal("auto-publish should not run while autopilot is paused")
		return campaign.CheckpointPublishResult{}, nil
	}

	_, err := RunGroupedAutoPilotPassGated(context.Background(), database, nil, "paused-test")
	if !errors.Is(err, ErrAutopilotPaused) {
		t.Fatalf("err = %v, want ErrAutopilotPaused", err)
	}
}

func TestRemoveCheckpointDeferredJobsFromPlan(t *testing.T) {
	blockedJob := &db.Job{ID: 10}
	keepJob := &db.Job{ID: 11}
	plan := campaign.AutoPlacementPlan{
		LaunchJobIDs: []int64{10, 11},
		LaunchGroups: []campaign.LaunchGroup{{
			JobIDs:           []int64{10, 11},
			CostPerHourCents: 75,
		}, {
			JobIDs:           []int64{10},
			CostPerHourCents: 25,
		}},
		LaunchRateCentsPerHour: 100,
		ReuseAssignments: []campaign.ReuseAssignment{
			{Job: blockedJob},
			{Job: keepJob},
		},
	}

	removeCheckpointDeferredJobsFromPlan(&plan, map[int64]string{10: "checkpoint auto-publish deferred: ssh timeout"})

	if len(plan.LaunchJobIDs) != 1 || plan.LaunchJobIDs[0] != 11 {
		t.Fatalf("LaunchJobIDs = %v, want [11]", plan.LaunchJobIDs)
	}
	if len(plan.LaunchGroups) != 1 || len(plan.LaunchGroups[0].JobIDs) != 1 || plan.LaunchGroups[0].JobIDs[0] != 11 {
		t.Fatalf("LaunchGroups = %+v, want only job 11", plan.LaunchGroups)
	}
	if plan.LaunchRateCentsPerHour != 75 {
		t.Fatalf("LaunchRateCentsPerHour = %d, want 75", plan.LaunchRateCentsPerHour)
	}
	if len(plan.ReuseAssignments) != 1 || plan.ReuseAssignments[0].Job.ID != 11 {
		t.Fatalf("ReuseAssignments = %+v, want only job 11", plan.ReuseAssignments)
	}
}

func recordQueuedCheckpointJob(t *testing.T, database *sql.DB, checkpointID string, sizeBytes int64) *db.Job {
	t.Helper()
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "checkpoint job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobInputs(database, jobID, []string{"checkpoint:" + checkpointID}); err != nil {
		t.Fatalf("SetJobInputs: %v", err)
	}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:        "cool30",
		Asset:       dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: checkpointID},
		Path:        "/remote/checkpoints/" + checkpointID,
		SizeBytes:   sizeBytes,
		ContentHash: strings.Repeat("d", 64),
		ContentType: dataloc.ContentTypeDirectory,
		LastSeen:    time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	return job
}

func autoPublishCheckpointTestConfig(enabled bool, maxGB float64) *config.Config {
	return &config.Config{
		Autopilot: config.AutopilotConfig{
			AutoPublishCheckpoints:     enabled,
			AutoPublishCheckpointMaxGB: maxGB,
		},
		Vastai: config.VastaiConfig{
			R2: config.R2Config{
				AccountID:       "account",
				AccessKeyID:     "key",
				SecretAccessKey: "secret",
				Bucket:          "bucket",
			},
		},
	}
}

func resetAutoPublishCheckpointAttempts(t *testing.T) {
	t.Helper()
	autoPublishCheckpointAttempts.Lock()
	old := autoPublishCheckpointAttempts.last
	autoPublishCheckpointAttempts.last = make(map[string]time.Time)
	autoPublishCheckpointAttempts.Unlock()
	t.Cleanup(func() {
		autoPublishCheckpointAttempts.Lock()
		autoPublishCheckpointAttempts.last = old
		autoPublishCheckpointAttempts.Unlock()
	})
}

func TestRunGroupedAutoPilotPass_DoesNotRelaunchPlannerBlockedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "blocked job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   nil,
			BlockedReasons: map[int64]string{jobID: "planner: capacity unavailable"},
		}, nil
	}

	relaunchCalls := 0
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchCalls++
		return &campaign.RelaunchResult{}, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if relaunchCalls != 0 {
		t.Fatalf("relaunch calls = %d, want 0", relaunchCalls)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Launched != 0 {
		t.Fatalf("Launched = %d, want 0", result.Launched)
	}
	if got := result.BlockedReasons[jobID]; got != "planner: capacity unavailable" {
		t.Fatalf("blocked reason = %q, want %q", got, "planner: capacity unavailable")
	}
}

func TestRunGroupedAutoPilotPass_InventoryTaggedJobsBypassCloudPlanner(t *testing.T) {
	database := db.SetupTestDB(t)

	invID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "inventory job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU inventory: %v", err)
	}
	if err := db.SetJobTags(database, invID, []string{db.TagInventory}); err != nil {
		t.Fatalf("SetJobTags inventory: %v", err)
	}
	cloudID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "cloud job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU cloud: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
	})

	var plannerSawJobs []int64
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, jobs []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		plannerSawJobs = nil
		for _, j := range jobs {
			plannerSawJobs = append(plannerSawJobs, j.ID)
		}
		// Planner blocks the cloud job so the pass returns cleanly without
		// trying to relaunch.
		return campaign.AutoPlacementPlan{
			BlockedReasons: map[int64]string{cloudID: "planner: capacity unavailable"},
		}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatal("autopilot should not invoke relaunch when only inventory jobs are awaiting")
		return &campaign.RelaunchResult{}, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	for _, id := range plannerSawJobs {
		if id == invID {
			t.Fatalf("planner saw inventory-tagged job %d; cloud planner must not see inventory jobs", invID)
		}
	}
	if got := result.BlockedReasons[invID]; got != inventoryAwaitingReason {
		t.Fatalf("inventory job blocked reason = %q, want %q", got, inventoryAwaitingReason)
	}
	if got := result.BlockedReasons[cloudID]; got != "planner: capacity unavailable" {
		t.Fatalf("cloud job blocked reason = %q, want %q", got, "planner: capacity unavailable")
	}

	job, err := db.GetJobByID(database, invID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if len(job.PlacementReasons) == 0 || job.PlacementReasons[len(job.PlacementReasons)-1] != inventoryAwaitingReason {
		t.Fatalf("inventory placement_reasons did not end with %q: %v", inventoryAwaitingReason, job.PlacementReasons)
	}
}

// TestRunGroupedAutoPilotPass_AssignsInventoryJobOnPrem is the regression test
// for the daemon assigning inventory jobs on-prem itself. Previously the
// autopilot pass only marked inventory jobs "waiting for on-prem host" and
// relied on the interactive hostsync worker to assign them, so they sat
// unplaced whenever no TUI was open. Now an inventory job an on-prem host can
// take is assigned by the pass, counted in Placed, and carries no awaiting
// reason.
func TestRunGroupedAutoPilotPass_AssignsInventoryJobOnPrem(t *testing.T) {
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)

	invID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "inventory job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU inventory: %v", err)
	}
	if err := db.SetJobTags(database, invID, []string{db.TagInventory}); err != nil {
		t.Fatalf("SetJobTags inventory: %v", err)
	}

	// Inject a placement that assigns the job on-prem, standing in for the
	// real on-prem scorer (which does live SSH metric collection).
	placeInventory := func(database *sql.DB, _ *config.Config, jobs []*db.Job) ([]*db.Job, int) {
		placed := 0
		remaining := make([]*db.Job, 0, len(jobs))
		for _, j := range jobs {
			if j == nil {
				continue
			}
			assigned, assignErr := db.AssignJobHost(database, j.ID, "host-alpha")
			if assignErr != nil || !assigned {
				remaining = append(remaining, j)
				continue
			}
			placed++
		}
		return remaining, placed
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil, withPlaceInventory(placeInventory))
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Placed != 1 {
		t.Fatalf("Placed = %d, want 1", result.Placed)
	}
	if reason, blocked := result.BlockedReasons[invID]; blocked {
		t.Fatalf("assigned inventory job should carry no blocked reason, got %q", reason)
	}

	job, err := db.GetJobByID(database, invID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "host-alpha" {
		t.Fatalf("inventory job host = %q, want host-alpha", job.Host)
	}
	for _, r := range job.PlacementReasons {
		if r == inventoryAwaitingReason {
			t.Fatalf("assigned inventory job should not carry %q: %v", inventoryAwaitingReason, job.PlacementReasons)
		}
	}
}

func TestRunGroupedAutoPilotPass_ErrorPathStillCarriesInventoryReasons(t *testing.T) {
	database := db.SetupTestDB(t)

	invID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "inventory job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU inventory: %v", err)
	}
	if err := db.SetJobTags(database, invID, []string{db.TagInventory}); err != nil {
		t.Fatalf("SetJobTags inventory: %v", err)
	}
	if _, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "cloud job", "A100"); err != nil {
		t.Fatalf("RecordQueuedWithGPU cloud: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	t.Cleanup(func() { autoPilotBuildPlan = originalBuildPlan })

	wantErr := errors.New("planner exploded")
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{}, wantErr
	}

	result, runErr := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if !errors.Is(runErr, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", runErr, wantErr)
	}
	if result == nil {
		t.Fatal("result is nil; error paths after inventory partition must return a non-nil result so callers see inventory reasons")
	}
	if got := result.BlockedReasons[invID]; got != inventoryAwaitingReason {
		t.Fatalf("inventory job blocked reason = %q, want %q", got, inventoryAwaitingReason)
	}
}

func TestRunGroupedAutoPilotPass_SkipsComputeIntensiveReuseBelowCPUFloor(t *testing.T) {
	// Hermetic inventory so this test doesn't depend on the developer's
	// ~/.config/weft/hosts/*.yaml. Without this, when a developer has
	// real on-prem hosts visible (no opt_in_only), placeComputeIntensiveOnPremBeforeRental
	// short-circuits the auto-planner mocks under test.
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "cpu-heavy", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{db.TagComputeIntensive}); err != nil {
		t.Fatalf("SetJobTags: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "A100",
		GPUMemGB:         80,
		ResolvedGPUName:  "A100",
		CPUCores:         8,
		CostPerHourCents: 100,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	originalPlace := autoPilotPlaceComputeIntensive
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
		autoPilotPlaceComputeIntensive = originalPlace
	})
	// Stub the on-prem pre-pass so the test's compute-intensive job
	// reaches the auto-planner mock under test rather than being
	// placed on a hermetic test host (host-alpha has 64 cores and
	// would pass the CPU floor).
	autoPilotPlaceComputeIntensive = func(_ *sql.DB, _ *config.Config, jobs []*db.Job) ([]*db.Job, int) {
		return jobs, 0
	}

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			ReuseAssignments: []campaign.ReuseAssignment{{
				Job: job,
				Instance: campaign.InstanceCapacity{
					Instance:   inst,
					DiskFreeGB: 100,
				},
			}},
			BlockedReasons: map[int64]string{},
		}, nil
	}
	submitCalls := 0
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ int64, _ []*db.Job) error {
		submitCalls++
		return nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if submitCalls != 0 {
		t.Fatalf("submit calls = %d, want 0", submitCalls)
	}
	if result.BlockedReasons[jobID] == "" || !strings.Contains(result.BlockedReasons[jobID], "CPU cores insufficient") {
		t.Fatalf("blocked reason = %q, want CPU floor reason", result.BlockedReasons[jobID])
	}
}

func TestRunGroupedAutoPilotPass_FallbackRelaunchesWhenPlannerReturnsNoDecisions(t *testing.T) {
	database := db.SetupTestDB(t)

	jobA, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_a.py", "job a", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobA: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_b.py", "job b", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobB: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   nil,
			BlockedReasons: map[int64]string{},
		}, nil
	}

	relaunchCalls := 0
	var gotScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchCalls++
		gotScope = append([]int64(nil), scope...)
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if relaunchCalls != 1 {
		t.Fatalf("relaunch calls = %d, want 1", relaunchCalls)
	}

	got := map[int64]bool{}
	for _, id := range gotScope {
		got[id] = true
	}
	if len(got) != 2 || !got[jobA] || !got[jobB] {
		t.Fatalf("relaunch scope = %v, want both jobs [%d %d]", gotScope, jobA, jobB)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
}

func TestSelectLaunchGroupsWithinHeadroom_PicksBestFitByJobsPerCost(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1, 2}, CostPerHourCents: 50},
		{JobIDs: []int64{3}, CostPerHourCents: 99},
		{JobIDs: []int64{4}, CostPerHourCents: 110},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if len(accepted) != 1 {
		t.Fatalf("accepted groups = %d, want 1", len(accepted))
	}
	if accepted[0].CostPerHourCents != 50 {
		t.Fatalf("accepted group cost = %d, want 50", accepted[0].CostPerHourCents)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected groups = %d, want 2", len(rejected))
	}
	if used != 50 {
		t.Fatalf("used = %d, want 50", used)
	}
}

func TestSelectLaunchGroupsWithinHeadroom_NoneFit(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1}, CostPerHourCents: 150},
		{JobIDs: []int64{2}, CostPerHourCents: 200},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if len(accepted) != 0 {
		t.Fatalf("accepted groups = %d, want 0", len(accepted))
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected groups = %d, want 2", len(rejected))
	}
	if used != 0 {
		t.Fatalf("used = %d, want 0", used)
	}
}

func TestSelectLaunchGroupsWithinHeadroom_PrefersPriorityGroup(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1}, CostPerHourCents: 100, Priority: 0},
		{JobIDs: []int64{2}, CostPerHourCents: 100, Priority: 1},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if used != 100 {
		t.Fatalf("used = %d, want 100", used)
	}
	if len(accepted) != 1 || accepted[0].JobIDs[0] != 2 {
		t.Fatalf("accepted = %+v, want priority job 2", accepted)
	}
	if len(rejected) != 1 || rejected[0].JobIDs[0] != 1 {
		t.Fatalf("rejected = %+v, want normal job 1", rejected)
	}
}

func TestSelectLaunchGroupsWithinHeadroom_ExemptsRateCappedGroup(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1}, CostPerHourCents: 3320, RateCapAuthorized: true},
		{JobIDs: []int64{2}, CostPerHourCents: 80},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if len(accepted) != 2 {
		t.Fatalf("accepted = %+v, want both groups", accepted)
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %+v, want none", rejected)
	}
	if used != 80 {
		t.Fatalf("used headroom = %d, want 80 from uncapped group only", used)
	}

	accepted, rejected, used = selectLaunchGroupsWithinHeadroom(groups[:1], 0)
	if len(accepted) != 1 || len(rejected) != 0 || used != 0 {
		t.Fatalf("zero-headroom result = accepted %+v rejected %+v used %d", accepted, rejected, used)
	}
}

func TestApplyAcceptedLaunchGroups_UpdatesLegacyLaunchFields(t *testing.T) {
	plan := campaign.AutoPlacementPlan{
		LaunchJobIDs: []int64{1, 2, 3},
		BlockedReasons: map[int64]string{
			1: "old reason",
			2: "old reason",
		},
	}
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{3, 1}, CostPerHourCents: 70},
	}

	applyAcceptedLaunchGroups(&plan, groups, plan.BlockedReasons)
	if plan.LaunchRateCentsPerHour != 70 {
		t.Fatalf("launch rate = %d, want 70", plan.LaunchRateCentsPerHour)
	}
	if len(plan.LaunchJobIDs) != 2 || plan.LaunchJobIDs[0] != 1 || plan.LaunchJobIDs[1] != 3 {
		t.Fatalf("launch job ids = %v, want [1 3]", plan.LaunchJobIDs)
	}
	if _, exists := plan.BlockedReasons[1]; exists {
		t.Fatalf("blocked reason for accepted job 1 should be cleared, got %q", plan.BlockedReasons[1])
	}
}

func TestNoRentalHeadroomReason_IncludesMatchDiagnostic(t *testing.T) {
	mem := 80
	job := &db.Job{GPUClass: "A100", GPUMemGB: &mem}
	caps := []campaign.InstanceCapacity{{
		Instance: &db.Launch{
			GPUClass: "A100",
			GPUMemGB: 40,
		},
	}}

	reason := noRentalHeadroomReason(job, caps, nil)
	if !strings.Contains(reason, "no rental headroom; running instances couldn't accept this job") {
		t.Fatalf("reason = %q, want base message", reason)
	}
	if !strings.Contains(reason, "GPU memory insufficient") {
		t.Fatalf("reason = %q, want MatchJobToInstance diagnostic", reason)
	}
}

func TestNoRentalHeadroomStructured_BuildsLaunchAndReuseBreakdown(t *testing.T) {
	mem := 80
	job := &db.Job{GPUClass: "A100", GPUMemGB: &mem}
	caps := []campaign.InstanceCapacity{
		{Instance: &db.Launch{ID: 11, GPUClass: "A100", GPUMemGB: 40}},
		{Instance: &db.Launch{ID: 12, GPUClass: "A100", GPUMemGB: 24}},
	}
	s := placementFailureStructured(job, caps, nil, noRentalHeadroomLaunchReason)
	if !s.IsPlacementFailure() {
		t.Fatalf("expected a placement failure, got %+v", s)
	}
	if s.Launch == "" {
		t.Fatalf("expected a launch blocker, got empty")
	}
	// Every rejecting instance is listed — not just the first.
	if len(s.Reuse) != 2 {
		t.Fatalf("expected 2 reuse rejections, got %d: %+v", len(s.Reuse), s.Reuse)
	}
	for _, r := range s.Reuse {
		if r.Instance == "" {
			t.Fatalf("reuse rejection missing instance label: %+v", r)
		}
		if !strings.Contains(r.Reason, "GPU memory insufficient") {
			t.Fatalf("reuse rejection reason = %q, want GPU memory diagnostic", r.Reason)
		}
	}
	if s.Flat() != noRentalHeadroomReason(job, caps, nil) {
		t.Fatalf("Flat() = %q, want parity with noRentalHeadroomReason", s.Flat())
	}
}

func TestRunGroupedAutoPilotPass_NoSubsetFitsTriggersPreferReuseRetry(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "A100",
		ResolvedGPUName:  "A100",
		GPUMemGB:         80,
		CostPerHourCents: 100,
		DiskGB:           200,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups:           []campaign.LaunchGroup{{JobIDs: []int64{jobID}, CostPerHourCents: 150}},
			LaunchJobIDs:           []int64{jobID},
			LaunchRateCentsPerHour: 150,
			BlockedReasons:         map[int64]string{},
		}, nil
	}

	retryCalled := false
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity, options campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		retryCalled = true
		if !options.PreferReuse {
			t.Fatalf("retry options PreferReuse = false, want true")
		}
		return campaign.AutoPlacementPlan{
			BlockedReasons: map[int64]string{},
		}, nil
	}

	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatalf("autoPilotRelaunch should not be called when the run-rate gate rejects the only launch")
		return nil, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if !retryCalled {
		t.Fatalf("expected PreferReuse retry call")
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if got := result.BlockedReasons[jobID]; strings.Contains(got, "no subset fits") || !strings.Contains(got, "requested $1.50/hr") || !strings.Contains(got, "job needs $1.50/hr") {
		t.Fatalf("blocked reason = %q, want single-job requested amount without no-subset marker", got)
	}
}

func TestRunGroupedAutoPilotPass_RunRateAllFitLeavesLaunchScope(t *testing.T) {
	database := db.SetupTestDB(t)
	jobA, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_a.py", "job a", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobA: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_b.py", "job b", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobB: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 10.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups: []campaign.LaunchGroup{
				{JobIDs: []int64{jobA}, CostPerHourCents: 120},
				{JobIDs: []int64{jobB}, CostPerHourCents: 180},
			},
			LaunchJobIDs:           []int64{jobA, jobB},
			LaunchRateCentsPerHour: 300,
			BlockedReasons:         map[int64]string{},
		}, nil
	}

	var gotScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		gotScope = append([]int64(nil), scope...)
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	got := map[int64]bool{}
	for _, id := range gotScope {
		got[id] = true
	}
	if len(got) != 2 || !got[jobA] || !got[jobB] {
		t.Fatalf("relaunch scope = %v, want both jobs [%d %d]", gotScope, jobA, jobB)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
}

func TestRunGroupedAutoPilotPass_RunRateNoneFitBlocksAll(t *testing.T) {
	database := db.SetupTestDB(t)
	jobA, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_a.py", "job a", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobA: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_b.py", "job b", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobB: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups: []campaign.LaunchGroup{
				{JobIDs: []int64{jobA}, CostPerHourCents: 120},
				{JobIDs: []int64{jobB}, CostPerHourCents: 150},
			},
			LaunchJobIDs:           []int64{jobA, jobB},
			LaunchRateCentsPerHour: 270,
			BlockedReasons:         map[int64]string{},
		}, nil
	}
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity, _ campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{BlockedReasons: map[int64]string{}}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatalf("autoPilotRelaunch should not run when no subset fits")
		return nil, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if got := result.BlockedReasons[jobA]; !strings.Contains(got, "no subset fits") {
		t.Fatalf("blocked reason A = %q, want no-subset-fits marker", got)
	}
	if got := result.BlockedReasons[jobB]; !strings.Contains(got, "no subset fits") {
		t.Fatalf("blocked reason B = %q, want no-subset-fits marker", got)
	}
}

func TestRunGroupedAutoPilotPass_RunRateNoneFitPersistsOverStaleReason(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	staleReason := "job wj2980 declares input \"checkpoint:regmatrix\", which weft cannot provision onto a cloud rental instance"
	appendPlacementReason(database, jobID, staleReason)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups:           []campaign.LaunchGroup{{JobIDs: []int64{jobID}, CostPerHourCents: 150}},
			LaunchJobIDs:           []int64{jobID},
			LaunchRateCentsPerHour: 150,
			BlockedReasons:         map[int64]string{},
		}, nil
	}
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity, _ campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{BlockedReasons: map[int64]string{}}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatalf("autoPilotRelaunch should not run when the run-rate gate rejects the only launch")
		return nil, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if got := result.BlockedReasons[jobID]; strings.Contains(got, "no subset fits") || !strings.Contains(got, "requested $1.50/hr") || !strings.Contains(got, "job needs $1.50/hr") {
		t.Fatalf("blocked reason = %q, want single-job requested amount without no-subset marker", got)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if len(job.PlacementReasons) < 2 {
		t.Fatalf("placement reasons = %#v, want stale reason plus persisted run-rate reason", job.PlacementReasons)
	}
	latest := job.PlacementReasons[len(job.PlacementReasons)-1]
	if !strings.Contains(latest, "run-rate target exceeded") || !strings.Contains(latest, "requested $1.50/hr") || strings.Contains(latest, "no subset fits") {
		t.Fatalf("latest placement reason = %q, want persisted single-job run-rate reason", latest)
	}
}

func TestRunGroupedAutoPilotPass_ReuseFallbackExecutesAssignmentsWithoutLaunch(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "A100",
		ResolvedGPUName:  "A100",
		GPUMemGB:         80,
		CostPerHourCents: 100,
		DiskGB:           200,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups:           []campaign.LaunchGroup{{JobIDs: []int64{jobID}, CostPerHourCents: 150}},
			LaunchJobIDs:           []int64{jobID},
			LaunchRateCentsPerHour: 150,
			BlockedReasons:         map[int64]string{},
		}, nil
	}

	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, jobs []*db.Job, capacities []campaign.InstanceCapacity, options campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		if !options.PreferReuse {
			t.Fatalf("retry options PreferReuse = false, want true")
		}
		if len(jobs) == 0 || len(capacities) == 0 || capacities[0].Instance == nil {
			t.Fatalf("expected jobs/capacities in retry")
		}
		return campaign.AutoPlacementPlan{
			ReuseAssignments: []campaign.ReuseAssignment{{
				Job:      jobs[0],
				Instance: capacities[0],
			}},
			BlockedReasons: map[int64]string{},
		}, nil
	}

	submitCalls := 0
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, instanceID int64, jobs []*db.Job) error {
		submitCalls++
		if len(jobs) != 1 || jobs[0] == nil || jobs[0].ID != jobID {
			t.Fatalf("submitted jobs = %#v, want [%d]", jobs, jobID)
		}
		if instanceID == 0 {
			t.Fatalf("instanceID = 0, want running instance id")
		}
		if err := db.SetJobLaunchID(database, jobs[0].ID, instanceID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}
		return nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		if len(scope) != 0 {
			t.Fatalf("expected no launch scope after reuse fallback, got %v", scope)
		}
		return &campaign.RelaunchResult{}, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if submitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1", submitCalls)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Placed != 1 {
		t.Fatalf("Placed = %d, want 1", result.Placed)
	}
	if result.Launched != 0 {
		t.Fatalf("Launched = %d, want 0", result.Launched)
	}
}

func TestMergeRelaunchReasonsIntoBlockedReasons_PrefersPerJobOverPerInstance(t *testing.T) {
	// Three sibling jobs (PFT, ASIDE, AIR eval) all came from the same prior
	// failed cloud instance, so they share one failedInstanceID. Each has its
	// own waiting-on-producer reason; recordNotReplacedReason aggregated those
	// under the single instance key as "multiple reasons (...)". The per-job
	// JobReasons map carries the accurate per-job reason. The merge must
	// surface each job's own reason rather than the aggregated one.
	const failedInstanceID int64 = 9001
	pftReason := `waiting for "output/exp_053_pft_d256/ise_masked_lossweighted.pt" from wj1602 (queued)`
	asideReason := `waiting for "output/exp_053_aside/ise_masked_lossweighted.pt" from wj1603 (queued)`
	airReason := `waiting for "output/exp_053_air/ise_masked_lossweighted.pt" from wj1604 (queued)`
	combined := "multiple reasons (" + pftReason + "; " + asideReason + "; " + airReason + ")"

	result := &campaign.RelaunchResult{
		NotReplacedReasons: map[int64]string{failedInstanceID: combined},
		JobReasons: map[int64]string{
			1605: pftReason,
			1606: asideReason,
			1607: airReason,
		},
	}
	failedInstanceByJob := map[int64]int64{
		1605: failedInstanceID,
		1606: failedInstanceID,
		1607: failedInstanceID,
	}

	blockedReasons := map[int64]string{}
	mergeRelaunchReasonsIntoBlockedReasons(blockedReasons, result, failedInstanceByJob)

	for jobID, want := range map[int64]string{
		1605: pftReason,
		1606: asideReason,
		1607: airReason,
	} {
		got := blockedReasons[jobID]
		if got != want {
			t.Errorf("blockedReasons[%d] = %q, want %q", jobID, got, want)
		}
		if strings.HasPrefix(got, "multiple reasons (") {
			t.Errorf("blockedReasons[%d] should not be the aggregated string, got %q", jobID, got)
		}
	}
}

func TestMergeRelaunchReasonsIntoBlockedReasons_FallsBackToPerInstance(t *testing.T) {
	// When a job has no per-job entry but its predecessor instance does, the
	// per-instance reason fills in as a fallback.
	result := &campaign.RelaunchResult{
		NotReplacedReasons: map[int64]string{42: "no offers available"},
		JobReasons:         map[int64]string{},
	}
	failedInstanceByJob := map[int64]int64{100: 42}

	blockedReasons := map[int64]string{}
	mergeRelaunchReasonsIntoBlockedReasons(blockedReasons, result, failedInstanceByJob)

	if got, want := blockedReasons[100], "no offers available"; got != want {
		t.Errorf("blockedReasons[100] = %q, want %q", got, want)
	}
}

func TestRunGroupedAutoPilotPass_ExcludesJobsWithOpenMoveIntent(t *testing.T) {
	// Regression: when the user has manually moved a job, an open
	// MoveIntent prevents the autopilot from racing the move flow and
	// reusing some other existing instance for the same job. See
	// specs/job-move.allium § AutopilotIgnoresMovingJobs.
	database := db.SetupTestDB(t)

	moving, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "moving", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU moving: %v", err)
	}
	other, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python b.py", "other", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU other: %v", err)
	}

	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID: moving, TargetKind: db.MoveTargetExisting, TargetLaunchID: &target,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	var seenUnplaced []int64
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, unplaced []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		for _, j := range unplaced {
			if j != nil {
				seenUnplaced = append(seenUnplaced, j.ID)
			}
		}
		return campaign.AutoPlacementPlan{}, nil
	}

	var relaunchScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchScope = append([]int64(nil), scope...)
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}

	if _, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil); err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}

	for _, id := range seenUnplaced {
		if id == moving {
			t.Errorf("planner saw moving job %d in unplaced set", moving)
		}
	}
	if len(seenUnplaced) == 0 || seenUnplaced[0] != other {
		t.Errorf("planner saw %v, expected only [%d]", seenUnplaced, other)
	}
	for _, id := range relaunchScope {
		if id == moving {
			t.Errorf("relaunch scope included moving job %d", moving)
		}
	}
}

func TestRunGroupedAutoPilotPass_RetriesOpenMoveIntentAfterNoStartLaunchFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "moving", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	source, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, source); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetNew,
		TargetLaunchID: &target,
		AttemptCount:   1,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := db.CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, db.StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, target, db.LaunchStatusFailed, db.TerminationReasonInfraFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus target: %v", err)
	}
	transition, err := db.HandleMoveTargetFailedBeforeStart(database, jobID, target, db.AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("HandleMoveTargetFailedBeforeStart: %v", err)
	}
	if !transition.Handled || !transition.Retryable || transition.Exhausted {
		t.Fatalf("transition = %+v, want handled retryable not exhausted", transition)
	}

	originalRetry := autoPilotLaunchMoveIntentRetry
	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotLaunchMoveIntentRetry = originalRetry
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})
	var retryCalls int
	var replacementID int64
	autoPilotLaunchMoveIntentRetry = func(_ context.Context, database *sql.DB, got *db.MoveIntent) (int, error) {
		retryCalls++
		if got.ID != intent.ID {
			t.Fatalf("retry intent id = %d, want %d", got.ID, intent.ID)
		}
		var err error
		replacementID, err = db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching, Provider: "vastai"})
		if err != nil {
			return 0, err
		}
		if err := db.AdvanceMoveIntentTargetLaunch(database, got.ID, replacementID); err != nil {
			return 0, err
		}
		return 1, nil
	}
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if retryCalls != 1 {
		t.Fatalf("retry calls = %d, want 1", retryCalls)
	}
	if result == nil || result.Launched != 1 {
		t.Fatalf("result.Launched = %v, want 1", result)
	}
	got, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != db.MoveIntentStateOpen {
		t.Fatalf("intent state = %q, want open", got.State)
	}
	if got.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2", got.AttemptCount)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != replacementID {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, replacementID)
	}
}

func TestMoveIntentRetryAction_ReadyTerminalTargetWithoutStartRetries(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "moving", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	source, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, source); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetNew,
		TargetLaunchID: &target,
		AttemptCount:   1,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := db.CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, db.StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := db.SetLaunchAgentReadyAtIfUnset(database, target, time.Now()); err != nil {
		t.Fatalf("SetLaunchAgentReadyAtIfUnset: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, target, db.LaunchStatusFailed, db.TerminationReasonInfraFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus target: %v", err)
	}

	action, err := moveIntentRetryAction(database, intent)
	if err != nil {
		t.Fatalf("moveIntentRetryAction: %v", err)
	}
	if action != moveIntentActionRetry {
		t.Fatalf("action = %v, want retry", action)
	}
	got, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != db.MoveIntentStateOpen {
		t.Fatalf("intent state = %s, want open", got.State)
	}
}

func TestFulfillOpenMoveToNewIntents_PrunesExhaustedTerminalTargetBeforeRetry(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "stale move", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetNew,
		TargetLaunchID: &target,
		AttemptCount:   4,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
		t.Fatalf("age intent: %v", err)
	}

	originalRetry := autoPilotLaunchMoveIntentRetry
	t.Cleanup(func() { autoPilotLaunchMoveIntentRetry = originalRetry })
	autoPilotLaunchMoveIntentRetry = func(context.Context, *sql.DB, *db.MoveIntent) (int, error) {
		t.Fatal("stale terminal target should be pruned before retry")
		return 0, nil
	}

	launched, err := fulfillOpenMoveToNewIntents(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("fulfillOpenMoveToNewIntents: %v", err)
	}
	if launched != 0 {
		t.Fatalf("launched = %d, want 0", launched)
	}
	got, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != db.MoveIntentStateCanceled || got.Resolution != db.MoveIntentResolutionStale {
		t.Fatalf("intent = (%s, %q), want canceled stale", got.State, got.Resolution)
	}
}

func TestRunGroupedAutoPilotPass_ReusesExistingInstanceWhenLaunchBlocked(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "reuse fallback", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUClass: "A100",
		GPUMemGB: 80,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	instance, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   []int64{jobID},
			BlockedReasons: map[int64]string{},
		}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		if len(scope) != 1 || scope[0] != jobID {
			t.Fatalf("relaunch scope = %v, want [%d]", scope, jobID)
		}
		return &campaign.RelaunchResult{BlockedReason: "paused: repeated launch failures without progress"}, nil
	}
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, jobs []*db.Job, _ []campaign.InstanceCapacity, options campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		if !options.PreferReuse {
			t.Fatal("fallback planner did not set PreferReuse")
		}
		if len(jobs) != 1 || jobs[0].ID != jobID {
			t.Fatalf("fallback jobs = %v, want job %d", jobs, jobID)
		}
		return campaign.AutoPlacementPlan{
			ReuseAssignments: []campaign.ReuseAssignment{{
				Job: jobs[0],
				Instance: campaign.InstanceCapacity{
					Instance: instance,
				},
			}},
			BlockedReasons: map[int64]string{},
		}, nil
	}
	submitted := false
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, gotInstanceID int64, jobs []*db.Job) error {
		submitted = true
		if gotInstanceID != instanceID {
			t.Fatalf("submit instance = %d, want %d", gotInstanceID, instanceID)
		}
		if len(jobs) != 1 || jobs[0].ID != jobID {
			t.Fatalf("submitted jobs = %v, want job %d", jobs, jobID)
		}
		if err := db.SetJobLaunchID(database, jobs[0].ID, gotInstanceID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}
		return nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if !submitted {
		t.Fatal("expected job to be submitted to reusable instance")
	}
	if result.Placed != 1 {
		t.Fatalf("Placed = %d, want 1", result.Placed)
	}
	if reason := result.BlockedReasons[jobID]; reason != "" {
		t.Fatalf("blocked reason for reused job = %q, want empty", reason)
	}
}

func TestRunGroupedAutoPilotPass_FillsReusableInstanceAfterPlanner(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "fill reusable", "H100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "H100",
		ResolvedGPUName:  "H100 SXM",
		GPUMemGB:         80,
		CostPerHourCents: 300,
		DiskGB:           200,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{BlockedReasons: map[int64]string{}}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatalf("autoPilotRelaunch scope = %v, want no launch after reuse fill", scope)
		return nil, nil
	}

	submitCalls := 0
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, gotInstanceID int64, jobs []*db.Job) error {
		submitCalls++
		if gotInstanceID != instanceID {
			t.Fatalf("submit instance = %d, want %d", gotInstanceID, instanceID)
		}
		if len(jobs) != 1 || jobs[0] == nil || jobs[0].ID != jobID {
			t.Fatalf("submitted jobs = %#v, want [%d]", jobs, jobID)
		}
		if err := db.SetJobLaunchID(database, jobs[0].ID, gotInstanceID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}
		return nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil, withRealFillReusableInstances)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if submitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1", submitCalls)
	}
	if result.ReuseFilled != 1 {
		t.Fatalf("ReuseFilled = %d, want 1", result.ReuseFilled)
	}
	if result.Placed != 1 {
		t.Fatalf("Placed = %d, want 1", result.Placed)
	}
}

func TestFillReusableInstancesRecordsRejectionReason(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "incompatible reuse", "H100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	gpuMem := 120
	if err := db.SetJobGPUMemGB(database, jobID, &gpuMem); err != nil {
		t.Fatalf("SetJobGPUMemGB: %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "H100",
		ResolvedGPUName: "H100 SXM",
		GPUMemGB:        80,
		DiskGB:          200,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	diagnostics := map[int64]string{}
	placed, err := fillReusableInstances(context.Background(), database, nil, nil, nil, diagnostics, map[int64]string{}, nil)
	if err != nil {
		t.Fatalf("fillReusableInstances: %v", err)
	}
	if placed != 0 {
		t.Fatalf("placed = %d, want 0", placed)
	}
	if got := diagnostics[jobID]; !strings.Contains(got, "could not reuse") || !strings.Contains(got, "GPU memory insufficient") {
		t.Fatalf("reuse diagnostic = %q, want a could-not-reuse GPU memory reason", got)
	}
}

func TestRunGroupedAutoPilotPass_ReuseDiagnosticAppendedNotMasking(t *testing.T) {
	// Regression: an opportunistic reuse-rejection diagnostic must not mask
	// the authoritative launch-path reason. The launch reason leads; the
	// reuse diagnostic is appended as trailing detail.
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "needs-big-gpu", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	gpuMem := 80
	if err := db.SetJobGPUMemGB(database, jobID, &gpuMem); err != nil {
		t.Fatalf("SetJobGPUMemGB: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	// A running instance far too small to host the job: reuse is rejected.
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "NVIDIA",
		ResolvedGPUName: "GTX 1660 S",
		GPUMemGB:        6,
		DiskGB:          100,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalPlace := autoPilotPlaceComputeIntensive
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotPlaceComputeIntensive = originalPlace
	})
	autoPilotPlaceComputeIntensive = func(_ *sql.DB, _ *config.Config, jobs []*db.Job) ([]*db.Job, int) {
		return jobs, 0
	}
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   []int64{job.ID},
			BlockedReasons: map[int64]string{},
		}, nil
	}
	constraintReason := "12 offers found, but none passed the 80GB VRAM requirement"
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return &campaign.RelaunchResult{JobReasons: map[int64]string{jobID: constraintReason}}, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil, withRealFillReusableInstances)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	reason := result.BlockedReasons[jobID]
	if reason == "" {
		t.Fatalf("blocked reason empty, want an authoritative launch reason")
	}
	if !strings.HasPrefix(reason, constraintReason) {
		t.Fatalf("blocked reason = %q, want it to lead with the launch-path reason", reason)
	}
	if !strings.Contains(reason, "could not reuse") {
		t.Fatalf("blocked reason = %q, want the reuse diagnostic appended as detail", reason)
	}
}

func TestRunGroupedAutoPilotPass_FailedReuseFallsBackToRentalReason(t *testing.T) {
	// Regression: when a reuse assignment fails in a mixed planner pass, the
	// job must enter the fresh-rental scope and receive that path's concrete
	// reason. A reuse failure alone is never a blocker.
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "reuse-target", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{db.TagComputeIntensive}); err != nil {
		t.Fatalf("SetJobTags: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	// A running instance whose CPU count is below the cpu-intensive floor, so
	// the reuse assignment is rejected without a successful submit.
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "A100",
		ResolvedGPUName: "A100",
		GPUMemGB:        80,
		CPUCores:        8,
		DiskGB:          200,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	originalPlace := autoPilotPlaceComputeIntensive
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
		autoPilotPlaceComputeIntensive = originalPlace
	})
	autoPilotPlaceComputeIntensive = func(_ *sql.DB, _ *config.Config, jobs []*db.Job) ([]*db.Job, int) {
		return jobs, 0
	}
	// The job under test appears only as an incompatible reuse assignment,
	// alongside an unrelated planner decision. This was the shape that used
	// to drop the job from fresh-rental fallback.
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			ReuseAssignments: []campaign.ReuseAssignment{{
				Job:      job,
				Instance: campaign.InstanceCapacity{Instance: inst, DiskFreeGB: 80},
			}},
			BlockedReasons: map[int64]string{int64(999999): "planner: no compatible offers"},
		}, nil
	}
	submitCalls := 0
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ int64, _ []*db.Job) error {
		submitCalls++
		return nil
	}
	var relaunchScope []int64
	constraintReason := "7 offers found, but none passed the 80GB VRAM requirement"
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchScope = append([]int64(nil), scope...)
		return &campaign.RelaunchResult{JobReasons: map[int64]string{jobID: constraintReason}}, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if submitCalls != 0 {
		t.Fatalf("submit calls = %d, want 0 (reuse assignment is incompatible)", submitCalls)
	}
	reason := result.BlockedReasons[jobID]
	if reason == "" {
		t.Fatalf("job left with no blocked reason")
	}
	if len(relaunchScope) != 1 || relaunchScope[0] != jobID {
		t.Fatalf("relaunch scope = %v, want [%d]", relaunchScope, jobID)
	}
	if !strings.HasPrefix(reason, constraintReason) {
		t.Fatalf("blocked reason = %q, want concrete rental reason first", reason)
	}
	if !strings.Contains(reason, "CPU cores insufficient") {
		t.Fatalf("blocked reason = %q, want reuse rejection as secondary detail", reason)
	}
}

func TestRunGroupedAutoPilotPass_RelaunchNilResultStillFinalizesBlockedReasons(t *testing.T) {
	// The relaunch-nil-result exit must still assemble the result and run
	// the deferred blocked-reason epilogue: a planner-blocked job's reason
	// has to be both on the returned result and persisted to
	// placement_reasons even though the pass returns an error.
	database := db.SetupTestDB(t)
	blockedJobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python blocked.py", "blocked job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU blockedJob: %v", err)
	}
	rentalJobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python rental.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU rentalJob: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
	})
	plannerReason := "planner: no compatible offers"
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			BlockedReasons: map[int64]string{blockedJobID: plannerReason},
		}, nil
	}
	var relaunchScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchScope = append([]int64(nil), scope...)
		return nil, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err == nil || !strings.Contains(err.Error(), "relaunch returned no result") {
		t.Fatalf("error = %v, want relaunch-nil-result error", err)
	}
	if len(relaunchScope) != 1 || relaunchScope[0] != rentalJobID {
		t.Fatalf("relaunch scope = %v, want [%d]", relaunchScope, rentalJobID)
	}
	if result == nil {
		t.Fatal("result is nil; the nil-result relaunch exit must still assemble the result")
	}
	if got := result.BlockedReasons[blockedJobID]; got != plannerReason {
		t.Fatalf("blocked reason = %q, want %q", got, plannerReason)
	}
	job, err := db.GetJobByID(database, blockedJobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	persisted := false
	for _, reason := range job.PlacementReasons {
		if reason == plannerReason {
			persisted = true
		}
	}
	if !persisted {
		t.Fatalf("placement reasons = %#v, want persisted %q", job.PlacementReasons, plannerReason)
	}
}

func TestRunGroupedAutoPilotPass_DoesNotBlockJobCreatedAfterPlannerSnapshot(t *testing.T) {
	// Regression: a job that appears after the planner snapshot must not be
	// swept into this pass's fresh-rental scope or inherit one of its reasons.
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)

	plannedID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python planned.py", "planned", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU planned: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
	})

	var lateID int64
	autoPilotBuildPlan = func(database *sql.DB, _ *config.Config, jobs []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		if len(jobs) != 1 || jobs[0].ID != plannedID {
			t.Fatalf("planner jobs = %+v, want only %d", jobs, plannedID)
		}
		var err error
		lateID, err = db.RecordQueuedWithGPU(database, "", t.TempDir(), "python late.py", "late", "A100")
		if err != nil {
			t.Fatalf("RecordQueuedWithGPU late: %v", err)
		}
		assigned, err := db.AssignJobHost(database, plannedID, "host-alpha")
		if err != nil {
			t.Fatalf("AssignJobHost planned: %v", err)
		}
		if !assigned {
			t.Fatalf("planned job %d was not assigned", plannedID)
		}
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   []int64{plannedID},
			BlockedReasons: map[int64]string{},
		}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatal("newly-created job must wait for the next pass, not relaunch in this pass")
		return &campaign.RelaunchResult{}, nil
	}

	result, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if result.BlockedReasons[lateID] != "" {
		t.Fatalf("late job blocked reason = %q, want none from this pass", result.BlockedReasons[lateID])
	}
	late, err := db.GetJobByID(database, lateID)
	if err != nil {
		t.Fatalf("GetJobByID late: %v", err)
	}
	if got := strings.Join(late.PlacementReasons, "\n"); got != "" {
		t.Fatalf("late placement reasons = %q, want none from this pass", got)
	}
}

func TestAddAutoPilotBlockedReasonCombinesDistinctPaths(t *testing.T) {
	reasons := map[int64]string{
		1951: "run-rate headroom exhausted ($0.24/hr free, this group needs $1.52/hr)",
	}

	addAutoPilotBlockedReason(reasons, 1951, "reuse blocked: wi2808 RTX 6000Ada 45GB: GPU class mismatch: job=h100 instance=NVIDIA")
	addAutoPilotBlockedReason(reasons, 1951, "reuse blocked: wi2808 RTX 6000Ada 45GB: GPU class mismatch: job=h100 instance=NVIDIA")

	got := reasons[1951]
	if !strings.Contains(got, "run-rate headroom exhausted") {
		t.Fatalf("reason = %q, want run-rate reason", got)
	}
	if strings.Count(got, "reuse blocked") != 1 {
		t.Fatalf("reason = %q, want one reuse reason", got)
	}
}

func TestRunGroupedAutoPilotPass_ExcludesJobsWithOpenPlacementIntent(t *testing.T) {
	// Regression: a job with an open PlacementIntent (e.g. inside an
	// in-flight RelaunchOrphanedJobs or bulk move) is invisible to the
	// autopilot's planner and relaunch scope. See specs/job-move.allium
	// § AutopilotIgnoresMovingJobs (intent-inclusive).
	database := db.SetupTestDB(t)

	placing, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "placing", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU placing: %v", err)
	}
	other, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python b.py", "other", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU other: %v", err)
	}
	if _, err := db.CreatePlacementIntent(database, placing, "test"); err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	var seenUnplaced []int64
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, unplaced []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		for _, j := range unplaced {
			if j != nil {
				seenUnplaced = append(seenUnplaced, j.ID)
			}
		}
		return campaign.AutoPlacementPlan{}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return blockedRelaunchResult(scope, "test rental blocker"), nil
	}

	if _, err := runGroupedAutoPilotPassForTest(t, context.Background(), database, nil); err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}

	for _, id := range seenUnplaced {
		if id == placing {
			t.Errorf("planner saw placing job %d in unplaced set", placing)
		}
	}
	if len(seenUnplaced) == 0 || seenUnplaced[0] != other {
		t.Errorf("planner saw %v, expected only [%d]", seenUnplaced, other)
	}
}

// Regression (wb18/wj2812): a job whose source deterministically cannot ship
// to the cloud (over the size cap) must be skipped before any claim, with
// the validation error recorded as the AUTHORITATIVE blocked reason so weft
// info shows the real blocker instead of a
// per-instance reuse diagnostic.
func TestSubmitAutoPilotReuseAssignments_SourceTooLargeBlocksWithoutClaiming(t *testing.T) {
	database := db.SetupTestDB(t)

	dir := t.TempDir()
	fileSize := int64(weftsync.LargeSourceBlobThresholdBytes - 1)
	fileCount := int(weftsync.MaxSourceTarballBytes/fileSize) + 1
	for i := range fileCount {
		writeSparseTestFile(t, filepath.Join(dir, fmt.Sprintf("part%d.bin", i)), fileSize)
	}

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", dir, "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	reuseDiagnostics := map[int64]string{}
	blockedReasons := map[int64]string{}
	placed := submitAutoPilotReuseAssignments(context.Background(), database, nil,
		[]campaign.ReuseAssignment{{Job: job, Instance: campaign.InstanceCapacity{Instance: inst}}},
		reuseDiagnostics, blockedReasons, nil)

	if placed != 0 {
		t.Fatalf("placed = %d, want 0", placed)
	}
	reason := blockedReasons[jobID]
	if !strings.Contains(reason, "weft data publish") && !strings.Contains(reason, "exceeds") && !strings.Contains(reason, "limit") {
		t.Fatalf("blockedReasons = %q, want authoritative source-size reason", reason)
	}
	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("GetLaunchAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempt rows = %d, want 0 (no claim on deterministic rejection)", len(attempts))
	}
}

// Reuse backoff: the persisted reuse.submit_failed streak gates the
// automatic reuse paths with growing delays (mirroring the relaunch
// backoff); a reuse.submit_ok resets the streak.
func TestReuseBackoffRemaining(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	now := time.Now()
	failAt := func(ts time.Time) {
		t.Helper()
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind:  db.EventReuseSubmitFailed,
			JobID:      jobID,
			OccurredAt: ts.Unix(),
		}); err != nil {
			t.Fatalf("InsertLifecycleEvent: %v", err)
		}
	}

	if remaining, count := reuseBackoffRemaining(database, jobID, now); remaining != 0 || count != 0 {
		t.Fatalf("fresh job: remaining=%v count=%d, want 0/0", remaining, count)
	}

	failAt(now.Add(-2 * time.Second))
	remaining, count := reuseBackoffRemaining(database, jobID, now)
	if count != 1 || remaining <= 0 {
		t.Fatalf("after 1 failure 2s ago: remaining=%v count=%d, want positive backoff", remaining, count)
	}

	// Second consecutive failure grows the delay.
	failAt(now.Add(-1 * time.Second))
	remaining2, count2 := reuseBackoffRemaining(database, jobID, now)
	if count2 != 2 || remaining2 <= remaining {
		t.Fatalf("after 2 failures: remaining=%v count=%d, want longer than %v", remaining2, count2, remaining)
	}

	// A long-elapsed window clears the backoff but keeps the count.
	if remaining, count := reuseBackoffRemaining(database, jobID, now.Add(10*time.Minute)); remaining != 0 || count != 2 {
		t.Fatalf("elapsed window: remaining=%v count=%d, want 0/2", remaining, count)
	}

	// Success resets the streak.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventReuseSubmitOK,
		JobID:      jobID,
		OccurredAt: now.Unix(),
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	if remaining, count := reuseBackoffRemaining(database, jobID, now); remaining != 0 || count != 0 {
		t.Fatalf("after submit_ok: remaining=%v count=%d, want streak reset", remaining, count)
	}
}

// A job in its reuse backoff window is skipped (no claim, no submit) with a
// diagnostic, and the skip is recorded once per (job, count).
func TestSubmitAutoPilotReuseAssignments_BackoffSkips(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobID, err := db.RecordQueued(database, "", dir, "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventReuseSubmitFailed,
		JobID:      jobID,
		LaunchID:   instanceID,
		OccurredAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	reuseBackoffEventEmitted.Delete(jobID)

	reuseDiagnostics := map[int64]string{}
	placed := submitAutoPilotReuseAssignments(context.Background(), database, nil,
		[]campaign.ReuseAssignment{{Job: job, Instance: campaign.InstanceCapacity{Instance: inst}}},
		reuseDiagnostics, map[int64]string{}, nil)

	if placed != 0 {
		t.Fatalf("placed = %d, want 0 (backoff window)", placed)
	}
	if diag := reuseDiagnostics[jobID]; !strings.Contains(diag, "reuse backoff") {
		t.Fatalf("diagnostic = %q, want reuse backoff detail", diag)
	}
	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("GetLaunchAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempt rows = %d, want 0", len(attempts))
	}
	ev, err := db.LatestLifecycleEvent(database, db.LifecycleEventFilter{Kind: db.EventReuseSkippedBackoff})
	if err != nil || ev == nil || ev.JobID != jobID {
		t.Fatalf("expected reuse.skipped.backoff event for job %d, got %+v err %v", jobID, ev, err)
	}
}

// Recorded reuse failures and on-prem rejection detail must survive into the
// persisted placement_blocked JSON, with recorded outcomes beating the
// re-probed match result for the same instance.
func TestFinalizeUnplacedBlockedReasons_PersistsRecordedReuseAndOnPrem(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
		GPUMemGB: 24,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	instLabel := ids.FormatInstanceID(instanceID)
	launchReason := "provider launch failed: test outage"
	blockedReasons := map[int64]string{jobID: launchReason}
	structuredBlocked := map[int64]*blockreason.Structured{}
	recordedReuse := map[int64][]blockreason.ReuseRejection{
		jobID: {{Instance: instLabel, Reason: "submit failed: upload source: connection reset", Detail: "full error text"}},
	}
	onPremDetails := map[int64]string{
		jobID: "cool30: driver floor: NVIDIA driver 525.125.06 < required >=570",
	}

	finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked,
		map[int64]string{}, recordedReuse, onPremDetails,
		map[int64]*db.Job{jobID: job}, []int64{jobID},
		[]campaign.InstanceCapacity{{Instance: inst, DiskFreeGB: 100}}, nil)

	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	s := blockreason.Parse(refreshed.PlacementBlockedJSON)
	if s == nil {
		t.Fatalf("placement_blocked did not decode: %q", refreshed.PlacementBlockedJSON)
	}
	var found *blockreason.ReuseRejection
	for i := range s.Reuse {
		if s.Reuse[i].Instance == instLabel {
			found = &s.Reuse[i]
		}
	}
	if found == nil || !strings.Contains(found.Reason, "submit failed") {
		t.Fatalf("recorded reuse rejection missing or overridden by probe: %+v", s.Reuse)
	}
	if !strings.Contains(s.OnPrem, "cool30") {
		t.Fatalf("on-prem detail not persisted: %q", s.OnPrem)
	}
}

func TestFinalizeUnplacedBlockedReasons_DropsReuseOnlyNonCandidate(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	blockedReasons := map[int64]string{}
	structuredBlocked := map[int64]*blockreason.Structured{}
	finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked,
		map[int64]string{jobID: "could not reuse running instances: wi3816 RTX A6000 45GB: GPU memory insufficient: job=48GB"},
		nil, nil, map[int64]*db.Job{jobID: job}, nil, nil, nil)

	if len(blockedReasons) != 0 {
		t.Fatalf("blockedReasons = %v, want no reuse-only blocker", blockedReasons)
	}
	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	if len(refreshed.PlacementReasons) != 0 {
		t.Fatalf("placement reasons = %#v, want none", refreshed.PlacementReasons)
	}
	if refreshed.PlacementBlockedJSON != "" {
		t.Fatalf("placement_blocked = %q, want empty", refreshed.PlacementBlockedJSON)
	}
}

func TestPersistBlockedReasonsForUnplacedClearsStaleUnassertedBlock(t *testing.T) {
	database := db.SetupTestDB(t)

	clearedJobID, err := db.RecordQueued(database, "", t.TempDir(), "python stale.py", "stale")
	if err != nil {
		t.Fatalf("RecordQueued cleared job: %v", err)
	}
	blockedJobID, err := db.RecordQueued(database, "", t.TempDir(), "python blocked.py", "blocked")
	if err != nil {
		t.Fatalf("RecordQueued blocked job: %v", err)
	}
	stale := (&blockreason.Structured{Summary: "old image block", Launch: "old image block"}).Marshal()
	if err := db.SetJobPlacementBlocked(database, clearedJobID, stale); err != nil {
		t.Fatalf("SetJobPlacementBlocked cleared job: %v", err)
	}
	if err := db.SetJobPlacementBlocked(database, blockedJobID, stale); err != nil {
		t.Fatalf("SetJobPlacementBlocked blocked job: %v", err)
	}

	fresh := &blockreason.Structured{
		Summary: "fresh provider block",
		Launch:  "fresh provider block",
	}
	persistBlockedReasonsForUnplaced(database,
		map[int64]string{blockedJobID: "fresh provider block"},
		map[int64]*blockreason.Structured{blockedJobID: fresh})

	cleared, err := db.GetJobByID(database, clearedJobID)
	if err != nil {
		t.Fatalf("GetJobByID cleared job: %v", err)
	}
	if cleared.PlacementBlockedJSON != "" {
		t.Fatalf("cleared job placement_blocked = %q, want empty", cleared.PlacementBlockedJSON)
	}
	blocked, err := db.GetJobByID(database, blockedJobID)
	if err != nil {
		t.Fatalf("GetJobByID blocked job: %v", err)
	}
	if blocked.PlacementBlockedJSON != fresh.Marshal() {
		t.Fatalf("blocked job placement_blocked = %q, want %q", blocked.PlacementBlockedJSON, fresh.Marshal())
	}
}

func TestPersistBlockedReasonsForUnplacedClearsWhenNothingBlocked(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python stale.py", "stale")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	stale := (&blockreason.Structured{Summary: "old image block", Launch: "old image block"}).Marshal()
	if err := db.SetJobPlacementBlocked(database, jobID, stale); err != nil {
		t.Fatalf("SetJobPlacementBlocked: %v", err)
	}

	persistBlockedReasonsForUnplaced(database, map[int64]string{}, nil)

	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if refreshed.PlacementBlockedJSON != "" {
		t.Fatalf("placement_blocked = %q, want empty", refreshed.PlacementBlockedJSON)
	}
}

// A job the run-rate gate blocked from launching, whose running instances also
// genuinely refuse it, must persist BOTH operative reasons: the budget verdict
// as the launch blocker and the per-instance reuse rejections. Before this, the
// gate's flat budget string was the only thing persisted, so the TUI had no
// structured breakdown to expand and the "won't run on existing host" reason
// was lost.
func TestFinalizeUnplacedBlockedReasons_RunRateBlockedAttachesReuseBreakdown(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100_PCIE",
		GPUClass: "A100",
		GPUMemGB: 40,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	mem := 48
	job.GPUClass = "A100"
	job.GPUMemGB = &mem

	runRate := "run-rate target exceeded (no subset fits): target $3.50/hr, current $3.42/hr + planned $4.36/hr = $7.78/hr (headroom $0.08/hr, cheapest group $4.36/hr)"
	blockedReasons := map[int64]string{jobID: runRate}
	structuredBlocked := map[int64]*blockreason.Structured{}

	finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked,
		map[int64]string{}, nil, nil,
		map[int64]*db.Job{jobID: job}, nil,
		[]campaign.InstanceCapacity{{Instance: inst, DiskFreeGB: 100}}, nil)

	if got := blockedReasons[jobID]; got != runRate {
		t.Fatalf("compact reason changed: got %q, want unchanged budget verdict", got)
	}

	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	s := blockreason.Parse(refreshed.PlacementBlockedJSON)
	if s == nil {
		t.Fatalf("placement_blocked did not decode: %q", refreshed.PlacementBlockedJSON)
	}
	if !s.IsPlacementFailure() {
		t.Fatalf("expected an expandable placement failure, got %+v", s)
	}
	if !strings.Contains(s.Launch, "run-rate target exceeded") {
		t.Fatalf("launch blocker = %q, want the budget verdict", s.Launch)
	}
	if len(s.Reuse) == 0 {
		t.Fatalf("expected reuse rejections recovered by probing, got none")
	}
	if !strings.Contains(s.Reuse[0].Reason, "GPU memory insufficient") {
		t.Fatalf("reuse rejection = %q, want GPU memory diagnostic", s.Reuse[0].Reason)
	}
}

// Regression for wj4656: when a run-rate blocker already had a trailing
// "could not reuse..." diagnostic appended, placement_blocked.launch inherited
// that whole composite string. The grouped TUI then treated the reuse text as
// part of the primary launch blocker. Launch must remain launch-only while the
// structured Reuse section carries the secondary instance diagnostics.
func TestFinalizeUnplacedBlockedReasons_RunRateCompositeKeepsLaunchClean(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUSpec:         "A100_PCIE",
		GPUClass:        "A100",
		ResolvedGPUName: "A100 PCIE",
		GPUMemGB:        40,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	mem := 48
	job.GPUClass = "A100"
	job.GPUMemGB = &mem

	runRate := "run-rate headroom exhausted ($1.60/hr free, this group needs $1.65/hr)"
	composite := runRate + "; could not reuse running instances: wi5212 A100 PCIE 40GB: image incompatible"
	blockedReasons := map[int64]string{jobID: composite}
	structuredBlocked := map[int64]*blockreason.Structured{}

	finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked,
		map[int64]string{}, nil, nil,
		map[int64]*db.Job{jobID: job}, nil,
		[]campaign.InstanceCapacity{{Instance: inst, DiskFreeGB: 100}}, nil)

	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	s := blockreason.Parse(refreshed.PlacementBlockedJSON)
	if s == nil {
		t.Fatalf("placement_blocked did not decode: %q", refreshed.PlacementBlockedJSON)
	}
	if s.Launch != runRate {
		t.Fatalf("launch blocker = %q, want %q", s.Launch, runRate)
	}
	if strings.Contains(s.Launch, "could not reuse") {
		t.Fatalf("launch blocker = %q, must not include reuse diagnostics", s.Launch)
	}
	if len(s.Reuse) == 0 {
		t.Fatalf("expected reuse rejections recovered by probing, got none")
	}
	if !strings.Contains(s.Reuse[0].Reason, "GPU memory insufficient") {
		t.Fatalf("reuse rejection = %q, want GPU memory diagnostic", s.Reuse[0].Reason)
	}
}

// When the run-rate gate blocks a launch but a running instance could actually
// accept the job (compatible, merely busy), there is only one operative reason
// — the budget verdict. The finalize step must not fabricate a reuse-side
// breakdown in that case, leaving placement_blocked empty so the row stays a
// plain, non-expandable budget blocker.
func TestFinalizeUnplacedBlockedReasons_RunRateBlockedCompatibleInstanceStaysBudgetOnly(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100_SXM4",
		GPUClass: "A100",
		GPUMemGB: 80,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python x.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	mem := 48
	job.GPUClass = "A100"
	job.GPUMemGB = &mem

	runRate := "run-rate target exceeded (no subset fits): target $3.50/hr, current $3.42/hr + planned $4.36/hr = $7.78/hr (headroom $0.08/hr, cheapest group $4.36/hr)"
	blockedReasons := map[int64]string{jobID: runRate}
	structuredBlocked := map[int64]*blockreason.Structured{}

	finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked,
		map[int64]string{}, nil, nil,
		map[int64]*db.Job{jobID: job}, nil,
		[]campaign.InstanceCapacity{{Instance: inst, DiskFreeGB: 100}}, nil)

	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	if refreshed.PlacementBlockedJSON != "" {
		t.Fatalf("placement_blocked = %q, want empty (budget-only blocker)", refreshed.PlacementBlockedJSON)
	}
}

// Regression: the autopilot reuse path honors the per-job launch-attempt
// cap. reuseBackoffRemaining only tracks failed submits, so a job whose
// submits succeed but whose attempts then orphan (an instance repeatedly
// bouncing it) had no limiter on this path — it would be re-accepted
// every autopilot pass without bound.
func TestSubmitAutoPilotReuseAssignments_AttemptCapSkips(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	failedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch failed predecessor: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	for i := 0; i < campaign.DefaultMaxCloudAttempts; i++ {
		end := time.Now().Add(-10 * time.Minute).Unix()
		if _, err := database.Exec(
			`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, cloud_outcome, queued_at, start_time, end_time)
			 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?)`,
			jobID, i+1, failedID, db.StatusFailed, db.AttemptOutcomeOrphaned, end-60, end-60, end,
		); err != nil {
			t.Fatalf("insert attempt %d: %v", i+1, err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	reuseDiagnostics := map[int64]string{}
	placed := submitAutoPilotReuseAssignments(context.Background(), database, nil,
		[]campaign.ReuseAssignment{{Job: job, Instance: campaign.InstanceCapacity{Instance: inst}}},
		reuseDiagnostics, map[int64]string{}, nil)

	if placed != 0 {
		t.Fatalf("placed = %d, want 0 at the attempt cap", placed)
	}
	if !strings.Contains(reuseDiagnostics[jobID], "max cloud attempts") {
		t.Fatalf("reuseDiagnostics = %q, want max-attempts reason", reuseDiagnostics[jobID])
	}
}
