package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

func TestGetLaunchJobsIncludingAttempts(t *testing.T) {
	database := setupTestDB(t)

	// Create a cloud instance
	ci := &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Insert a job associated with the cloud instance
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Verify both functions return the job before reset
	jobs, err := GetLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("GetLaunchJobs before reset: got %d jobs, want 1", len(jobs))
	}

	jobsIncl, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobsIncl) != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts before reset: got %d jobs, want 1", len(jobsIncl))
	}

	// Reset the instance's jobs (simulates instance failure)
	n, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetLaunchJobs: reset %d jobs, want 1", n)
	}

	// GetLaunchJobs should return 0 jobs (cloud_instance_id was cleared)
	jobs, err = GetLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobs after reset: %v", err)
	}

	// GetLaunchJobsIncludingAttempts should still return 1 job
	jobsIncl, err = GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts after reset: %v", err)
	}
	if len(jobsIncl) != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts after reset: got %d jobs, want 1", len(jobsIncl))
	}
	if jobsIncl[0].ID != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts: got job ID %d, want 1", jobsIncl[0].ID)
	}
}

func TestGetLaunchJobsIncludingAttemptsTreatsOpenAttemptsAsCurrent(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 203, "open attempt current", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 203, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(203): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts: got %d jobs, want 1", len(jobs))
	}
	if jobs[0].ID != 203 {
		t.Fatalf("job ID = %d, want 203", jobs[0].ID)
	}
	if jobs[0].LaunchID == nil || *jobs[0].LaunchID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d", jobs[0].LaunchID, instanceID)
	}
}

