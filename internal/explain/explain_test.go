package explain

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestForJobExplainsInventoryDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
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
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, now)
	if x.State != "waiting" {
		t.Fatalf("State = %q, want waiting", x.State)
	}
	if !x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = false, want true")
	}
	if !strings.Contains(x.PrimaryReason, "retry #2") {
		t.Fatalf("PrimaryReason = %q, want retry count", x.PrimaryReason)
	}
	if !strings.Contains(x.SuggestedAction, "replan") {
		t.Fatalf("SuggestedAction = %q, want replan", x.SuggestedAction)
	}
	if !strings.Contains(x.SuggestedAction, "waiting") {
		t.Fatalf("SuggestedAction = %q, want waiting wording", x.SuggestedAction)
	}
}

// TestForJobExplainsSourceProvenanceMismatchAsDispatchBlock validates that a
// queued job whose latest dispatch failed with a source_provenance_mismatch
// reason surfaces that reason through explain.ForJob (Layer A+B). Without
// this wiring the diagnose surface falls back to the generic "job is queued
// / wait" reply that triggered the original bug report.
func TestForJobExplainsSourceProvenanceMismatchAsDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-2*time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	reason := "source_provenance_mismatch: expected=991f6ad9, marker=d7531f4c"
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-30 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		Detail:     reason,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, now)
	if x.State != "blocked" {
		t.Fatalf("State = %q, want blocked", x.State)
	}
	if !strings.Contains(x.PrimaryReason, "source_provenance_mismatch") {
		t.Fatalf("PrimaryReason = %q, want source_provenance_mismatch", x.PrimaryReason)
	}
	if !strings.Contains(x.PrimaryReason, "expected=991f6ad9") {
		t.Fatalf("PrimaryReason = %q, want expected=991f6ad9", x.PrimaryReason)
	}
	var hasReplan bool
	for _, opt := range x.Options {
		if opt.Label == "replan" {
			hasReplan = true
			break
		}
	}
	if !hasReplan {
		t.Fatalf("Options missing 'replan' choice: %+v", x.Options)
	}
}

func TestForJobExplainsUnplacedPlacementReason(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	reason := "planner: provider command timed out: vastai search offers timed out after 30s"
	if err := db.SetJobPlacementReasons(database, jobID, []string{
		"cloud instance 4087 failed (provider_timeout)",
		reason,
	}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, time.Unix(20_000, 0))
	if x.State != "blocked" {
		t.Fatalf("State = %q, want blocked", x.State)
	}
	if x.PrimaryReason != reason {
		t.Fatalf("PrimaryReason = %q, want %q", x.PrimaryReason, reason)
	}
	if x.SuggestedAction != "inspect blocker" {
		t.Fatalf("SuggestedAction = %q, want inspect blocker", x.SuggestedAction)
	}
	if x.Confidence != "high" {
		t.Fatalf("Confidence = %q, want high", x.Confidence)
	}
}

func TestForJobExplainsQueuedRentalTarget(t *testing.T) {
	launchID := int64(4092)
	job := &db.Job{
		ID:       3390,
		Status:   db.StatusQueued,
		LaunchID: &launchID,
	}

	x := ForJob(nil, job, time.Unix(20_000, 0))
	if x.PrimaryReason != "waiting for assigned instance to start the job" {
		t.Fatalf("PrimaryReason = %q", x.PrimaryReason)
	}
	if x.SuggestedAction != "wait for instance startup or inspect instance" {
		t.Fatalf("SuggestedAction = %q", x.SuggestedAction)
	}
}

