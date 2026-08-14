package db

import (
	"database/sql"
	"sync"
	"testing"
)

func TestUpsertExternalJobFromObservationIsIdempotent(t *testing.T) {
	database := SetupTestDB(t)

	obs := ExternalJobObservation{
		Executor:                ExternalExecutorSkyPilot,
		ExternalJobID:           "42",
		ExternalTaskID:          "task-a",
		ExternalClusterName:     "sky-cluster",
		RawStatus:               "RUNNING",
		RawStatusMessage:        "healthy",
		NormalizedStatus:        StatusRunning,
		SubmittedFromWorkingDir: "/tmp/project",
		Command:                 "python train.py",
		Description:             "train",
		Tags:                    []string{"processed", "compute-intensive"},
	}
	binding, created, err := UpsertExternalJobFromObservation(database, obs)
	if err != nil {
		t.Fatalf("UpsertExternalJobFromObservation: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Backend != BackendSkyPilot {
		t.Fatalf("Backend = %q, want %q", job.Backend, BackendSkyPilot)
	}
	if got := job.TargetKind(); got != JobTargetExternal {
		t.Fatalf("TargetKind = %q, want %q", got, JobTargetExternal)
	}
	if job.EffectiveStatus() != StatusRunning {
		t.Fatalf("status = %q, want %q", job.EffectiveStatus(), StatusRunning)
	}

	obs.RawStatus = "SUCCEEDED"
	obs.NormalizedStatus = StatusCompleted
	refreshed, created, err := UpsertExternalJobFromObservation(database, obs)
	if err != nil {
		t.Fatalf("second UpsertExternalJobFromObservation: %v", err)
	}
	if created {
		t.Fatal("second created = true, want false")
	}
	if refreshed.JobID != binding.JobID {
		t.Fatalf("second JobID = %d, want %d", refreshed.JobID, binding.JobID)
	}
	job, err = GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetJobByID after refresh: %v", err)
	}
	if job.EffectiveStatus() != StatusCompleted {
		t.Fatalf("status after refresh = %q, want %q", job.EffectiveStatus(), StatusCompleted)
	}
	if job.EndTime == nil {
		t.Fatal("completed external job should have end_time")
	}
}

func TestMarkExternalSyncWarningLeavesStatusUnchanged(t *testing.T) {
	database := SetupTestDB(t)

	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor:                ExternalExecutorSkyPilot,
		ExternalJobID:           "42",
		RawStatus:               "RUNNING",
		RawStatusMessage:        "healthy",
		NormalizedStatus:        StatusRunning,
		SubmittedFromWorkingDir: "/tmp/project",
		Command:                 "python train.py",
	})
	if err != nil {
		t.Fatalf("UpsertExternalJobFromObservation: %v", err)
	}
	if err := MarkExternalSyncWarning(database, binding.JobID, "sky unavailable"); err != nil {
		t.Fatalf("MarkExternalSyncWarning: %v", err)
	}

	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got := job.EffectiveStatus(); got != StatusRunning {
		t.Fatalf("status after warning = %q, want %q", got, StatusRunning)
	}
	refreshed, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetExternalJobBindingByJobID: %v", err)
	}
	if refreshed.RawStatus != "RUNNING" {
		t.Fatalf("raw status after warning = %q, want RUNNING", refreshed.RawStatus)
	}
	// The executor's own last message must survive. Writing the transport
	// failure here would delete the most recent thing actually known about the
	// job and replace it with a weft-side complaint.
	if refreshed.RawStatusMessage != "healthy" {
		t.Fatalf("raw status message = %q, want the executor's message preserved", refreshed.RawStatusMessage)
	}
	if refreshed.SyncWarning != "sky unavailable" {
		t.Fatalf("sync warning = %q, want it recorded on its own channel", refreshed.SyncWarning)
	}
	if refreshed.SyncWarningAt == nil {
		t.Fatal("sync warning timestamp not recorded")
	}

	// A successful observation means the channel recovered; the stale warning
	// must not keep implying weft is out of contact.
	if err := UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: binding.ExternalJobID,
		RawStatus: "RUNNING", RawStatusMessage: "healthy again", NormalizedStatus: StatusRunning,
	}); err != nil {
		t.Fatalf("UpdateExternalJobObservation: %v", err)
	}
	recovered, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetExternalJobBindingByJobID: %v", err)
	}
	if recovered.SyncWarning != "" || recovered.SyncWarningAt != nil {
		t.Fatalf("sync warning survived a successful observation: %q", recovered.SyncWarning)
	}
}

