package campaign

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/retrypolicy"
)

func TestDefaultMaxCloudAttemptsUsesSharedPlacementBudget(t *testing.T) {
	if got, want := DefaultMaxCloudAttempts, retrypolicy.MaxPlacementAttempts(); got != want {
		t.Fatalf("DefaultMaxCloudAttempts = %d, want %d", got, want)
	}
}

func TestRelaunchOrphanedJobsSkipsUnsatisfiedAfterDependency(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordJobStarting(database, "cool30", "/tmp/p", "cmd", "producer")
	if err != nil {
		t.Fatalf("RecordJobStarting producer: %v", err)
	}
	consumerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/c", "cmd", "consumer", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU consumer: %v", err)
	}
	if err := db.SetJobDepSpec(database, consumerID, strconv.FormatInt(producerID, 10)); err != nil {
		t.Fatalf("SetJobDepSpec consumer: %v", err)
	}

	result, err := RelaunchOrphanedJobs(RelaunchConfig{
		Database:             database,
		IncludeFreshUnplaced: true,
	})
	if err != nil {
		t.Fatalf("RelaunchOrphanedJobs: %v", err)
	}
	if len(result.InstanceIDs) != 0 {
		t.Fatalf("InstanceIDs = %v, want none", result.InstanceIDs)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	reason := result.JobReasons[consumerID]
	if !strings.Contains(reason, "waiting for") || !strings.Contains(reason, "to succeed") {
		t.Fatalf("JobReasons[%d] = %q, want dependency wait", consumerID, reason)
	}
}

