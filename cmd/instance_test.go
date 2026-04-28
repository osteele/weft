package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestMarkReleasedInstanceFailed_ClosesUnresolvedJobsAsFailed(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusGrace,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusRunning, jobID); err != nil {
		t.Fatalf("set job running: %v", err)
	}

	if err := markReleasedInstanceFailed(database, instanceID); err != nil {
		t.Fatalf("markReleasedInstanceFailed: %v", err)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonJobFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonJobFailure)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusFailed {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusFailed)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("job launch_id = %v, want %d", job.LaunchID, instanceID)
	}

	outcomes, err := db.GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get outcomes: %v", err)
	}
	if outcomes[jobID] != db.AttemptOutcomeFailed {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[jobID], db.AttemptOutcomeFailed)
	}
}

func TestJoinNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"all empty", []string{"", "", ""}, ""},
		{"middle empty", []string{"a", "", "b"}, "a | b"},
		{"trailing empty", []string{"a", "b", ""}, "a | b"},
		{"leading empty", []string{"", "a", "b"}, "a | b"},
		{"all populated", []string{"a", "b", "c"}, "a | b | c"},
		{"whitespace treated as empty", []string{" ", "\t", "x"}, "x"},
		{"trims each segment", []string{"  a  ", "b\n"}, "a | b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joinNonEmpty(tc.in, " | ")
			if got != tc.want {
				t.Fatalf("joinNonEmpty(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInstanceInfoCommandAliasExists(t *testing.T) {
	var found bool
	for _, sub := range instanceCmd.Commands() {
		if sub.Name() == "info" {
			found = true
			if sub.RunE == nil {
				t.Fatal("instance info command should have a handler")
			}
		}
	}
	if !found {
		t.Fatal("instance info command not found")
	}
}