func TestUpdateExternalJobObservationRejectsMissingStatusAtomically(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		RawStatusMessage: "healthy",
		NormalizedStatus: StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	before := *binding.LastObservedAt

	err = UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatusMessage: "partial row",
		NormalizedStatus: StatusQueued,
	})
	if err == nil {
		t.Fatal("UpdateExternalJobObservation returned nil, want missing-status error")
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != StatusRunning {
		t.Fatalf("status = %q, want running", job.Status)
	}
	refreshed, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.RawStatus != "RUNNING" || refreshed.RawStatusMessage != "healthy" {
		t.Fatalf("binding changed to raw=%q message=%q", refreshed.RawStatus, refreshed.RawStatusMessage)
	}
	if refreshed.LastObservedAt == nil || *refreshed.LastObservedAt != before {
		t.Fatalf("last_observed_at = %v, want unchanged %d", refreshed.LastObservedAt, before)
	}
}

func TestConcurrentUpsertExternalJobFromObservationIsIdempotent(t *testing.T) {
	database := SetupTestDBWithOpen(t)
	obs := ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		ExternalTaskID:   "task-a",
		RawStatus:        "RUNNING",
		NormalizedStatus: StatusRunning,
		Command:          "python train.py",
	}

	const writers = 4
	start := make(chan struct{})
	type result struct {
		binding *ExternalJobBinding
		created bool
		err     error
	}
	results := make(chan result, writers)
	var ready sync.WaitGroup
	ready.Add(writers)
	for range writers {
		go func() {
			ready.Done()
			<-start
			binding, created, err := UpsertExternalJobFromObservation(database, obs)
			results <- result{binding: binding, created: created, err: err}
		}()
	}
	ready.Wait()
	close(start)

	var jobID int64
	createdCount := 0
	for range writers {
		result := <-results
		if result.err != nil {
			t.Fatalf("UpsertExternalJobFromObservation: %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if result.binding == nil {
			t.Fatal("nil binding")
		}
		if jobID == 0 {
			jobID = result.binding.JobID
		} else if result.binding.JobID != jobID {
			t.Fatalf("binding JobID = %d, want shared %d", result.binding.JobID, jobID)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
	var jobs, bindings int
	if err := database.QueryRow(`SELECT COUNT(*) FROM jobs WHERE backend = ?`, BackendSkyPilot).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE executor = ? AND external_job_id = ? AND external_task_id = ?`, ExternalExecutorSkyPilot, "42", "task-a").Scan(&bindings); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if jobs != 1 || bindings != 1 {
		t.Fatalf("jobs, bindings = %d, %d; want 1, 1", jobs, bindings)
	}
}

func TestAttachExternalBindingCannotStealExistingIdentity(t *testing.T) {
	database := SetupTestDB(t)
	identity := ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		ExternalTaskID:   "task-a",
		RawStatus:        "RUNNING",
		NormalizedStatus: StatusRunning,
		Command:          "python first.py",
	}
	first, _, err := UpsertExternalJobFromObservation(database, identity)
	if err != nil {
		t.Fatalf("create first binding: %v", err)
	}

	secondJobID, secondAttemptID, err := CreateExternalPendingJob(database, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		RawStatus:        "submitted",
		NormalizedStatus: StatusQueued,
		Command:          "python second.py",
	})
	if err != nil {
		t.Fatalf("create second pending job: %v", err)
	}
	if err := AttachExternalBinding(database, secondJobID, secondAttemptID, identity); err == nil {
		t.Fatal("AttachExternalBinding returned nil, want identity-conflict error")
	}

	bound, err := FindExternalJobBinding(database, identity.Executor, identity.ExternalJobID, identity.ExternalTaskID)
	if err != nil {
		t.Fatalf("find original binding: %v", err)
	}
	if bound == nil || bound.JobID != first.JobID {
		t.Fatalf("identity moved to job %v, want original job %d", bound, first.JobID)
	}
	secondBinding, err := GetExternalJobBindingByJobID(database, secondJobID)
	if err != nil {
		t.Fatalf("get second binding: %v", err)
	}
	if secondBinding != nil {
		t.Fatalf("second job acquired binding %+v", secondBinding)
	}
	second, err := GetJobByID(database, secondJobID)
	if err != nil {
		t.Fatalf("get second job: %v", err)
	}
	if got := second.EffectiveStatus(); got != StatusQueued {
		t.Fatalf("second job status = %q, want queued after atomic conflict", got)
	}
}

func TestRepeatedExternalTerminalObservationPreservesEndTime(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "SUCCEEDED",
		NormalizedStatus: StatusCompleted,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create completed binding: %v", err)
	}
	const originalEnd = int64(123456789)
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = ? WHERE job_id = ?`, originalEnd, binding.JobID); err != nil {
		t.Fatalf("set end time: %v", err)
	}
	if err := UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "SUCCEEDED",
		NormalizedStatus: StatusCompleted,
	}); err != nil {
		t.Fatalf("repeat terminal observation: %v", err)
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.EndTime == nil || *job.EndTime != originalEnd {
		t.Fatalf("end_time = %v, want preserved %d", job.EndTime, originalEnd)
	}
}