func TestRecordNotReplacedReason_DoesNotNestMultipleReasonsPrefix(t *testing.T) {
	result := &RelaunchResult{NotReplacedReasons: map[int64]string{}}
	recordNotReplacedReason(result, 1, "reason A")
	recordNotReplacedReason(result, 1, "reason B")
	recordNotReplacedReason(result, 1, "reason C")

	got := result.NotReplacedReasons[1]
	want := "multiple reasons (reason A; reason B; reason C)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRecordNotReplacedReason_DedupesIdenticalReasons(t *testing.T) {
	result := &RelaunchResult{NotReplacedReasons: map[int64]string{}}
	recordNotReplacedReason(result, 1, "reason A")
	recordNotReplacedReason(result, 1, "reason B")
	recordNotReplacedReason(result, 1, "reason A")

	got := result.NotReplacedReasons[1]
	want := "multiple reasons (reason A; reason B)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRecordNotReplacedReason_PreservesParensInReasonText(t *testing.T) {
	// New "waiting for ... (running)" reasons end with ')'. Make sure we
	// don't trim that when the existing entry isn't already wrapped.
	result := &RelaunchResult{NotReplacedReasons: map[int64]string{}}
	recordNotReplacedReason(result, 1, `waiting for "X" from wj1 (running)`)
	recordNotReplacedReason(result, 1, "no offers")

	got := result.NotReplacedReasons[1]
	want := `multiple reasons (waiting for "X" from wj1 (running); no offers)`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRecordRelaunchAssetStageEvent(t *testing.T) {
	database := db.SetupTestDB(t)
	groups := []InstanceGroup{
		{
			GPUClass: "l4",
			Jobs: []*db.Job{
				{ID: 1, Command: "echo one"},
				{ID: 2, Command: "echo two"},
			},
		},
	}

	recordRelaunchAssetStageEvent(database, groups, AssetStageStatus{
		Key:   "agent",
		Kind:  AssetStageKindAgent,
		Label: "agent",
		Phase: "checking local agent cache",
	})

	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{Kind: db.EventRelaunchAssetStage})
	if err != nil {
		t.Fatalf("ListLifecycleEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].GPUSpec != "l4" {
		t.Fatalf("GPUSpec = %q, want l4", events[0].GPUSpec)
	}
	if events[0].JobCount != 2 {
		t.Fatalf("JobCount = %d, want 2", events[0].JobCount)
	}
	if events[0].Detail != "agent agent checking local agent cache" {
		t.Fatalf("Detail = %q", events[0].Detail)
	}
}

func TestDriverFailureGPUNameSatisfiesGroup_DoesNotTreatL40AsL4(t *testing.T) {
	group := InstanceGroup{GPUClass: "l4"}

	if !driverFailureGPUNameSatisfiesGroup(group, "NVIDIA L4") {
		t.Fatal("NVIDIA L4 should satisfy l4")
	}
	if driverFailureGPUNameSatisfiesGroup(group, "NVIDIA L40") {
		t.Fatal("NVIDIA L40 should not satisfy l4")
	}
	if driverFailureGPUNameSatisfiesGroup(group, "NVIDIA L40S") {
		t.Fatal("NVIDIA L40S should not satisfy l4")
	}
}

func TestFilterOffersByDriverFailureExclusions(t *testing.T) {
	exclusions := driverFailureExclusions{
		machineKeys: map[string]struct{}{
			db.ProviderMachineKey("runpod", "machine-old-driver"): {},
		},
		gpuDataCenters: map[string]struct{}{
			driverFailureGPUDataCenterKey("runpod", "NVIDIA L4", "EU-RO-1"): {},
		},
		gpuNames: map[string]struct{}{
			driverFailureGPUKey("runpod", "NVIDIA L40"): {},
		},
	}
	offers := []cloud.Offer{
		{Provider: cloud.ProviderRunpod, GPUName: "NVIDIA L4", DataCenter: "EU-RO-2", MachineID: "ok"},
		{Provider: cloud.ProviderRunpod, GPUName: "NVIDIA L4", DataCenter: "EU-RO-2", MachineID: "machine-old-driver"},
		{Provider: cloud.ProviderRunpod, GPUName: "NVIDIA L4", DataCenter: "EU-RO-1", MachineID: "other"},
		{Provider: cloud.ProviderRunpod, GPUName: "NVIDIA L40", DataCenter: "US-KS-2", MachineID: "different"},
		{Provider: cloud.ProviderVastai, GPUName: "NVIDIA L40", DataCenter: "US-KS-2", MachineID: "different"},
	}

	filtered, summary := filterOffersByDriverFailureExclusions(offers, exclusions)

	if summary.Removed != 3 {
		t.Fatalf("Removed = %d, want 3", summary.Removed)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered len = %d, want 2", len(filtered))
	}
	if filtered[0].Provider != cloud.ProviderRunpod || filtered[0].GPUName != "NVIDIA L4" || filtered[0].DataCenter != "EU-RO-2" {
		t.Fatalf("unexpected first surviving offer: %+v", filtered[0])
	}
	if filtered[1].Provider != cloud.ProviderVastai || filtered[1].GPUName != "NVIDIA L40" {
		t.Fatalf("unexpected second surviving offer: %+v", filtered[1])
	}
	wantReasons := []string{"prior incompatible gpu", "same gpu/datacenter", "same provider machine"}
	if len(summary.Reasons) != len(wantReasons) {
		t.Fatalf("Reasons = %#v, want %#v", summary.Reasons, wantReasons)
	}
	for i, want := range wantReasons {
		if summary.Reasons[i] != want {
			t.Fatalf("Reasons[%d] = %q, want %q", i, summary.Reasons[i], want)
		}
	}
}

func TestExceedsRetryBudget_FirstRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 45 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  45 * time.Minute,
		NextCostCents:  25,
	}
	if exceeded, _ := exceedsRetryBudget(budget, 1, 10*time.Minute, 50); exceeded {
		t.Fatal("first retry should not exceed budget")
	}
	if exceeded, detail := exceedsRetryBudget(budget, 1, 46*time.Minute, 50); !exceeded {
		t.Fatal("first retry time should exceed budget")
	} else if detail == "" {
		t.Fatal("expected detail for exceeded budget")
	}
	if exceeded, _ := exceedsRetryBudget(budget, 1, 10*time.Minute, 120); !exceeded {
		t.Fatal("first retry spend should exceed budget")
	}
}

func TestExceedsRetryBudget_SubsequentRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 45 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  30 * time.Minute,
		NextCostCents:  25,
	}
	if exceeded, _ := exceedsRetryBudget(budget, 2, 20*time.Minute, 20); exceeded {
		t.Fatal("subsequent retry should not exceed budget")
	}
	if exceeded, _ := exceedsRetryBudget(budget, 2, 31*time.Minute, 20); !exceeded {
		t.Fatal("subsequent retry time should exceed budget")
	}
	if exceeded, _ := exceedsRetryBudget(budget, 3, 20*time.Minute, 30); !exceeded {
		t.Fatal("subsequent retry spend should exceed budget")
	}
}