func TestSetLaunchLiveInstancePhasePreservesBootstrapState(t *testing.T) {
	database := setupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := UpsertLaunchLiveState(database, LaunchLiveState{
		LaunchID:       launchID,
		BootstrapStage: "agent_installed",
		AgentVersion:   "abc123",
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	changedAt, err := SetLaunchLiveInstancePhase(database, launchID, "waiting for RunPod SSH readiness")
	if err != nil {
		t.Fatalf("SetLaunchLiveInstancePhase: %v", err)
	}
	if changedAt == nil {
		t.Fatal("phase_changed_at should be set on first phase observation")
	}

	state, err := GetLaunchLiveState(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if state.InstancePhase != "waiting for RunPod SSH readiness" {
		t.Fatalf("InstancePhase = %q", state.InstancePhase)
	}
	if state.BootstrapStage != "agent_installed" {
		t.Fatalf("BootstrapStage = %q, want preserved stage", state.BootstrapStage)
	}
	if state.AgentVersion != "abc123" {
		t.Fatalf("AgentVersion = %q, want preserved version", state.AgentVersion)
	}
}

func TestUpdateLaunchOfferMetadata(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:            LaunchStatusPlanned,
		Provider:          "vastai",
		GPUSpec:           "RTX_4090",
		ResolvedGPUName:   "RTX_4090",
		CostPerHourCents:  100,
		NumGPUs:           1,
		DLPerf:            10,
		Reliability:       0.98,
		InetDownMbps:      100,
		InetUpMbps:        50,
		CUDAVersion:       12.1,
		DiskGB:            80,
		ProvisionedInputs: []string{"hf:test/model"},
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	replacement := cloud.Offer{
		GPUName:           "RTX_5090",
		CostPerHour:       1.24,
		NumGPUs:           2,
		GPUMemGB:          32,
		DLPerf:            42,
		Reliability:       0.995,
		DownloadBandwidth: 500,
		UploadBandwidth:   200,
		CUDAVersion:       12.4,
		DiskSpaceGB:       160,
	}
	if err := UpdateLaunchOfferMetadata(database, instanceID, replacement); err != nil {
		t.Fatalf("UpdateLaunchOfferMetadata: %v", err)
	}

	inst, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if inst.ResolvedGPUName != "RTX_5090" || inst.CostPerHourCents != 124 || inst.NumGPUs != 2 {
		t.Fatalf("updated instance metadata = %+v", inst)
	}
	if inst.DiskGB != 160 || inst.CUDAVersion != 12.4 || inst.InetDownMbps != 500 || inst.InetUpMbps != 200 {
		t.Fatalf("updated network/disk metadata = %+v", inst)
	}
	if inst.GPUMemGB != 32 {
		t.Fatalf("updated gpu_mem_gb = %d, want 32", inst.GPUMemGB)
	}
}

// TestJobStatusView_CanceledAttemptOnLiveLaunch verifies that an attempt
// closed with status='canceled' on a launch that's still running surfaces as
// 'canceled' rather than the previous 'dead' fallthrough.
func TestJobStatusView_CanceledAttemptOnLiveLaunch(t *testing.T) {
	database := setupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	const jobID = int64(7100)
	insertTestJob(t, database, jobID, "echo hi", "/tmp", StatusQueued, withLaunch(launchID))

	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ?`,
		StatusCanceled, time.Now().Unix(), jobID,
	); err != nil {
		t.Fatalf("close attempt as canceled: %v", err)
	}

	var derived string
	if err := database.QueryRow(`SELECT status FROM job_status WHERE id = ?`, jobID).Scan(&derived); err != nil {
		t.Fatalf("read job_status: %v", err)
	}
	if derived != StatusCanceled {
		t.Errorf("derived status = %q, want %q (regression: previously fell through to 'dead')", derived, StatusCanceled)
	}
}

// TestCleanupStaleAttempts_StampsCloudOutcomeOnSupersededDuplicate verifies
// the wj1483 fix: when cleanupStaleAttempts closes a duplicate open cloud
// attempt as 'canceled', it also records cloud_outcome='superseded' so the
// view's cloud-job branch doesn't strand the job as 'dead'.
func TestCleanupStaleAttempts_StampsCloudOutcomeOnSupersededDuplicate(t *testing.T) {
	database := setupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	const jobID = int64(7101)
	insertTestJob(t, database, jobID, "echo hi", "/tmp", StatusQueued, withLaunch(launchID))

	// Create a second open attempt on the same job + launch, simulating the
	// duplicate-open-attempt state that triggers cleanupStaleAttempts.
	if _, err := database.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at)
		 VALUES (?, 2, '', ?, ?, ?)`,
		jobID, launchID, StatusQueued, time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert duplicate attempt: %v", err)
	}

	if err := cleanupStaleAttempts(database); err != nil {
		t.Fatalf("cleanupStaleAttempts: %v", err)
	}

	var outcome sql.NullString
	if err := database.QueryRow(
		`SELECT cloud_outcome FROM job_attempts WHERE job_id = ? AND attempt_number = 1`,
		jobID,
	).Scan(&outcome); err != nil {
		t.Fatalf("read cloud_outcome on superseded attempt: %v", err)
	}
	if !outcome.Valid || outcome.String != AttemptOutcomeSuperseded {
		t.Errorf("superseded attempt cloud_outcome = %v, want %q (regression: wj1483 wedge)", outcome, AttemptOutcomeSuperseded)
	}
}

func TestTerminalLaunchJobDisposition_PreemptedYieldsPreemptedOutcome(t *testing.T) {
	reset, outcome := terminalLaunchJobDisposition(&Launch{
		Status:            LaunchStatusFailed,
		TerminationReason: TerminationReasonPreempted,
	})
	if !reset {
		t.Errorf("preempted launch should reset jobs (retryable)")
	}
	if outcome != AttemptOutcomePreempted {
		t.Errorf("outcome = %q, want %q", outcome, AttemptOutcomePreempted)
	}
}

func TestSetJobLaunchID_LinksPredecessorAttempt(t *testing.T) {
	database := setupTestDB(t)

	firstLaunch, err := CreateLaunch(database, &Launch{Status: LaunchStatusPlanned, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	const jobID = int64(7001)
	insertTestJob(t, database, jobID, "echo hi", "/tmp", StatusQueued, withLaunch(firstLaunch))

	var firstAttemptID int64
	if err := database.QueryRow(`SELECT id FROM job_attempts WHERE job_id = ? AND launch_id = ?`, jobID, firstLaunch).Scan(&firstAttemptID); err != nil {
		t.Fatalf("read first attempt id: %v", err)
	}

	// Close the first attempt and fail the first launch as preempted so the
	// new SetJobLaunchID accepts the reassignment to a fresh launch.
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = ?, cloud_outcome = ? WHERE id = ?`,
		time.Now().Unix(), AttemptOutcomePreempted, firstAttemptID); err != nil {
		t.Fatalf("close first attempt: %v", err)
	}
	if err := UpdateLaunchStatus(database, firstLaunch, LaunchStatusFailed, TerminationReasonPreempted); err != nil {
		t.Fatalf("fail first launch: %v", err)
	}

	secondLaunch, err := CreateLaunch(database, &Launch{Status: LaunchStatusPlanned, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch second: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, secondLaunch); err != nil {
		t.Fatalf("second SetJobLaunchID: %v", err)
	}

	var predecessorID sql.NullInt64
	if err := database.QueryRow(
		`SELECT predecessor_attempt_id FROM job_attempts WHERE job_id = ? AND launch_id = ?`,
		jobID, secondLaunch).Scan(&predecessorID); err != nil {
		t.Fatalf("read second attempt: %v", err)
	}
	if !predecessorID.Valid || predecessorID.Int64 != firstAttemptID {
		t.Errorf("predecessor_attempt_id = %v, want %d", predecessorID, firstAttemptID)
	}
}

func TestLaunchPausedSeconds(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	now := time.Now()
	// Sequence: running -> stopped (60s) -> running -> stopped (still stopped at ref).
	steps := []struct {
		old, new string
		offset   time.Duration
	}{
		{"created", "running", -300 * time.Second},
		{"running", "stopped", -240 * time.Second},
		{"stopped", "running", -180 * time.Second},
		{"running", "stopped", -60 * time.Second},
	}
	for _, s := range steps {
		if err := RecordProviderStatus(database, id, now.Add(s.offset), s.old, s.new); err != nil {
			t.Fatalf("RecordProviderStatus: %v", err)
		}
	}
	// Closed stop interval: 60s. Open stop interval extends to now: 60s.
	got, err := LaunchPausedSeconds(database, id, now)
	if err != nil {
		t.Fatalf("LaunchPausedSeconds: %v", err)
	}
	if got < 110 || got > 130 {
		t.Errorf("paused seconds = %d, want ~120 (60+60)", got)
	}
}

func TestLastProviderStatusTransitionTime(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// No transitions yet → nil.
	got, err := LastProviderStatusTransitionTime(database, id)
	if err != nil {
		t.Fatalf("LastProviderStatusTransitionTime: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for launch with no transitions", got)
	}

	// One transition → its observed_at.
	now := time.Now()
	t1 := now.Add(-5 * time.Minute)
	if err := RecordProviderStatus(database, id, t1, "created", "running"); err != nil {
		t.Fatalf("RecordProviderStatus: %v", err)
	}
	got, err = LastProviderStatusTransitionTime(database, id)
	if err != nil {
		t.Fatalf("LastProviderStatusTransitionTime: %v", err)
	}
	if got == nil || got.Unix() != t1.Unix() {
		t.Errorf("got %v, want %v", got, t1)
	}

	// Two transitions → most recent.
	t2 := now.Add(-1 * time.Minute)
	if err := RecordProviderStatus(database, id, t2, "running", "offline"); err != nil {
		t.Fatalf("RecordProviderStatus: %v", err)
	}
	got, err = LastProviderStatusTransitionTime(database, id)
	if err != nil {
		t.Fatalf("LastProviderStatusTransitionTime: %v", err)
	}
	if got == nil || got.Unix() != t2.Unix() {
		t.Errorf("got %v, want %v (most recent)", got, t2)
	}
}

func TestValidateReservedPlacementTags_BenchmarkPreemptibleRejected(t *testing.T) {
	err := validateReservedPlacementTags([]string{TagBenchmark, TagPreemptible})
	if err == nil {
		t.Fatal("expected error for benchmark + preemptible, got nil")
	}
	// Each tag alone is fine.
	if err := validateReservedPlacementTags([]string{TagBenchmark}); err != nil {
		t.Errorf("benchmark alone should be valid: %v", err)
	}
	if err := validateReservedPlacementTags([]string{TagPreemptible}); err != nil {
		t.Errorf("preemptible alone should be valid: %v", err)
	}
}

func TestCanonicalizeTag_PreemptibleAlias(t *testing.T) {
	if got := CanonicalizeTag("preemptible"); got != TagInterruptible {
		t.Errorf("CanonicalizeTag(\"preemptible\") = %q, want %q", got, TagInterruptible)
	}
	if got := CanonicalizeTag("interruptible"); got != TagInterruptible {
		t.Errorf("CanonicalizeTag(\"interruptible\") = %q, want %q", got, TagInterruptible)
	}
	if !HasPreemptibleTag([]string{"preemptible"}) {
		t.Error("legacy 'preemptible' tag should canonicalize to interruptible")
	}
	if !HasPreemptibleTag([]string{"interruptible"}) {
		t.Error("'interruptible' tag should be detected")
	}
}

func TestCreateLaunch_RentalTypeMetadata_RoundTrip(t *testing.T) {
	database := setupTestDB(t)

	bid := 145
	ref := 210
	instanceID, err := CreateLaunch(database, &Launch{
		Status:           LaunchStatusPlanned,
		Provider:         "vastai",
		GPUSpec:          "RTX_4090",
		CostPerHourCents: 145,
		InstanceType:     "interruptible",
		MaxBidPriceCents: &bid,
		OnDemandRefCents: &ref,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	got, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if got.InstanceType != "interruptible" {
		t.Errorf("InstanceType = %q, want interruptible", got.InstanceType)
	}
	if got.MaxBidPriceCents == nil || *got.MaxBidPriceCents != bid {
		t.Errorf("MaxBidPriceCents = %v, want %d", got.MaxBidPriceCents, bid)
	}
	if got.OnDemandRefCents == nil || *got.OnDemandRefCents != ref {
		t.Errorf("OnDemandRefCents = %v, want %d", got.OnDemandRefCents, ref)
	}

	// On-demand launch: no bid, no counterfactual.
	onDemandID, err := CreateLaunch(database, &Launch{
		Status:       LaunchStatusPlanned,
		Provider:     "vastai",
		GPUSpec:      "A100",
		InstanceType: "on-demand",
	})
	if err != nil {
		t.Fatalf("CreateLaunch on-demand: %v", err)
	}
	od, err := GetLaunch(database, onDemandID)
	if err != nil {
		t.Fatalf("GetLaunch on-demand: %v", err)
	}
	if od.InstanceType != "on-demand" {
		t.Errorf("on-demand InstanceType = %q", od.InstanceType)
	}
	if od.MaxBidPriceCents != nil {
		t.Errorf("on-demand MaxBidPriceCents = %v, want nil", od.MaxBidPriceCents)
	}
	if od.OnDemandRefCents != nil {
		t.Errorf("on-demand OnDemandRefCents = %v, want nil", od.OnDemandRefCents)
	}
}

func TestCreateLaunch_CPUMetadataRoundTrip(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
		CPUCores: 24,
		CPUName:  "AMD EPYC 7763",
		RAMGB:    128,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	got, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if got.CPUCores != 24 {
		t.Fatalf("CPUCores = %d, want 24", got.CPUCores)
	}
	if got.CPUName != "AMD EPYC 7763" {
		t.Fatalf("CPUName = %q, want AMD EPYC 7763", got.CPUName)
	}
	if got.RAMGB != 128 {
		t.Fatalf("RAMGB = %d, want 128", got.RAMGB)
	}
}

func TestGetLaunchJobsIncludingAttemptsSortsByCampaignIndex(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 249, "historical", "/tmp", StatusQueued)
	insertTestJob(t, database, 203, "open attempt current", "/tmp", StatusQueued)
	insertTestJob(t, database, 199, "current earlier campaign slot", "/tmp", StatusQueued)

	if err := SetJobLaunchID(database, 249, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(249): %v", err)
	}
	if err := CloseLaunchAttempt(database, 249, AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt(249): %v", err)
	}
	if err := SetJobLaunchID(database, 203, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(203): %v", err)
	}
	if err := SetJobLaunchID(database, 199, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(199): %v", err)
	}
	// Mark job 199 as running after placement (simulates agent picking it up)
	if err := MarkQueuedJobRunning(database, 199); err != nil {
		t.Fatalf("MarkQueuedJobRunning(199): %v", err)
	}
	if err := SetJobCampaignIndex(database, 203, 1); err != nil {
		t.Fatalf("SetJobCampaignIndex(203): %v", err)
	}
	if err := SetJobCampaignIndex(database, 199, 0); err != nil {
		t.Fatalf("SetJobCampaignIndex(199): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("GetLaunchJobsIncludingAttempts: got %d jobs, want 3", len(jobs))
	}
	if jobs[0].ID != 199 || jobs[1].ID != 203 || jobs[2].ID != 249 {
		t.Fatalf("job order = [%d %d %d], want [199 203 249]", jobs[0].ID, jobs[1].ID, jobs[2].ID)
	}
}

// TestGetLaunchJobsIncludingAttemptsRetainsOrderAfterFailure verifies that
// a failed (historical) job keeps its campaign position instead of moving after
// still-running jobs.
func TestGetLaunchJobsIncludingAttemptsRetainsOrderAfterFailure(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Job 292 (campaign index 0) failed and was reset — cloud_instance_id cleared.
	// Job 293 (campaign index 1) is still running.
	insertTestJob(t, database, 292, "echo first", "/tmp", StatusQueued)
	insertTestJob(t, database, 293, "echo second", "/tmp", StatusQueued)
	if err := SetJobCampaignIndex(database, 292, 0); err != nil {
		t.Fatalf("SetJobCampaignIndex(292): %v", err)
	}
	if err := SetJobCampaignIndex(database, 293, 1); err != nil {
		t.Fatalf("SetJobCampaignIndex(293): %v", err)
	}

	// Place both jobs, then mark 292 as failed (simulates run + failure).
	if err := SetJobLaunchID(database, 292, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(292): %v", err)
	}
	if err := CloseLaunchAttempt(database, 292, AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt(292): %v", err)
	}
	if err := SetJobLaunchID(database, 293, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(293): %v", err)
	}
	// Mark 293 as running (simulates agent picking it up)
	if err := MarkQueuedJobRunning(database, 293); err != nil {
		t.Fatalf("MarkQueuedJobRunning(293): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].ID != 292 || jobs[1].ID != 293 {
		t.Fatalf("job order = [%d %d], want [292 293]", jobs[0].ID, jobs[1].ID)
	}
}

// TestGetLaunchJobsIncludingAttemptsOverridesStatusForHistorical verifies
// that after a job is reset (orphaned), its display status reflects the attempt
// outcome rather than the current global status.
func TestGetLaunchJobsIncludingAttemptsOverridesStatusForHistorical(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Insert a job, place it on the instance, mark running, then reset (simulating instance failure).
	insertTestJob(t, database, 317, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 317, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := MarkQueuedJobRunning(database, 317); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	// Simulate ResetLaunchJobs: close attempt as orphaned, clear instance assignment, reset to queued.
	n, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}

	// Verify the job's global status is now "queued".
	job, err := GetJobByID(database, 317)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("global status = %q, want %q", job.Status, StatusQueued)
	}

	// GetLaunchJobsIncludingAttempts should preserve "queued" status for
	// orphaned jobs — AttemptDisplayStatus() uses the attempt outcome for display.
	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Status != StatusQueued {
		t.Fatalf("display status = %q, want %q", jobs[0].Status, StatusQueued)
	}
}

func TestGetLaunchJobsIncludingAttemptsUsesHistoricalAttemptTiming(t *testing.T) {
	database := setupTestDB(t)

	firstInstanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(first): %v", err)
	}
	secondInstanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(second): %v", err)
	}

	insertTestJob(t, database, 401, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 401, firstInstanceID); err != nil {
		t.Fatalf("SetJobLaunchID(first): %v", err)
	}
	if err := MarkQueuedJobRunning(database, 401); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	var historicalEnd sql.NullInt64
	if _, err := ResetLaunchJobs(database, firstInstanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs(first): %v", err)
	}
	if err := database.QueryRow(
		`SELECT end_time FROM job_attempts WHERE job_id = ? AND launch_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		401, firstInstanceID,
	).Scan(&historicalEnd); err != nil {
		t.Fatalf("query historical end_time: %v", err)
	}
	if !historicalEnd.Valid || historicalEnd.Int64 == 0 {
		t.Fatalf("historical end_time = %v, want non-zero", historicalEnd)
	}

	if err := SetJobLaunchID(database, 401, secondInstanceID); err != nil {
		t.Fatalf("SetJobLaunchID(second): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, firstInstanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].EndTime == nil {
		t.Fatal("EndTime = nil, want historical attempt end time")
	}
	if *jobs[0].EndTime != historicalEnd.Int64 {
		t.Fatalf("EndTime = %d, want %d", *jobs[0].EndTime, historicalEnd.Int64)
	}
}

func TestResetOrphanedCloudJobsSetsPlacementReasons(t *testing.T) {
	database := setupTestDB(t)

	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusRunning, withHost("vastai:77"))

	n, err := ResetOrphanedCloudJobs(database)
	if err != nil {
		t.Fatalf("ResetOrphanedCloudJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetOrphanedCloudJobs reset %d jobs, want 1", n)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "" || job.Status != StatusQueued {
		t.Fatalf("job = %+v, want queued unplaced", job)
	}
	if got := strings.Join(job.PlacementReasons, "\n"); got != "cloud instance 77 no longer active; job returned to unplaced queue" {
		t.Fatalf("PlacementReasons = %v", job.PlacementReasons)
	}
}

func TestGetAttemptOutcomesByLaunch(t *testing.T) {
	database := setupTestDB(t)

	// Create a cloud instance and a job
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Before closing the attempt, outcomes should be empty
	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("expected 0 outcomes before closing, got %d", len(outcomes))
	}

	// Close the attempt as orphaned (simulates instance failure + reset)
	if err := CloseLaunchAttempts(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("CloseLaunchAttempts: %v", err)
	}

	outcomes, err = GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch after close: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(outcomes))
	}
	if outcomes[1] != AttemptOutcomeOrphaned {
		t.Fatalf("expected outcome %q, got %q", AttemptOutcomeOrphaned, outcomes[1])
	}
}

func TestGetAttemptOutcomesByLaunch_NewerOpenAttemptHidesOlderOutcome(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)

	// First attempt on this launch; mark it superseded as if it was closed
	// and replaced (the SetJobLaunchID code path marks prior closed attempts
	// on any launch as superseded).
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("set cloud instance (first): %v", err)
	}
	// Close the first attempt and mark it superseded directly.
	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET end_time = ?, cloud_outcome = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		now, AttemptOutcomeSuperseded, 1, instanceID,
	); err != nil {
		t.Fatalf("mark superseded: %v", err)
	}
	// Re-attach on the same launch; this creates a new open attempt.
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("set cloud instance (second): %v", err)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if got, ok := outcomes[1]; ok {
		t.Fatalf("expected no outcome for job with open latest attempt, got %q", got)
	}
}