func TestDelayedRunningObservationDoesNotReopenTerminalExternalJob(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		NormalizedStatus: StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create running binding: %v", err)
	}
	if err := UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "SUCCEEDED",
		NormalizedStatus: StatusCompleted,
	}); err != nil {
		t.Fatalf("complete external job: %v", err)
	}
	const completedObservedAt = int64(200)
	if _, err := database.Exec(`UPDATE external_job_bindings SET last_observed_at = ? WHERE job_id = ?`, completedObservedAt, binding.JobID); err != nil {
		t.Fatalf("set completed observation time: %v", err)
	}
	completed, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get completed job: %v", err)
	}
	if completed.EndTime == nil {
		t.Fatal("completed job has no end time")
	}
	completedEnd := *completed.EndTime

	if err := UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		NormalizedStatus: StatusRunning,
	}); err != nil {
		t.Fatalf("apply delayed running observation: %v", err)
	}
	after, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job after delayed observation: %v", err)
	}
	if after.Status != StatusCompleted || after.EndTime == nil || *after.EndTime != completedEnd {
		t.Fatalf("status, end_time = %q, %v; want completed, %d", after.Status, after.EndTime, completedEnd)
	}
	refreshed, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding after delayed observation: %v", err)
	}
	if refreshed.LastObservedAt == nil || *refreshed.LastObservedAt != completedObservedAt || refreshed.RawStatus != "SUCCEEDED" {
		t.Fatalf("binding after delayed observation = raw %q observed %v; want SUCCEEDED at %d", refreshed.RawStatus, refreshed.LastObservedAt, completedObservedAt)
	}
}

func TestConflictingTerminalObservationDoesNotRewriteExternalOutcome(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "SUCCEEDED", NormalizedStatus: StatusCompleted, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create completed job: %v", err)
	}
	before := *binding.LastObservedAt
	applied, err := ApplyExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "FAILED", NormalizedStatus: StatusFailed,
	})
	if err != nil {
		t.Fatalf("apply conflicting terminal observation: %v", err)
	}
	if applied {
		t.Fatal("conflicting terminal observation applied, want final outcome preserved")
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", job.Status)
	}
	after, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if after.RawStatus != "SUCCEEDED" || after.LastObservedAt == nil || *after.LastObservedAt != before {
		t.Fatalf("binding = raw %q at %v, want original SUCCEEDED at %d", after.RawStatus, after.LastObservedAt, before)
	}
}