func TestLaunchElapsedAndSpendCents(t *testing.T) {
	start := time.Now().Add(-30 * time.Minute).Unix()
	end := time.Now().Add(-5 * time.Minute).Unix()
	ci := &db.Launch{
		LaunchedAt:         &start,
		EndedAt:            &end,
		CostPerHourCents:   120,
		ActualSpendCents:   0,
		ProviderRunningAt:  nil,
		ProviderInstanceID: "x",
	}
	elapsed, cents := launchElapsedAndSpendCents(ci, time.Now())
	if elapsed < 24*time.Minute || elapsed > 26*time.Minute {
		t.Fatalf("elapsed = %v, want about 25m", elapsed)
	}
	if cents < 49 || cents > 51 {
		t.Fatalf("spend cents = %d, want about 50", cents)
	}

	ci.ActualSpendCents = 77
	_, cents = launchElapsedAndSpendCents(ci, time.Now())
	if cents != 77 {
		t.Fatalf("actual spend should win, got %d", cents)
	}
}

func TestResolveHedgeProbeLaunchTargetUsesProbeOfferProvider(t *testing.T) {
	vastClient := &cloud.MockClient{ProviderVal: cloud.ProviderVastai}
	runpodClient := &cloud.MockClient{ProviderVal: cloud.ProviderRunpod}
	cfg := RelaunchConfig{
		Clients: []cloud.Client{vastClient, runpodClient},
		CreateOptsForProvider: func(provider cloud.Provider) (cloud.CreateOpts, error) {
			return cloud.CreateOpts{Image: string(provider) + "-image"}, nil
		},
	}

	client, createOpts, err := resolveHedgeProbeLaunchTarget(cfg, cloud.Offer{Provider: cloud.ProviderRunpod})
	if err != nil {
		t.Fatalf("resolveHedgeProbeLaunchTarget: %v", err)
	}
	if client != runpodClient {
		t.Fatalf("client provider = %s, want %s", client.Provider(), cloud.ProviderRunpod)
	}
	if createOpts.Image != "runpod-image" {
		t.Fatalf("create opts image = %q, want runpod-image", createOpts.Image)
	}
}

func TestApplyRetryBudgetMultiplier_FirstRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 10 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  20 * time.Minute,
		NextCostCents:  50,
	}
	scaled := applyRetryBudgetMultiplier(budget, 1, 2.0)
	if scaled.FirstTimeLimit != 20*time.Minute {
		t.Fatalf("first time limit = %s, want 20m", scaled.FirstTimeLimit)
	}
	if scaled.FirstCostCents != 200 {
		t.Fatalf("first cost = %d, want 200", scaled.FirstCostCents)
	}
	if scaled.NextTimeLimit != budget.NextTimeLimit || scaled.NextCostCents != budget.NextCostCents {
		t.Fatal("next-tier limits should remain unchanged for first retry scaling")
	}
}

func TestApplyRetryBudgetMultiplier_SubsequentRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 10 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  15 * time.Minute,
		NextCostCents:  30,
	}
	scaled := applyRetryBudgetMultiplier(budget, 2, 2.0)
	if scaled.NextTimeLimit != 30*time.Minute {
		t.Fatalf("next time limit = %s, want 30m", scaled.NextTimeLimit)
	}
	if scaled.NextCostCents != 60 {
		t.Fatalf("next cost = %d, want 60", scaled.NextCostCents)
	}
	if scaled.FirstTimeLimit != budget.FirstTimeLimit || scaled.FirstCostCents != budget.FirstCostCents {
		t.Fatal("first-tier limits should remain unchanged for subsequent retry scaling")
	}
}

