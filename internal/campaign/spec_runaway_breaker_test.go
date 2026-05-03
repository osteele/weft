// Tests derived from specs/campaign-lifecycle.allium — RunawayBreakerTrips and
// RunawayBreakerClears rules. Verifies the breaker trips when repeated failures
// occur without progress, blocks subsequent relaunches, and auto-clears on retry.
package campaign

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func seedOrphanedLaunches(t *testing.T, database *sql.DB, jobID int64, count int, campaignID *int64) {
	t.Helper()
	for i := 0; i < count; i++ {
		launchID, err := db.CreateLaunch(database, &db.Launch{
			CampaignID: campaignID,
			Status:     db.LaunchStatusPlanned,
			Provider:   "vastai",
			GPUSpec:    "RTX_4090",
		})
		if err != nil {
			t.Fatalf("CreateLaunch[%d]: %v", i, err)
		}
		if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusLaunching); err != nil {
			t.Fatalf("launch[%d] to launching: %v", i, err)
		}
		if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusRunning); err != nil {
			t.Fatalf("launch[%d] to running: %v", i, err)
		}
		if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID[%d]: %v", i, err)
		}
		if _, err := database.Exec(
			`UPDATE job_attempts SET cloud_outcome = ?, end_time = ?
			 WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
			db.AttemptOutcomeOrphaned, time.Now().Unix(), jobID, launchID,
		); err != nil {
			t.Fatalf("mark orphaned[%d]: %v", i, err)
		}
		if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure); err != nil {
			t.Fatalf("fail launch[%d]: %v", i, err)
		}
	}
}

func TestSpec_RunawayBreakerTrips_OnOrphanChurnWithNoProgress(t *testing.T) {
	// Spec: breaker trips when zero completions AND orphan count >= limit.
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// Create a job in the campaign scope.
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	seedOrphanedLaunches(t, database, jobID, 10, &campaignID)

	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
		},
	}

	tripped, reason, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if !tripped {
		t.Fatal("expected breaker to trip: 10 orphaned attempts with zero completions")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestSpec_RunawayBreakerDoesNotTrip_WithCompletions(t *testing.T) {
	// Spec: breaker requires zero completions. If any job completed, no trip.
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	// Create a mix of orphaned and completed attempts.
	for i := 0; i < 10; i++ {
		launchID, err := db.CreateLaunch(database, &db.Launch{
			CampaignID: &campaignID,
			Status:     db.LaunchStatusPlanned,
			Provider:   "vastai",
			GPUSpec:    "RTX_4090",
		})
		if err != nil {
			t.Fatalf("CreateLaunch[%d]: %v", i, err)
		}
		if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusLaunching); err != nil {
			t.Fatalf("launch[%d] to launching: %v", i, err)
		}
		if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusRunning); err != nil {
			t.Fatalf("launch[%d] to running: %v", i, err)
		}
		if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID[%d]: %v", i, err)
		}

		outcome := db.AttemptOutcomeOrphaned
		if i == 5 {
			outcome = db.AttemptOutcomeCompleted // one completion breaks the breaker
		}
		if _, err := database.Exec(
			`UPDATE job_attempts SET cloud_outcome = ?, end_time = ?
			 WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
			outcome, time.Now().Unix(), jobID, launchID,
		); err != nil {
			t.Fatalf("mark outcome[%d]: %v", i, err)
		}
		if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure); err != nil {
			t.Fatalf("fail launch[%d]: %v", i, err)
		}
	}

	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
		},
	}

	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if tripped {
		t.Fatal("breaker should NOT trip when there are completions")
	}
}

func TestSpec_RunawayBreakerBlocks_WhenAlreadyTripped(t *testing.T) {
	// Spec: once tripped, subsequent evaluations return blocked without re-evaluating metrics.
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	// Create a launch so the job can infer the campaign ID.
	launchID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Insert a tripped event matching the empty-project scope (project=<all>).
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: campaignID,
		Detail:     "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
		},
	}

	tripped, reason, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if !tripped {
		t.Fatal("expected breaker to be tripped (event exists)")
	}
	if reason == "" {
		t.Error("expected non-empty reason for tripped breaker")
	}
}

func TestSpec_RunawayBreakerClears_OnResume(t *testing.T) {
	// Spec: RunawayBreakerClears — resume event after trip clears the block.
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	// Trip then resume.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: campaignID,
		Detail:     "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("insert tripped: %v", err)
	}
	// Small delay to ensure resume timestamp > trip timestamp.
	time.Sleep(10 * time.Millisecond)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayResumed,
		CampaignID: campaignID,
		Detail:     "project=<all>; auto-resumed via retry",
	}); err != nil {
		t.Fatalf("insert resumed: %v", err)
	}

	launchID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
		},
	}

	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if tripped {
		t.Fatal("breaker should NOT be tripped after resume event")
	}
}

func TestSpec_RunawayBreakerGracePeriod_SkipsCheckAfterResume(t *testing.T) {
	// After a resume, the breaker should not re-trip during the grace period,
	// even if metrics would otherwise trigger it. This prevents false positives
	// when new instances haven't completed any jobs yet.
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	// Trip the breaker
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: campaignID,
		Detail:     "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("insert tripped: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	// Resume it
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayResumed,
		CampaignID: campaignID,
		Detail:     "project=<all>; auto-resumed via retry",
	}); err != nil {
		t.Fatalf("insert resumed: %v", err)
	}

	// Create orphaned instances that would normally re-trip the breaker
	for i := 0; i < 5; i++ {
		launchID, err := db.CreateLaunch(database, &db.Launch{
			CampaignID: &campaignID,
			Status:     db.LaunchStatusFailed,
			Provider:   "vastai",
			GPUSpec:    "RTX_4090",
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if i == 0 {
			if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
				t.Fatalf("SetJobLaunchID: %v", err)
			}
		}
	}

	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         3, // low threshold — would trip without grace
			SpendNoProgressLimitCent: 500,
			ResumeGracePeriod:        15 * time.Minute,
		},
	}

	// Evaluate immediately after resume — should NOT trip due to grace period
	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if tripped {
		t.Fatal("breaker should NOT re-trip during grace period after resume")
	}
}