func TestSameBucketTerminalObservationDoesNotRewriteExternalOutcome(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "FAILED_SETUP", NormalizedStatus: StatusFailed, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create failed job: %v", err)
	}
	before := *binding.LastObservedAt
	applied, err := ApplyExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "FAILED_CONTROLLER", NormalizedStatus: StatusFailed,
	})
	if err != nil {
		t.Fatalf("apply same-bucket terminal observation: %v", err)
	}
	if applied {
		t.Fatal("same-bucket conflicting terminal observation applied, want first raw outcome preserved")
	}
	after, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if after.RawStatus != "FAILED_SETUP" || after.LastObservedAt == nil || *after.LastObservedAt != before {
		t.Fatalf("binding = raw %q at %v, want original FAILED_SETUP at %d", after.RawStatus, after.LastObservedAt, before)
	}
}

func TestCancellingObservationDoesNotClearIntentOrCloseAttempt(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	if requested, err := SetExternalCancelIntent(database, binding.JobID); err != nil || !requested {
		t.Fatalf("set cancel intent = %t, %v", requested, err)
	}
	if err := UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "CANCELLING", NormalizedStatus: StatusRunning,
	}); err != nil {
		t.Fatalf("apply cancelling observation: %v", err)
	}
	var requested sql.NullInt64
	var attemptStatus string
	var endTime sql.NullInt64
	if err := database.QueryRow(`
		SELECT b.cancel_requested_at, a.status, a.end_time
		  FROM external_job_bindings b JOIN job_attempts a ON a.job_id = b.job_id
		 WHERE b.job_id = ?`, binding.JobID).Scan(&requested, &attemptStatus, &endTime); err != nil {
		t.Fatalf("read cancelling state: %v", err)
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get cancelling job: %v", err)
	}
	if !requested.Valid || attemptStatus != StatusRunning || endTime.Valid || job.Status != StatusRunning {
		t.Fatalf("intent/status/end/public = %v/%q/%v/%q, want pending intent with open running job", requested, attemptStatus, endTime, job.Status)
	}
}