func TestRetryBudgetUsage_NoStartConsumesNoBudget(t *testing.T) {
	now := time.Unix(2_000, 0)
	launchedAt := int64(1_000)
	endedAt := int64(1_900)
	facts := relaunchAttemptFacts{
		LastLaunch: &db.Launch{
			LaunchedAt:       &launchedAt,
			EndedAt:          &endedAt,
			CostPerHourCents: 200,
		},
		LastAttemptStartTime: 0,
		LastAttemptEndTime:   &endedAt,
	}

	elapsed, spend := retryBudgetUsage(facts, now)
	if elapsed != 0 {
		t.Fatalf("elapsed = %v, want 0", elapsed)
	}
	if spend != 0 {
		t.Fatalf("spend = %d, want 0", spend)
	}
}

func TestRetryBudgetUsage_ProratesActualSpendByAttemptElapsed(t *testing.T) {
	now := time.Unix(4_000, 0)
	launchedAt := int64(1_000)
	endedAt := int64(3_000) // 2000s launch elapsed
	attemptStart := int64(2_000)
	attemptEnd := int64(3_000) // 1000s attempt elapsed (50%)
	facts := relaunchAttemptFacts{
		LastLaunch: &db.Launch{
			LaunchedAt:       &launchedAt,
			EndedAt:          &endedAt,
			ActualSpendCents: 300,
		},
		LastAttemptStartTime: attemptStart,
		LastAttemptEndTime:   &attemptEnd,
	}

	elapsed, spend := retryBudgetUsage(facts, now)
	if elapsed != time.Duration(attemptEnd-attemptStart)*time.Second {
		t.Fatalf("elapsed = %v, want %v", elapsed, time.Duration(attemptEnd-attemptStart)*time.Second)
	}
	if spend != 150 {
		t.Fatalf("spend = %d, want 150", spend)
	}
}

func TestAttemptFactsForRelaunch_SkipsCanceledAttemptForTiming(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	launchA, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch A: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchA); err != nil {
		t.Fatalf("set launch A: %v", err)
	}
	failedStart := time.Now().Add(-2 * time.Hour).Unix()
	failedEnd := failedStart + 40
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ?, end_time = ?, status = ?, cloud_outcome = ?
		 WHERE job_id = ? AND launch_id = ?`,
		failedStart, failedEnd, db.StatusFailed, db.AttemptOutcomeFailed, jobID, launchA,
	); err != nil {
		t.Fatalf("update attempt A: %v", err)
	}

	launchB, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch B: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create attempt B: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchB); err != nil {
		t.Fatalf("set launch B: %v", err)
	}
	canceledStart := time.Now().Add(-90 * time.Minute).Unix()
	canceledEnd := canceledStart + int64((12*time.Hour + 38*time.Minute + 35*time.Second).Seconds())
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ?, end_time = ?, status = ?, cloud_outcome = ?
		 WHERE job_id = ? AND launch_id = ?`,
		canceledStart, canceledEnd, db.StatusCanceled, db.AttemptOutcomeCancelled, jobID, launchB,
	); err != nil {
		t.Fatalf("update attempt B: %v", err)
	}

	facts, err := attemptFactsForRelaunch(database, jobID)
	if err != nil {
		t.Fatalf("attemptFactsForRelaunch: %v", err)
	}
	if facts.LastAttemptStartTime != failedStart {
		t.Fatalf("LastAttemptStartTime = %d, want %d", facts.LastAttemptStartTime, failedStart)
	}
	if facts.LastAttemptEndTime == nil || *facts.LastAttemptEndTime != failedEnd {
		t.Fatalf("LastAttemptEndTime = %v, want %d", facts.LastAttemptEndTime, failedEnd)
	}

	elapsed, _ := retryBudgetUsage(facts, time.Now())
	if elapsed > time.Minute {
		t.Fatalf("elapsed = %v, want ~40s", elapsed)
	}
}