func TestCloseLaunchAttempts_CompletedSetsExitCode(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1, "echo done", "/tmp", StatusRunning, withLaunch(instanceID))

	if err := CloseLaunchAttempts(database, instanceID, AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseLaunchAttempts(completed): %v", err)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, StatusCompleted)
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("job exit_code = %v, want 0", job.ExitCode)
	}

	var lastSynced string
	if err := database.QueryRow(
		`SELECT COALESCE(last_synced_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		1,
	).Scan(&lastSynced); err != nil {
		t.Fatalf("query last_synced_status: %v", err)
	}
	if lastSynced != StatusCompleted {
		t.Fatalf("last_synced_status = %q, want %q", lastSynced, StatusCompleted)
	}
}

func TestCloseLaunchAttempts_EndTimeZero(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1, "echo done", "/tmp", StatusRunning, withLaunch(instanceID))

	// Simulate the bug: set end_time=0 (not NULL) as RecordCloudJobCompletion used to do
	if _, err := database.Exec(
		`UPDATE job_attempts SET end_time = 0 WHERE job_id = 1`,
	); err != nil {
		t.Fatalf("set end_time=0: %v", err)
	}

	if err := CloseLaunchAttempts(database, instanceID, AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseLaunchAttempts: %v", err)
	}

	var endTime int64
	if err := database.QueryRow(
		`SELECT end_time FROM job_attempts WHERE job_id = 1 ORDER BY attempt_number DESC LIMIT 1`,
	).Scan(&endTime); err != nil {
		t.Fatalf("query end_time: %v", err)
	}
	if endTime == 0 {
		t.Fatal("end_time still 0 after CloseLaunchAttempts — WHERE clause missed end_time=0")
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, StatusCompleted)
	}
}

func TestResetLaunchJobs_PreservesCanceledJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 1, "echo canceled", "/tmp", StatusCanceled, withLaunch(instanceID))
	insertTestJob(t, database, 2, "echo running", "/tmp", StatusRunning, withLaunch(instanceID))

	n, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}

	canceledJob, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID(canceled): %v", err)
	}
	if canceledJob.Status != StatusCanceled {
		t.Fatalf("canceled job status = %q, want %q", canceledJob.Status, StatusCanceled)
	}
	if canceledJob.LaunchID == nil || *canceledJob.LaunchID != instanceID {
		t.Fatalf("canceled job cloud_instance_id = %v, want %d", canceledJob.LaunchID, instanceID)
	}
	if canceledJob.Host != "" {
		t.Fatalf("canceled job host = %q, want empty", canceledJob.Host)
	}
	// The durable intent lives on jobs.requested_status; closeAttemptsAndRequeue
	// must not overwrite it. See CancelSurvivesInstanceTermination.
	var requested sql.NullString
	if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = 1`).Scan(&requested); err != nil {
		t.Fatalf("read requested_status: %v", err)
	}
	if !requested.Valid || requested.String != StatusCanceled {
		t.Fatalf("canceled job requested_status = %q (valid=%v), want %q",
			requested.String, requested.Valid, StatusCanceled)
	}

	resetJob, err := GetJobByID(database, 2)
	if err != nil {
		t.Fatalf("GetJobByID(reset): %v", err)
	}
	if resetJob.Status != StatusQueued {
		t.Fatalf("reset job status = %q, want %q", resetJob.Status, StatusQueued)
	}
	// After schema refactor: the attempt retains its cloud_instance_id.
	// The job_status view shows the latest attempt's cloud_instance_id.
	if resetJob.Host != "" {
		t.Fatalf("reset job host = %q, want empty", resetJob.Host)
	}
}

