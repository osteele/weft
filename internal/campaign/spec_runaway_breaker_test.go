// Tests derived from specs/campaign-lifecycle.allium — RunawayBreakerTrips and
// RunawayBreakerClears rules. Verifies the breaker trips when repeated failures
// occur without progress, blocks subsequent relaunches, and auto-clears on retry.
package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

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

	// Simulate orphaned attempts: create launches that failed, each with an
	// orphaned attempt for our job.
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
		// Mark the attempt as orphaned (instance failed before job ran).
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

	// Re-read the job to get it in queued state for evaluation.
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