func TestForJobShowsTransitiveProducerRootBlocker(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 300, "", "/tmp/root", "python root.py", "root producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 300: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, 300, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	nowUnix := time.Unix(20_000, 0).Unix()
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ?`,
		db.StatusRunning, nowUnix-120, 300); err != nil {
		t.Fatalf("set root running: %v", err)
	}
	if err := db.RecordQueuedWithGPUAndID(database, 301, "", "/tmp/mid", "python mid.py", "intermediate", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 301: %v", err)
	}
	if err := db.SetJobNeeds(database, 301, []string{"output/root.pt:300"}); err != nil {
		t.Fatalf("SetJobNeeds 301: %v", err)
	}
	if err := db.RecordQueuedWithGPUAndID(database, 302, "", "/tmp/consumer", "python consumer.py", "consumer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 302: %v", err)
	}
	if err := db.SetJobNeeds(database, 302, []string{"output/mid.pt:301"}); err != nil {
		t.Fatalf("SetJobNeeds 302: %v", err)
	}
	job, err := db.GetJobByID(database, 302)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID(302): %v %v", job, err)
	}
	job.QueueBlockedReason = `waiting for "output/mid.pt" from wj301 (queued)`

	x := ForJob(database, job, time.Unix(20_000, 0))
	if x.PrimaryReason != `waiting for "output/mid.pt" from wj301 (queued)` {
		t.Fatalf("PrimaryReason = %q", x.PrimaryReason)
	}
	root, ok := evidenceValue(x, "root wait")
	if !ok {
		t.Fatalf("missing root wait evidence: %+v", x.Evidence)
	}
	if !strings.Contains(root, `waiting for "output/root.pt" from wj300 (running)`) {
		t.Fatalf("root wait = %q", root)
	}
	if x.SuggestedAction != "inspect root producer" {
		t.Fatalf("SuggestedAction = %q", x.SuggestedAction)
	}
}

func evidenceValue(x Explanation, label string) (string, bool) {
	for _, ev := range x.Evidence {
		if ev.Label == label {
			return ev.Value, true
		}
	}
	return "", false
}

func TestForJobShowsReusePlacement(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := db.RecordPlacementDecision(database, db.PlacementDecision{
		JobID:          &jobID,
		DecisionKind:   "acted",
		Operation:      "autopilot_reuse",
		SelectedKind:   "reuse-instance",
		SelectedTarget: "wi4424",
		Details: db.PlacementDecisionDetails{
			Instance:        "wi4424",
			GPU:             "NVIDIA 48GB",
			ColocatedBehind: 1,
			Why:             "reused running instance (queued behind 1 job(s) on its GPU)",
		},
	}, nil); err != nil {
		t.Fatalf("RecordPlacementDecision: %v", err)
	}

	launchID := int64(4424)
	job := &db.Job{ID: jobID, Status: db.StatusQueued, LaunchID: &launchID}
	x := ForJob(database, job, time.Unix(20_000, 0))

	placed, ok := evidenceValue(x, "placed")
	if !ok {
		t.Fatalf("expected a 'placed' evidence line, got %+v", x.Evidence)
	}
	if !strings.Contains(placed, "reused wi4424") || !strings.Contains(placed, "queued behind 1") {
		t.Fatalf("placed evidence = %q", placed)
	}
}

func TestForJobPlacementEvidenceSkippedWithoutDB(t *testing.T) {
	launchID := int64(4424)
	job := &db.Job{ID: 3390, Status: db.StatusQueued, LaunchID: &launchID}
	// Nil database is the TUI render path; it must not query and must not panic.
	x := ForJob(nil, job, time.Unix(20_000, 0))
	if _, ok := evidenceValue(x, "placed"); ok {
		t.Fatalf("placed evidence must not appear when database is nil: %+v", x.Evidence)
	}
}

func TestForJobAutoReplanUsesStartOfDispatchBlockStreak(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	for _, at := range []time.Time{now.Add(-20 * time.Minute), now.Add(-12 * time.Minute), now.Add(-3 * time.Minute)} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchDeferred,
			JobID:      jobID,
			Detail:     "source sync deferred (host unreachable): ssh timeout",
		}); err != nil {
			t.Fatalf("InsertLifecycleEvent: %v", err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, now)
	if !x.AutoReplanAllowed {
		t.Fatalf("AutoReplanAllowed = false, want true; suggested=%q", x.SuggestedAction)
	}
	if !strings.Contains(x.SuggestedAction, "replan") {
		t.Fatalf("SuggestedAction = %q, want replan", x.SuggestedAction)
	}
}

func TestLatestInventoryDispatchBlockClearedByOK(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-10 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		Detail:     "source sync failed",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent failed: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-5 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchOK,
		JobID:      jobID,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent ok: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if _, ok := LatestInventoryDispatchBlock(database, job, now); ok {
		t.Fatal("LatestInventoryDispatchBlock ok = true, want false after dispatch ok")
	}
}

func TestLatestInventoryDispatchBlockR2PreflightFailureSurvivesOK(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-1*time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	detail := `r2_isolated_source_fetch_failed: download tarball: exec: "rclone": executable file not found in $PATH`
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-10 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		Detail:     detail,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent failed: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-5 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchOK,
		JobID:      jobID,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent ok: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	block, ok := LatestInventoryDispatchBlock(database, job, now)
	if !ok {
		t.Fatal("LatestInventoryDispatchBlock ok = false, want R2 preflight blocker despite dispatch ok")
	}
	if block.Detail != detail {
		t.Fatalf("block.Detail = %q, want %q", block.Detail, detail)
	}
}