// TestResetLaunchJobs_CancelWithOpenAttempt reproduces the production bug
// (2026-04-24, wj1383/wj1386/wj1387 on structural-probes): a job is cancelled
// while placed on a rental with an open (non-terminal) attempt; terminating
// the instance must not flip requested_status back to 'queued' and re-expose
// the job to the dispatcher.
func TestResetLaunchJobs_CancelWithOpenAttempt(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Job is placed on the instance with an OPEN attempt (status=running, no end_time).
	insertTestJob(t, database, 1383, "echo placed", "/tmp", StatusRunning, withLaunch(instanceID))

	// Simulate `weft job cancel wj1383`: user cancel while attempt is still open.
	// KillOrCancelCloudJob sets requested_status='canceled' but does not itself
	// close the attempt (the agent does that asynchronously).
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusCanceled, 1383); err != nil {
		t.Fatalf("set requested_status: %v", err)
	}

	// Simulate `weft instance terminate wi<instanceID>`.
	if _, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}

	var requested sql.NullString
	if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, 1383).Scan(&requested); err != nil {
		t.Fatalf("read requested_status: %v", err)
	}
	if !requested.Valid || requested.String != StatusCanceled {
		t.Fatalf("after terminate: requested_status = %q (valid=%v), want %q",
			requested.String, requested.Valid, StatusCanceled)
	}

	job, err := GetJobByID(database, 1383)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCanceled {
		t.Fatalf("after terminate: job.Status = %q, want %q", job.Status, StatusCanceled)
	}
}

func TestResetLaunchJobs_DoesNotRewriteCompletedAttempts(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	for _, stmt := range []struct {
		id     int64
		status string
	}{
		{id: 1, status: StatusCompleted},
		{id: 2, status: StatusRunning},
	} {
		insertTestJob(t, database, stmt.id, "echo test", "/tmp", stmt.status, withLaunch(instanceID))
	}
	if err := CloseLaunchAttempt(database, 1, AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseLaunchAttempt(completed): %v", err)
	}

	if _, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1] != AttemptOutcomeCompleted {
		t.Fatalf("completed job outcome = %q, want %q", outcomes[1], AttemptOutcomeCompleted)
	}
	if outcomes[2] != AttemptOutcomeOrphaned {
		t.Fatalf("reset job outcome = %q, want %q", outcomes[2], AttemptOutcomeOrphaned)
	}
}

