package core

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

func TestKillJobUsesEffectiveStatusForHostlessRunning(t *testing.T) {
	// In the new model, a hostless job has no attempt and shows as "queued"
	// from requested_status. KillJob should still cancel it.
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "sleep 100", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	service := NewServiceWithDB(database)
	result, err := service.KillJob(jobID, ops.TimeoutFast)
	if err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	if !result.Outcome.Success {
		t.Fatalf("expected success outcome, got %+v", result.Outcome)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("effective status = %q, want %q", job.EffectiveStatus(), db.StatusCanceled)
	}
}

func TestLocalExecutionControlsRejectExternalJobsWithoutMutation(t *testing.T) {
	controls := []struct {
		name string
		run  func(*Service, int64) error
	}{
		{name: "kill", run: func(s *Service, id int64) error { _, err := s.KillJob(id, ops.TimeoutFast); return err }},
		{name: "draft", run: func(s *Service, id int64) error { _, err := s.DraftJob(id, ops.TimeoutFast); return err }},
		{name: "pause", run: func(s *Service, id int64) error { _, err := s.PauseJob(id, ops.TimeoutFast); return err }},
		{name: "resume", run: func(s *Service, id int64) error { _, err := s.ResumeJob(id, ops.TimeoutFast); return err }},
		{name: "request status", run: func(s *Service, id int64) error {
			_, err := s.RequestStatus(id, db.StatusDraft, ops.TimeoutFast)
			return err
		}},
	}
	for _, tc := range controls {
		t.Run(tc.name, func(t *testing.T) {
			database := db.SetupTestDB(t)
			binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
				Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
				RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
			})
			if err != nil {
				t.Fatalf("create external job: %v", err)
			}
			service := NewServiceWithDB(database)
			if err := tc.run(service, binding.JobID); err == nil || !strings.Contains(err.Error(), "SkyPilot") {
				t.Fatalf("control error = %v, want SkyPilot refusal", err)
			}
			job, err := db.GetJobByID(database, binding.JobID)
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			var requested sql.NullString
			if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, binding.JobID).Scan(&requested); err != nil {
				t.Fatalf("read requested status: %v", err)
			}
			if job.Status != db.StatusRunning || requested.Valid {
				t.Fatalf("control mutated external job: status=%q requested=%v", job.Status, requested)
			}
		})
	}
}