func TestSpec_RunawayBreakerIgnoresGracePeriodFailuresAfterResume(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	resumedAt := time.Now().Unix()
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayResumed,
		OccurredAt: resumedAt,
		Detail:     "project=<all>; manual reset via test",
	}); err != nil {
		t.Fatalf("insert resumed: %v", err)
	}

	graceAttemptAt := resumedAt + int64((5 * time.Minute).Seconds())
	for i := 0; i < 5; i++ {
		launchID, err := db.CreateLaunch(database, &db.Launch{
			Status:   db.LaunchStatusFailed,
			Provider: "vastai",
			GPUSpec:  "RTX_4090",
		})
		if err != nil {
			t.Fatalf("CreateLaunch[%d]: %v", i, err)
		}
		if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID[%d]: %v", i, err)
		}
		if _, err := database.Exec(
			`UPDATE job_attempts SET queued_at = ?, cloud_outcome = ?, end_time = ?
			 WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
			graceAttemptAt, db.AttemptOutcomeOrphaned, graceAttemptAt, jobID, launchID,
		); err != nil {
			t.Fatalf("mark orphaned[%d]: %v", i, err)
		}
	}

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         3,
			SpendNoProgressLimitCent: 500,
			ResumeGracePeriod:        15 * time.Minute,
		},
	}

	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Unix(resumedAt, 0).Add(16*time.Minute))
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if tripped {
		t.Fatal("breaker should ignore orphaned attempts that happened during reset grace")
	}
}

func TestSpec_RunawayBreakerDisabled(t *testing.T) {
	// Spec: RunawayBreakerTrips requires config.runaway_enabled.
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	// Disabled policy — should never trip.
	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled: false,
		},
	}

	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if tripped {
		t.Fatal("breaker should not trip when disabled")
	}

	// Nil policy — should never trip.
	cfg.RunawayPolicy = nil
	tripped, _, err = evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker (nil policy): %v", err)
	}
	if tripped {
		t.Fatal("breaker should not trip with nil policy")
	}
}

func TestSpec_RunawayBreakerTrips_WithoutCampaignID(t *testing.T) {
	// Breaker must trip even when launches have NULL campaign_id.
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	seedOrphanedLaunches(t, database, jobID, 10, nil)

	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
		},
	}

	tripped, reason, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, time.Now())
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker: %v", err)
	}
	if !tripped {
		t.Fatal("expected breaker to trip on 10 orphaned attempts even with NULL campaign_id")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestResetGlobalRunawayBreaker_ClearsTrippedBreaker(t *testing.T) {
	// ResetGlobalRunawayBreaker should write a resume event with the
	// project=<all> scope, clearing a previously tripped global breaker.
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	// Trip the breaker without a campaign linkage (the case the new helper targets).
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent (trip): %v", err)
	}

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
			ResumeGracePeriod:        time.Nanosecond, // skip the post-resume grace window
		},
	}

	now := time.Now()
	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, now)
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker (before reset): %v", err)
	}
	if !tripped {
		t.Fatal("expected breaker tripped before reset")
	}

	if err := ResetGlobalRunawayBreaker(database, "test"); err != nil {
		t.Fatalf("ResetGlobalRunawayBreaker: %v", err)
	}

	// Advance time past the grace window so the breaker is fully clear.
	tripped, _, err = evaluateRunawayBreaker(database, cfg, []*db.Job{job}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker (after reset): %v", err)
	}
	if tripped {
		t.Fatal("expected breaker cleared after ResetGlobalRunawayBreaker")
	}
}

func TestResetGlobalRunawayBreaker_ClearsCampaignScopedTrip(t *testing.T) {
	// A global resume must clear a trip that was originally recorded with
	// a specific campaign_id. The runaway-breaker check infers the
	// campaign from the unplaced jobs' launches, so a global reset that
	// only writes a campaign_id=NULL resume previously failed to match,
	// leaving the campaign-scoped trip in effect.
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	const campaignID int64 = 42
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: campaignID,
		Detail:     "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent (trip): %v", err)
	}

	now := time.Now()
	resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, campaignID, "")
	trippedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayTripped, campaignID, "")
	if !(trippedAt > resumedAt) {
		t.Fatalf("expected trip > resume before reset, got tripped=%d resumed=%d", trippedAt, resumedAt)
	}

	if err := ResetGlobalRunawayBreaker(database, "test"); err != nil {
		t.Fatalf("ResetGlobalRunawayBreaker: %v", err)
	}

	resumedAt = latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, campaignID, "")
	trippedAt = latestRunawayEventAt(database, db.EventRelaunchRunawayTripped, campaignID, "")
	if resumedAt < trippedAt {
		t.Fatalf("expected global resume to override campaign-scoped trip, got tripped=%d resumed=%d", trippedAt, resumedAt)
	}

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:                  true,
			Window:                   24 * time.Hour,
			ChainNoProgressLimit:     3,
			OrphanChurnLimit:         8,
			SpendNoProgressLimitCent: 500,
			ResumeGracePeriod:        time.Nanosecond,
		},
	}
	tripped, _, err := evaluateRunawayBreaker(database, cfg, []*db.Job{job}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("evaluateRunawayBreaker (after reset): %v", err)
	}
	if tripped {
		t.Fatal("expected breaker cleared by global reset even though trip was campaign-scoped")
	}
}