func TestResetLaunchJobs_RestoresOpenNoStartMoveTargetToLiveSource(t *testing.T) {
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 1201, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1201, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          1201,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		TargetGPUName:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 1201, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}

	n, err := ResetLaunchJobs(database, dst, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}

	job, err := GetJobByID(database, 1201)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, StatusQueued)
	}
	if job.LaunchID == nil || *job.LaunchID != src {
		t.Fatalf("job launch_id = %v, want source %d", job.LaunchID, src)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateCanceled {
		t.Fatalf("move intent state = %q, want %q", gotIntent.State, MoveIntentStateCanceled)
	}
	outcomes, err := GetAttemptOutcomesByLaunch(database, dst)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1201] != AttemptOutcomeOrphaned {
		t.Fatalf("target attempt outcome = %q, want %q", outcomes[1201], AttemptOutcomeOrphaned)
	}
}

func TestResetLaunchJobs_RestoresRetryableNoStartMoveTargetAndKeepsIntentOpen(t *testing.T) {
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 1204, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1204, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          1204,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		TargetGPUName:  "A100",
		AttemptCount:   1,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 1204, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}

	n, err := ResetLaunchJobs(database, dst, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}
	job, err := GetJobByID(database, 1204)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != src {
		t.Fatalf("job launch_id = %v, want source %d", job.LaunchID, src)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateOpen {
		t.Fatalf("move intent state = %q, want open", gotIntent.State)
	}
	if gotIntent.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 until replacement launch registers", gotIntent.AttemptCount)
	}
}

func TestHandleMoveTargetFailedBeforeStart_ConsumesStaleClosedTarget(t *testing.T) {
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 1205, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1205, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          1205,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		TargetGPUName:  "A100",
		AttemptCount:   1,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 1205, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		StatusCanceled, now, AttemptOutcomeOrphaned, 1205, dst,
	); err != nil {
		t.Fatalf("close target attempt: %v", err)
	}
	if _, err := CreateAttempt(database, 1205, "", &src, StatusQueued); err != nil {
		t.Fatalf("restore source attempt: %v", err)
	}

	transition, err := HandleMoveTargetFailedBeforeStart(database, 1205, dst, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("HandleMoveTargetFailedBeforeStart: %v", err)
	}
	if !transition.Handled {
		t.Fatal("Handled = false, want true")
	}
	if !transition.Retryable || transition.Exhausted {
		t.Fatalf("transition retryable/exhausted = %t/%t, want true/false", transition.Retryable, transition.Exhausted)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateOpen {
		t.Fatalf("move intent state = %q, want open", gotIntent.State)
	}
	var openSource int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		1205, src,
	).Scan(&openSource); err != nil {
		t.Fatalf("count source attempts: %v", err)
	}
	if openSource != 1 {
		t.Fatalf("open source attempts = %d, want 1", openSource)
	}
}

func TestFailedMoveTargetMachineIDs_ReturnsNoStartFailures(t *testing.T) {
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dstFailed, err := CreateLaunch(database, &Launch{
		Status:            LaunchStatusFailed,
		Provider:          "vastai",
		MachineID:         "machine-bad",
		TerminationReason: TerminationReasonInfraFailure,
	})
	if err != nil {
		t.Fatalf("CreateLaunch failed target: %v", err)
	}
	dstStarted, err := CreateLaunch(database, &Launch{
		Status:            LaunchStatusFailed,
		Provider:          "vastai",
		MachineID:         "machine-started",
		TerminationReason: TerminationReasonInfraFailure,
	})
	if err != nil {
		t.Fatalf("CreateLaunch started target: %v", err)
	}
	jobID := int64(1206)
	insertTestJob(t, database, jobID, "move target machines", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          jobID,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, jobID, dstFailed); err != nil {
		t.Fatalf("TransferJobLaunchID failed target: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		StatusCanceled, time.Now().Unix(), AttemptOutcomeOrphaned, jobID, dstFailed,
	); err != nil {
		t.Fatalf("close failed target attempt: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("restore source: %v", err)
	}
	if err := TransferJobLaunchID(database, jobID, dstStarted); err != nil {
		t.Fatalf("TransferJobLaunchID started target: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		StatusCanceled, time.Now().Unix()-10, time.Now().Unix(), AttemptOutcomeOrphaned, jobID, dstStarted,
	); err != nil {
		t.Fatalf("close started target attempt: %v", err)
	}

	got, err := FailedMoveTargetMachineIDs(database, intent)
	if err != nil {
		t.Fatalf("FailedMoveTargetMachineIDs: %v", err)
	}
	if _, ok := got["machine-bad"]; !ok {
		t.Fatalf("machine-bad missing from failed machines: %#v", got)
	}
	if _, ok := got["machine-started"]; ok {
		t.Fatalf("started target should not be excluded as no-start failure: %#v", got)
	}
}

func TestResetLaunchJobs_DoesNotRestoreConfirmedNoStartMoveTarget(t *testing.T) {
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 1203, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1203, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          1203,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		TargetGPUName:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 1203, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateConfirmed, "new A100 instance"); err != nil {
		t.Fatalf("ResolveMoveIntent confirmed: %v", err)
	}

	if _, err := ResetLaunchJobs(database, dst, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	job, err := GetJobByID(database, 1203)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID != nil {
		t.Fatalf("job launch_id = %v, want unplaced so autopilot can resubmit to live capacity", job.LaunchID)
	}
}

func TestResetLaunchJobs_DoesNotRestoreMoveTargetAfterJobStarted(t *testing.T) {
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 1202, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1202, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	if _, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          1202,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		TargetGPUName:  "A100",
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 1202, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		time.Now().Unix(), 1202, dst,
	); err != nil {
		t.Fatalf("set start_time: %v", err)
	}

	if _, err := ResetLaunchJobs(database, dst, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	job, err := GetJobByID(database, 1202)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID != nil {
		t.Fatalf("job launch_id = %v, want unplaced after started target failed", job.LaunchID)
	}
}

func TestResetLaunchJobs_ArchivesPreviousRun(t *testing.T) {
	t.Skip("job_runs archival removed")
}

func TestGetActiveLaunchJobCounts_OnlyCountsNonTerminalJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	for _, stmt := range []struct {
		id     int64
		status string
	}{
		{id: 1, status: StatusQueued},
		{id: 2, status: StatusRunning},
		{id: 3, status: StatusCompleted},
		{id: 4, status: StatusFailed},
	} {
		insertTestJob(t, database, stmt.id, "echo test", "/tmp", stmt.status, withLaunch(instanceID))
	}

	counts, err := GetActiveLaunchJobCounts(database)
	if err != nil {
		t.Fatalf("GetActiveLaunchJobCounts: %v", err)
	}
	if counts[instanceID] != 2 {
		t.Fatalf("active count = %d, want 2", counts[instanceID])
	}
}

func TestNormalizeTerminalLaunchJobs_FailedInstanceOrphansRunningJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "echo running", "/tmp", StatusRunning, withLaunch(instanceID))

	n, err := NormalizeTerminalLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("NormalizeTerminalLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("NormalizeTerminalLaunchJobs reset %d jobs, want 1", n)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, StatusQueued)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1] != AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[1], AttemptOutcomeOrphaned)
	}
}

func TestNormalizeTerminalLaunchJobs_JobFailureClosesAttemptsAsFailed(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "echo running", "/tmp", StatusRunning, withLaunch(instanceID))

	n, err := NormalizeTerminalLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("NormalizeTerminalLaunchJobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("NormalizeTerminalLaunchJobs reset %d jobs, want 0", n)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusFailed {
		t.Fatalf("job status = %q, want %q", job.Status, StatusFailed)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("job launch_id = %v, want %d", job.LaunchID, instanceID)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1] != AttemptOutcomeFailed {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[1], AttemptOutcomeFailed)
	}
}