func TestAttemptFactsForRelaunch_AllCanceledYieldsZeroTiming(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	launch, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launch); err != nil {
		t.Fatalf("set launch: %v", err)
	}
	start := time.Now().Add(-time.Hour).Unix()
	end := start + 1800
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ?, end_time = ?, status = ?, cloud_outcome = ?
		 WHERE job_id = ? AND launch_id = ?`,
		start, end, db.StatusCanceled, db.AttemptOutcomeCancelled, jobID, launch,
	); err != nil {
		t.Fatalf("update attempt: %v", err)
	}

	facts, err := attemptFactsForRelaunch(database, jobID)
	if err != nil {
		t.Fatalf("attemptFactsForRelaunch: %v", err)
	}
	if facts.LastAttemptStartTime != 0 {
		t.Fatalf("LastAttemptStartTime = %d, want 0", facts.LastAttemptStartTime)
	}
	elapsed, spend := retryBudgetUsage(facts, time.Now())
	if elapsed != 0 || spend != 0 {
		t.Fatalf("elapsed=%v spend=%d, want zero", elapsed, spend)
	}
}

func TestRetryBudgetUsage_IgnoresCanceledAttemptDuration(t *testing.T) {
	// retryBudgetUsage relies on attemptFactsForRelaunch to filter out
	// canceled attempts before passing facts in; this test locks in the
	// contract at the retryBudgetUsage boundary.
	now := time.Unix(10_000, 0)
	failedStart := int64(9_960)
	failedEnd := int64(10_000)
	facts := relaunchAttemptFacts{
		LastAttemptStartTime: failedStart,
		LastAttemptEndTime:   &failedEnd,
	}
	elapsed, spend := retryBudgetUsage(facts, now)
	if elapsed != 40*time.Second {
		t.Fatalf("elapsed = %v, want 40s", elapsed)
	}
	if spend != 0 {
		t.Fatalf("spend = %d, want 0", spend)
	}
}

func TestRelaunchGroupingJobsTreatsUnplacedPendingPlacementAsQueued(t *testing.T) {
	queued := &db.Job{ID: 1, Status: db.StatusQueued}
	pendingUnplaced := &db.Job{ID: 2, Status: db.StatusPendingPlacement}
	pendingPlaced := &db.Job{ID: 3, Status: db.StatusPendingPlacement, LaunchID: testInt64Ptr(77)}

	grouping := relaunchGroupingJobs([]*db.Job{queued, pendingUnplaced, pendingPlaced})
	if len(grouping) != 3 {
		t.Fatalf("grouping len = %d, want 3", len(grouping))
	}

	if grouping[0] != queued {
		t.Fatal("queued job should be passed through")
	}
	if grouping[1] == pendingUnplaced {
		t.Fatal("pending unplaced job should be copied before normalization")
	}
	if grouping[1].Status != db.StatusQueued {
		t.Fatalf("pending unplaced normalized status = %q, want %q", grouping[1].Status, db.StatusQueued)
	}
	if pendingUnplaced.Status != db.StatusPendingPlacement {
		t.Fatalf("original pending unplaced status mutated to %q", pendingUnplaced.Status)
	}
	if grouping[2] != pendingPlaced {
		t.Fatal("pending placed job should not be rewritten")
	}
}

func testInt64Ptr(v int64) *int64 {
	return &v
}

func TestBackoffRemaining(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		count    int
		hasEnd   bool
		elapsed  time.Duration
		wantZero bool
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{name: "no failures", count: 0, wantZero: true},
		{name: "missing end time falls open", count: 3, wantZero: true},
		{name: "within first window", count: 1, hasEnd: true, elapsed: 5 * time.Second, wantMin: 1, wantMax: 15 * time.Second},
		{name: "past first window", count: 1, hasEnd: true, elapsed: 30 * time.Second, wantZero: true},
		{name: "second window still pending", count: 2, hasEnd: true, elapsed: 20 * time.Second, wantMin: 1, wantMax: 30 * time.Second},
		{name: "high count clamps to 2m schedule", count: 10, hasEnd: true, elapsed: 30 * time.Second, wantMin: 60 * time.Second, wantMax: 2 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := relaunchAttemptFacts{Count: tt.count}
			if tt.hasEnd {
				end := now.Add(-tt.elapsed).Unix()
				facts.LastAttemptEndTime = &end
			}
			got := backoffRemaining(facts, now)
			if tt.wantZero {
				if got != 0 {
					t.Fatalf("want 0, got %v", got)
				}
				return
			}
			if got < tt.wantMin || got > tt.wantMax {
				t.Fatalf("want %v..%v, got %v", tt.wantMin, tt.wantMax, got)
			}
		})
	}
}