func TestRebindUnconfirmedExternalJobRejectsTerminalJobAtomically(t *testing.T) {
	database := SetupTestDB(t)
	jobID, _, err := CreateExternalPendingJob(database, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		RawStatus:        "submitted",
		NormalizedStatus: StatusQueued,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	if err := MarkExternalSubmissionFailed(database, jobID, "definitively refused"); err != nil {
		t.Fatalf("mark submission failed: %v", err)
	}
	if binding, err := RebindUnconfirmedExternalJob(database, jobID, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "SUCCEEDED",
		NormalizedStatus: StatusCompleted,
	}); err == nil || binding != nil {
		t.Fatalf("RebindUnconfirmedExternalJob = %+v, %v; want terminal-job error", binding, err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get terminal job: %v", err)
	}
	if job.Status != StatusDead {
		t.Fatalf("status = %q, want original dead status", job.Status)
	}
	binding, err := GetExternalJobBindingByJobID(database, jobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding != nil {
		t.Fatalf("terminal job acquired binding %+v", binding)
	}
}

func TestRebindUnconfirmedExternalJobIsIdempotentForSameIdentity(t *testing.T) {
	database := SetupTestDB(t)
	jobID, _, err := CreateExternalPendingJob(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, RawStatus: "submitted",
		NormalizedStatus: StatusQueued, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	obs := ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning,
	}
	first, err := RebindUnconfirmedExternalJob(database, jobID, obs)
	if err != nil {
		t.Fatalf("first rebind: %v", err)
	}
	if _, err := database.Exec(`
		UPDATE external_job_bindings
		SET last_observed_at = 1, sync_warning = 'stale observation', sync_warning_at = 1
		WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("seed stale binding state: %v", err)
	}
	completed := obs
	completed.RawStatus = "SUCCEEDED"
	completed.NormalizedStatus = StatusCompleted
	second, err := RebindUnconfirmedExternalJob(database, jobID, completed)
	if err != nil {
		t.Fatalf("repeat same rebind: %v", err)
	}
	if second == nil || first == nil || second.ID != first.ID || second.JobID != jobID {
		t.Fatalf("repeat binding = %+v, want original %+v", second, first)
	}
	if second.RawStatus != "SUCCEEDED" || second.NormalizedStatus != StatusCompleted || second.SyncWarning != "" || second.LastObservedAt == nil || *second.LastObservedAt <= 1 {
		t.Fatalf("refreshed binding = %+v, want completed positive observation with warning cleared", second)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get refreshed job: %v", err)
	}
	if job.Status != StatusCompleted || job.EndTime == nil {
		t.Fatalf("refreshed job status/end = %q/%v, want completed terminal attempt", job.Status, job.EndTime)
	}
	conflicting := completed
	conflicting.RawStatus = "FAILED"
	conflicting.NormalizedStatus = StatusFailed
	third, err := RebindUnconfirmedExternalJob(database, jobID, conflicting)
	if err != nil {
		t.Fatalf("conflicting terminal re-import should be an idempotent no-op: %v", err)
	}
	if third.RawStatus != "SUCCEEDED" || third.NormalizedStatus != StatusCompleted {
		t.Fatalf("conflicting terminal re-import rewrote final binding: %+v", third)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if count != 1 {
		t.Fatalf("binding count = %d, want 1", count)
	}
}

func TestHistoricalExternalImportPreservesExecutorTimestamps(t *testing.T) {
	database := SetupTestDB(t)
	submittedAt, startedAt, endedAt := int64(1_700_000_000), int64(1_700_000_100), int64(1_700_000_200)
	binding, created, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "FAILED", NormalizedStatus: StatusFailed, Command: "python train.py",
		SubmittedAt: &submittedAt, StartedAt: &startedAt, EndedAt: &endedAt,
	})
	if err != nil || !created {
		t.Fatalf("historical import = %+v, %t, %v", binding, created, err)
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get imported job: %v", err)
	}
	if job.CreatedAt != submittedAt || job.StartTime != startedAt || job.EndTime == nil || *job.EndTime != endedAt {
		t.Fatalf("job lifecycle times = created %d, start %d, end %v", job.CreatedAt, job.StartTime, job.EndTime)
	}
	attempts, err := ListAttempts(database, binding.JobID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("list attempts = %+v, %v", attempts, err)
	}
	if attempts[0].QueuedAt == nil || *attempts[0].QueuedAt != submittedAt || attempts[0].StartTime == nil || *attempts[0].StartTime != startedAt || attempts[0].EndTime == nil || *attempts[0].EndTime != endedAt {
		t.Fatalf("attempt lifecycle times = %+v", attempts[0])
	}
}

func TestRebindUnconfirmedExternalJobRefinesProvisionalTaskIdentity(t *testing.T) {
	database := SetupTestDB(t)
	jobID, attemptID, err := CreateExternalPendingJob(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, RawStatus: "submitted",
		NormalizedStatus: StatusQueued, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	if err := AttachExternalBinding(database, jobID, attemptID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "SUBMITTED", NormalizedStatus: StatusQueued,
	}); err != nil {
		t.Fatalf("attach provisional binding: %v", err)
	}

	binding, err := RebindUnconfirmedExternalJob(database, jobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning,
	})
	if err != nil {
		t.Fatalf("refine provisional binding: %v", err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get refined job: %v", err)
	}
	if count != 1 || binding.ExternalTaskID != "task-a" || job.Status != StatusRunning {
		t.Fatalf("refined state = count %d, binding %+v, status %q", count, binding, job.Status)
	}
}

func TestTerminalExternalObservationClearsPendingCancelIntent(t *testing.T) {
	database := SetupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	if requested, err := SetExternalCancelIntent(database, binding.JobID); err != nil || !requested {
		t.Fatalf("set cancel intent: %v", err)
	}
	if err := UpdateExternalJobObservation(database, binding.JobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "SUCCEEDED", NormalizedStatus: StatusCompleted,
	}); err != nil {
		t.Fatalf("apply terminal observation: %v", err)
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed external observation", job.Status)
	}
	var requested sql.NullString
	if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, binding.JobID).Scan(&requested); err != nil {
		t.Fatalf("read requested status: %v", err)
	}
	if requested.Valid {
		t.Fatalf("requested_status = %q, want cleared after terminal observation", requested.String)
	}
	refreshed, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.CancelRequestedAt != nil {
		t.Fatalf("cancel_requested_at = %v, want cleared after terminal observation", refreshed.CancelRequestedAt)
	}
}

func TestExternalTaskIdentityRefinementConflictRollsBackObservation(t *testing.T) {
	database := SetupTestDB(t)
	firstJobID, firstAttemptID, err := CreateExternalPendingJob(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, RawStatus: "submitted",
		NormalizedStatus: StatusQueued, Command: "python first.py",
	})
	if err != nil {
		t.Fatalf("create first pending job: %v", err)
	}
	if err := AttachExternalBinding(database, firstJobID, firstAttemptID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "SUBMITTED", NormalizedStatus: StatusQueued,
	}); err != nil {
		t.Fatalf("attach unqualified binding: %v", err)
	}
	secondJobID, secondAttemptID, err := CreateExternalPendingJob(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, RawStatus: "submitted",
		NormalizedStatus: StatusQueued, Command: "python second.py",
	})
	if err != nil {
		t.Fatalf("create second pending job: %v", err)
	}
	if err := AttachExternalBinding(database, secondJobID, secondAttemptID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning,
	}); err != nil {
		t.Fatalf("attach qualified binding: %v", err)
	}

	err = UpdateExternalJobObservation(database, firstJobID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning,
	})
	if err == nil {
		t.Fatal("UpdateExternalJobObservation returned nil, want identity-conflict error")
	}
	first, err := GetJobByID(database, firstJobID)
	if err != nil {
		t.Fatalf("get first job: %v", err)
	}
	if first.Status != StatusQueued {
		t.Fatalf("first job status = %q, want queued after rollback", first.Status)
	}
	bindings, err := ListExternalJobBindings(database, ExternalExecutorSkyPilot, "")
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("binding count = %d, want 2", len(bindings))
	}
	byJob := make(map[int64]string, len(bindings))
	for _, binding := range bindings {
		byJob[binding.JobID] = binding.ExternalTaskID
	}
	if byJob[firstJobID] != "" || byJob[secondJobID] != "task-a" {
		t.Fatalf("binding tasks = %#v, want first unqualified and second task-a", byJob)
	}
}

func TestExternalJobAllowsOnlyOneBindingAndNoLocalRetry(t *testing.T) {
	database := setupTestDB(t)
	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "SUCCEEDED", NormalizedStatus: StatusCompleted, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	if binding.AttemptID == nil {
		t.Fatal("external binding has no attempt")
	}
	err = AttachExternalBinding(database, binding.JobID, *binding.AttemptID, ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "99", ExternalTaskID: "task-b",
		RawStatus: "RUNNING", NormalizedStatus: StatusRunning,
	})
	if err == nil {
		t.Fatal("AttachExternalBinding accepted a second identity for one job")
	}
	var bindings int
	if err := database.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE job_id = ?`, binding.JobID).Scan(&bindings); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if bindings != 1 {
		t.Fatalf("bindings = %d, want 1", bindings)
	}
	var attemptsBefore int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, binding.JobID).Scan(&attemptsBefore); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if err := RequeueByID(database, binding.JobID); err == nil {
		t.Fatal("RequeueByID accepted an external job")
	}
	if err := RequeueFreshAttemptByID(database, binding.JobID, ""); err == nil {
		t.Fatal("RequeueFreshAttemptByID accepted an external job")
	}
	var attemptsAfter int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, binding.JobID).Scan(&attemptsAfter); err != nil {
		t.Fatalf("count attempts after: %v", err)
	}
	if attemptsAfter != attemptsBefore {
		t.Fatalf("retry guards changed attempts: %d -> %d", attemptsBefore, attemptsAfter)
	}
}