func TestResetJobsOnTerminalLaunches_SkipsCompletedInstances(t *testing.T) {
	database := setupTestDB(t)

	completedID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(completed): %v", err)
	}
	failedID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(failed): %v", err)
	}

	for _, tc := range []struct {
		jobID      int64
		instanceID int64
	}{
		{jobID: 1, instanceID: completedID},
		{jobID: 2, instanceID: failedID},
	} {
		insertTestJob(t, database, tc.jobID, "echo test", "/tmp", StatusRunning, withLaunch(tc.instanceID))
	}

	resetMap, err := ResetJobsOnTerminalLaunches(database)
	if err != nil {
		t.Fatalf("ResetJobsOnTerminalLaunches: %v", err)
	}
	if len(resetMap) != 1 {
		t.Fatalf("ResetJobsOnTerminalLaunches reset %d jobs, want 1", len(resetMap))
	}
	if instID, ok := resetMap[2]; !ok {
		t.Fatalf("resetMap missing job 2")
	} else if instID != failedID {
		t.Fatalf("resetMap[2] = %d, want %d (failedID)", instID, failedID)
	}

	completedJob, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID(completed): %v", err)
	}
	if completedJob.Status != StatusRunning {
		t.Fatalf("completed-instance job status = %q, want %q", completedJob.Status, StatusRunning)
	}

	failedJob, err := GetJobByID(database, 2)
	if err != nil {
		t.Fatalf("GetJobByID(failed): %v", err)
	}
	if failedJob.Status != StatusQueued {
		t.Fatalf("failed-instance job status = %q, want %q", failedJob.Status, StatusQueued)
	}
}

func TestResetJobsOnTerminalLaunches_JobFailureDoesNotMarkForRelaunch(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "echo test", "/tmp", StatusRunning, withLaunch(instanceID))

	resetMap, err := ResetJobsOnTerminalLaunches(database)
	if err != nil {
		t.Fatalf("ResetJobsOnTerminalLaunches: %v", err)
	}
	if len(resetMap) != 0 {
		t.Fatalf("ResetJobsOnTerminalLaunches reset %d jobs, want 0", len(resetMap))
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusFailed {
		t.Fatalf("job status = %q, want %q", job.Status, StatusFailed)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1] != AttemptOutcomeFailed {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[1], AttemptOutcomeFailed)
	}
}

func TestIsRetryableTermination(t *testing.T) {
	tests := []struct {
		name   string
		ci     *Launch
		expect bool
	}{
		{"nil instance", nil, false},
		{"running instance", &Launch{Status: LaunchStatusRunning}, false},
		{"completed", &Launch{Status: LaunchStatusCompleted}, false},
		{"canceled", &Launch{Status: LaunchStatusCancelled}, false},
		{"provider failure", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonProviderFailure}, true},
		{"infra failure", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonInfraFailure}, true},
		{"failed to launch (empty reason)", &Launch{Status: LaunchStatusFailed, TerminationReason: ""}, true},
		{"job failure", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonJobFailure}, false},
		{"disk full", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonDiskFull}, false},
		{"canceled reason", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonCancelled}, false},
		{"preempted", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonPreempted}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRetryableTermination(tt.ci)
			if got != tt.expect {
				t.Errorf("IsRetryableTermination() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestCountLaunchAttemptsInCampaign(t *testing.T) {
	database := setupTestDB(t)

	// Create two campaigns (simulating two separate `launch` commands)
	campaign1ID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign 1: %v", err)
	}
	campaign2ID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign 2: %v", err)
	}

	// Create a job
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "training", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	// Launch in campaign 1 (completes successfully)
	inst1ID, err := CreateLaunch(database, &Launch{
		Status:     LaunchStatusCompleted,
		Provider:   "vastai",
		GPUSpec:    "RTX_3090",
		CampaignID: &campaign1ID,
	})
	if err != nil {
		t.Fatalf("create instance 1: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, inst1ID); err != nil {
		t.Fatalf("set job launch 1: %v", err)
	}

	// Launch in campaign 2 (fails, orphaned)
	inst2ID, err := CreateLaunch(database, &Launch{
		Status:     LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_3090",
		CampaignID: &campaign2ID,
	})
	if err != nil {
		t.Fatalf("create instance 2: %v", err)
	}
	if _, err := CreateAttempt(database, jobID, "", nil, StatusQueued); err != nil {
		t.Fatalf("create attempt 2: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, inst2ID); err != nil {
		t.Fatalf("set job launch 2: %v", err)
	}

	// Superseded attempts are excluded from retry-budget counting.
	// The first campaign attempt is superseded by the second launch chain.
	total, err := CountLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("count all attempts: %v", err)
	}
	if total != 1 {
		t.Errorf("CountLaunchAttempts = %d, want 1", total)
	}

	// Campaign-scoped counts exclude superseded attempts as well.
	count1, err := CountLaunchAttemptsInCampaign(database, jobID, campaign1ID)
	if err != nil {
		t.Fatalf("count campaign 1 attempts: %v", err)
	}
	if count1 != 0 {
		t.Errorf("CountLaunchAttemptsInCampaign(campaign1) = %d, want 0", count1)
	}

	count2, err := CountLaunchAttemptsInCampaign(database, jobID, campaign2ID)
	if err != nil {
		t.Fatalf("count campaign 2 attempts: %v", err)
	}
	if count2 != 1 {
		t.Errorf("CountLaunchAttemptsInCampaign(campaign2) = %d, want 1", count2)
	}
}

func TestRefineInstanceTerminationReason_DiskFull(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "python train.py", "/tmp", StatusFailed,
		withLaunch(instanceID), withFailureReason(TerminationReasonDiskFull))

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.TerminationReason != TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonDiskFull)
	}
}

func TestRefineInstanceTerminationReason_OnlyRefinesJobFailure(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonInfraFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "python train.py", "/tmp", StatusFailed,
		withLaunch(instanceID), withFailureReason(TerminationReasonDiskFull))

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.TerminationReason != TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonInfraFailure)
	}
}

func TestRefineInstanceTerminationReason_UsesHistoricalAttempts(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	// Simulate the job running and failing with disk-full
	if err := CloseAttempt(database, 1, StatusFailed, nil, 1000); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET failure_reason = ? WHERE id = (
		SELECT id FROM job_attempts WHERE job_id = 1 ORDER BY attempt_number DESC LIMIT 1
	)`, TerminationReasonDiskFull)

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.TerminationReason != TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonDiskFull)
	}
}

func TestLaunchLiveState(t *testing.T) {
	database := setupTestDB(t)

	// Get returns nil for nonexistent row
	got, err := GetLaunchLiveState(database, 999)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}

	// Create a launch to reference
	ci := &Launch{Status: LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX3090"}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("InsertLaunch: %v", err)
	}

	// Upsert and read back
	state := LaunchLiveState{
		LaunchID:         instanceID,
		InstancePhase:    "running:42",
		HeartbeatJSON:    `{"ts":1234}`,
		HeartbeatTS:      1234,
		JobProgressPct:   75,
		JobProgressID:    42,
		JobProgressPhase: 3,
		AgentVersion:     "abc123",
	}
	if _, err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	got, err = GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if got.InstancePhase != "running:42" {
		t.Errorf("phase = %q, want %q", got.InstancePhase, "running:42")
	}
	if got.JobProgressPct != 75 {
		t.Errorf("progress = %d, want 75", got.JobProgressPct)
	}
	if got.JobProgressPhase != 3 {
		t.Errorf("progress_phase = %d, want 3", got.JobProgressPhase)
	}
	if got.AgentVersion != "abc123" {
		t.Errorf("agent_version = %q, want %q", got.AgentVersion, "abc123")
	}

	// Upsert overwrites
	state.InstancePhase = "running:43"
	state.JobProgressPct = -1
	if _, err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}
	got, err = GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got.InstancePhase != "running:43" {
		t.Errorf("phase = %q, want %q", got.InstancePhase, "running:43")
	}
	if got.JobProgressPct != -1 {
		t.Errorf("progress = %d, want -1", got.JobProgressPct)
	}

}

func TestLaunchLiveStateBootstrapTransitions(t *testing.T) {
	database := setupTestDB(t)

	ci := &Launch{Status: LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX3090"}
	launchID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	state := LaunchLiveState{
		LaunchID:       launchID,
		BootstrapStage: "deps_installing",
	}
	if _, err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}
	if _, err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState duplicate: %v", err)
	}
	state.BootstrapStage = "agent_starting"
	if _, err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState changed: %v", err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM bootstrap_transitions WHERE launch_id = ?`, launchID).Scan(&count); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if count != 2 {
		t.Fatalf("transition count = %d, want 2", count)
	}
	enteredAt, err := LatestBootstrapStageEnteredAt(database, launchID, "agent_starting")
	if err != nil {
		t.Fatalf("LatestBootstrapStageEnteredAt: %v", err)
	}
	if enteredAt <= 0 {
		t.Fatal("agent_starting entered_at was not recorded")
	}
}

func TestLaunchLiveStateDestroyingOrphansQueuedAttempts(t *testing.T) {
	database := setupTestDB(t)

	ci := &Launch{Status: LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX3090"}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = 1`, StatusQueued); err != nil {
		t.Fatalf("set requested status: %v", err)
	}
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	var openBefore int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = 1 AND launch_id = ? AND end_time IS NULL`, instanceID).Scan(&openBefore); err != nil {
		t.Fatalf("count open attempts before: %v", err)
	}
	if openBefore != 1 {
		t.Fatalf("open attempts before = %d, want 1", openBefore)
	}

	if _, err := UpsertLaunchLiveState(database, LaunchLiveState{
		LaunchID:      instanceID,
		InstancePhase: "destroying",
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		var req sql.NullString
		var latestOutcome sql.NullString
		_ = database.QueryRow(`SELECT requested_status FROM jobs WHERE id = 1`).Scan(&req)
		_ = database.QueryRow(`SELECT cloud_outcome FROM job_attempts WHERE job_id = 1 ORDER BY attempt_number DESC LIMIT 1`).Scan(&latestOutcome)
		t.Fatalf("job status = %q, want %q (requested_status=%q latest_outcome=%q)", job.Status, StatusQueued, req.String, latestOutcome.String)
	}
	if job.LaunchID != nil {
		t.Fatalf("job launch_id = %v, want nil", *job.LaunchID)
	}

	var closedStatus, closedOutcome string
	var closedEnd sql.NullInt64
	if err := database.QueryRow(`
		SELECT status, COALESCE(cloud_outcome, ''), end_time
		FROM job_attempts
		WHERE job_id = 1
		ORDER BY attempt_number DESC
		LIMIT 1`).Scan(&closedStatus, &closedOutcome, &closedEnd); err != nil {
		t.Fatalf("read latest attempt: %v", err)
	}
	if closedStatus != StatusCanceled {
		t.Fatalf("latest attempt status = %q, want %q", closedStatus, StatusCanceled)
	}
	if closedOutcome != AttemptOutcomeOrphaned {
		t.Fatalf("latest attempt outcome = %q, want %q", closedOutcome, AttemptOutcomeOrphaned)
	}
	if !closedEnd.Valid || closedEnd.Int64 == 0 {
		t.Fatal("latest attempt end_time should be set")
	}
}

func TestSetJobLaunchID_NoOpenAttempt(t *testing.T) {
	database := setupTestDB(t)

	ci := &Launch{Status: LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX 4090"}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Insert a job, then close the attempt to simulate a previous failure.
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if err := CloseAttempt(database, 1, StatusFailed, nil, 1000); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}

	// SetJobLaunchID creates a fresh placement attempt even with no open attempt.
	err = SetJobLaunchID(database, 1, instanceID)
	if err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("status = %q, want %q", job.Status, StatusQueued)
	}
}

func TestSetJobLaunchID_RunningAttempt(t *testing.T) {
	database := setupTestDB(t)

	ci := &Launch{Status: LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX 4090"}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Insert a job and mark its attempt as running.
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if err := MarkQueuedJobRunning(database, 1); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	// SetJobLaunchID closes the running attempt and creates a new placement attempt.
	err = SetJobLaunchID(database, 1, instanceID)
	if err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("status = %q, want %q", job.Status, StatusQueued)
	}
}

func TestDisplayGPUBrief_UsesResolvedNameAndActualMem(t *testing.T) {
	ci := &Launch{
		ResolvedGPUName: "RTX 4090",
		GPUSpec:         "NVIDIA ≥20GB",
		GPUMemGB:        24,
		NumGPUs:         1,
	}
	if got := ci.DisplayGPUBrief(); got != "RTX 4090 24GB" {
		t.Fatalf("DisplayGPUBrief() = %q, want %q", got, "RTX 4090 24GB")
	}
}

func TestDisplayGPUBrief_MultiGPUShowsCount(t *testing.T) {
	ci := &Launch{
		ResolvedGPUName: "A100 SXM4",
		GPUMemGB:        80,
		NumGPUs:         4,
	}
	if got := ci.DisplayGPUBrief(); got != "4x A100 SXM4 80GB" {
		t.Fatalf("DisplayGPUBrief() = %q, want %q", got, "4x A100 SXM4 80GB")
	}
}

func TestDisplayGPUDetails_IncludesModelMemCUDA(t *testing.T) {
	ci := &Launch{
		ResolvedGPUName: "A100 NVL",
		GPUMemGB:        80,
		NumGPUs:         2,
		CUDAVersion:     12.4,
	}
	got := ci.DisplayGPUDetails()
	for _, want := range []string{"A100 NVL", "80GB per GPU", "2x GPUs", "CUDA 12.4"} {
		if !strings.Contains(got, want) {
			t.Fatalf("DisplayGPUDetails() = %q, missing %q", got, want)
		}
	}
}

// --- Opslog sync tests ---

func createTerminalLaunch(t *testing.T, database *sql.DB, endedSecondsAgo int64) int64 {
	t.Helper()
	id, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	endedAt := nowUnix() - endedSecondsAgo
	if _, err := database.Exec(`UPDATE launches SET provider_instance_id = '12345', ended_at = ? WHERE id = ?`, endedAt, id); err != nil {
		t.Fatalf("set provider_instance_id/ended_at: %v", err)
	}
	return id
}

func TestListLaunchIDsNeedingOpslogSync_ExcludesTimedOut(t *testing.T) {
	database := setupTestDB(t)

	// Create a terminal launch that ended 1 hour ago
	id := createTerminalLaunch(t, database, 3600)

	// Initially it should need sync
	ids, err := ListLaunchIDsNeedingOpslogSync(database, 7*24*time.Hour) // 7 days as Duration
	if err != nil {
		t.Fatalf("ListLaunchIDsNeedingOpslogSync: %v", err)
	}
	if !containsID(ids, id) {
		t.Fatal("expected launch to need opslog sync before marking timeout")
	}

	// Mark as timed out
	if err := MarkOpslogTimeout(database, id); err != nil {
		t.Fatalf("MarkOpslogTimeout: %v", err)
	}

	// Should now be excluded
	ids, err = ListLaunchIDsNeedingOpslogSync(database, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("ListLaunchIDsNeedingOpslogSync: %v", err)
	}
	if containsID(ids, id) {
		t.Fatal("timed-out launch should be excluded from opslog sync")
	}
}

func TestListLaunchIDsNeedingOpslogSync_TimedOutRecentStillIncluded(t *testing.T) {
	database := setupTestDB(t)

	// Create a launch that ended just 1 second ago
	id := createTerminalLaunch(t, database, 1)

	// Mark timed out immediately (within the 10-minute safety buffer)
	if err := MarkOpslogTimeout(database, id); err != nil {
		t.Fatalf("MarkOpslogTimeout: %v", err)
	}

	// Should still be included because the timeout was set <10min after termination
	ids, err := ListLaunchIDsNeedingOpslogSync(database, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("ListLaunchIDsNeedingOpslogSync: %v", err)
	}
	if !containsID(ids, id) {
		t.Fatal("recently-timed-out launch should still be included (within safety buffer)")
	}
}

func TestMarkOpslogSynced_ClearsTimeoutFlag(t *testing.T) {
	database := setupTestDB(t)
	id := createTerminalLaunch(t, database, 3600)

	// Mark timed out then synced
	if err := MarkOpslogTimeout(database, id); err != nil {
		t.Fatalf("MarkOpslogTimeout: %v", err)
	}
	if err := MarkOpslogSynced(database, id); err != nil {
		t.Fatalf("MarkOpslogSynced: %v", err)
	}

	// Verify timeout flag is cleared
	var timeout int
	if err := database.QueryRow(`SELECT oplog_timeout FROM launches WHERE id = ?`, id).Scan(&timeout); err != nil {
		t.Fatalf("query: %v", err)
	}
	if timeout != 0 {
		t.Fatalf("oplog_timeout = %d after MarkOpslogSynced, want 0", timeout)
	}
}

func containsID(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func nowUnix() int64 {
	return time.Now().Unix()
}

func TestLaunchBootstrapDeadlineAndAgentReady(t *testing.T) {
	database := SetupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	deadline := time.Now().Add(20 * time.Minute)
	if err := SetLaunchBootstrapDeadline(database, id, deadline); err != nil {
		t.Fatalf("SetLaunchBootstrapDeadline: %v", err)
	}
	got, err := GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if got.BootstrapDeadlineUnix == nil || *got.BootstrapDeadlineUnix != deadline.Unix() {
		t.Fatalf("BootstrapDeadlineUnix = %v, want %d", got.BootstrapDeadlineUnix, deadline.Unix())
	}
	if got.BootstrapDeadlineExceeded(time.Now()) {
		t.Errorf("future deadline should not be exceeded")
	}
	if !got.BootstrapDeadlineExceeded(deadline.Add(time.Second)) {
		t.Errorf("past deadline should be exceeded")
	}
	if got.IsAgentReady() {
		t.Errorf("agent should not be ready before SetLaunchAgentReadyAtIfUnset")
	}

	first := time.Unix(1700000000, 0)
	if err := SetLaunchAgentReadyAtIfUnset(database, id, first); err != nil {
		t.Fatalf("SetLaunchAgentReadyAtIfUnset: %v", err)
	}
	got, _ = GetLaunch(database, id)
	if !got.IsAgentReady() || *got.AgentReadyAtUnix != first.Unix() {
		t.Fatalf("AgentReadyAtUnix = %v, want %d", got.AgentReadyAtUnix, first.Unix())
	}

	// Subsequent calls must not overwrite the first transition.
	if err := SetLaunchAgentReadyAtIfUnset(database, id, first.Add(time.Hour)); err != nil {
		t.Fatalf("second SetLaunchAgentReadyAtIfUnset: %v", err)
	}
	got, _ = GetLaunch(database, id)
	if *got.AgentReadyAtUnix != first.Unix() {
		t.Errorf("first transition should win: AgentReadyAtUnix = %d, want %d", *got.AgentReadyAtUnix, first.Unix())
	}
}

func TestJobsWithOrphanStreaks(t *testing.T) {
	database := setupTestDB(t)

	addOrphan := func(jobID int64, attempt int) {
		t.Helper()
		launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if _, err := database.Exec(
			`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, cloud_outcome, queued_at, end_time)
			 VALUES (?, ?, '', ?, ?, ?, strftime('%s','now'), strftime('%s','now'))`,
			jobID, attempt, launchID, StatusCanceled, AttemptOutcomeOrphaned,
		); err != nil {
			t.Fatalf("insert orphan: %v", err)
		}
	}
	addCompleted := func(jobID int64, attempt int) {
		t.Helper()
		launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusCompleted, Provider: "vastai"})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if _, err := database.Exec(
			`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, cloud_outcome, queued_at, end_time)
			 VALUES (?, ?, '', ?, ?, ?, strftime('%s','now'), strftime('%s','now'))`,
			jobID, attempt, launchID, StatusCompleted, AttemptOutcomeCompleted,
		); err != nil {
			t.Fatalf("insert completed: %v", err)
		}
	}

	// Job A: 3 orphans at tail, queued — should appear (streak=3).
	// Set requested_status='queued' so job_status reports queued even though
	// every closed attempt is an orphan; this mirrors production state after
	// ResetOrphanedCloudJobs has cycled the job back to unplaced.
	insertTestJob(t, database, 5001, "loop", "/tmp", StatusQueued)
	if _, err := database.Exec(`UPDATE jobs SET requested_status = 'queued' WHERE id = 5001`); err != nil {
		t.Fatalf("set requested_status: %v", err)
	}
	for i := 0; i < 3; i++ {
		addOrphan(5001, 100+i)
	}

	// Job B: 4 orphans but status completed — terminal, excluded.
	insertTestJob(t, database, 5002, "done", "/tmp", StatusCompleted)
	for i := 0; i < 4; i++ {
		addOrphan(5002, 200+i)
	}

	// Job C: 5 orphans then a completed attempt at tail — streak broken.
	insertTestJob(t, database, 5003, "recovered", "/tmp", StatusQueued)
	if _, err := database.Exec(`UPDATE jobs SET requested_status = 'queued' WHERE id = 5003`); err != nil {
		t.Fatalf("set requested_status: %v", err)
	}
	for i := 0; i < 5; i++ {
		addOrphan(5003, 300+i)
	}
	addCompleted(5003, 350)

	streaks, err := JobsWithOrphanStreaks(database, 3)
	if err != nil {
		t.Fatalf("JobsWithOrphanStreaks: %v", err)
	}
	var st string
	_ = database.QueryRow(`SELECT status FROM job_status WHERE id = 5001`).Scan(&st)
	if len(streaks) != 1 {
		t.Fatalf("len=%d, want 1; got %+v (job_status[5001]=%q)", len(streaks), streaks, st)
	}
	if streaks[0].JobID != 5001 || streaks[0].OrphanCount != 3 {
		t.Fatalf("got %+v, want job_id=5001 count=3", streaks[0])
	}
}
