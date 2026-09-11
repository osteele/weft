package db

import "testing"

func TestSetJobCPUReserveCores(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host", "/tmp", "echo ok", "desc")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	reserve := 2
	if err := SetJobCPUReserveCores(database, jobID, &reserve); err != nil {
		t.Fatalf("set cpu reserve: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.CPUReserveCores == nil || *job.CPUReserveCores != reserve {
		t.Fatalf("expected CPU reserve %d, got %#v", reserve, job.CPUReserveCores)
	}
	if got := job.RequestedCPUReserveCores(); got != reserve {
		t.Fatalf("RequestedCPUReserveCores = %d, want %d", got, reserve)
	}

	if err := SetJobCPUReserveCores(database, jobID, nil); err != nil {
		t.Fatalf("clear cpu reserve: %v", err)
	}

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after clear: %v", err)
	}
	if job.CPUReserveCores != nil {
		t.Fatalf("expected CPU reserve cleared, got %#v", job.CPUReserveCores)
	}
	if got := job.RequestedCPUReserveCores(); got != 0 {
		t.Fatalf("RequestedCPUReserveCores after clear = %d, want 0", got)
	}
}

func TestRequestedCPUReserveCores(t *testing.T) {
	reserve := 4
	if got := (&Job{CPUReserveCores: &reserve}).RequestedCPUReserveCores(); got != 4 {
		t.Errorf("set = %d, want 4", got)
	}
	negative := -3
	if got := (&Job{CPUReserveCores: &negative}).RequestedCPUReserveCores(); got != 0 {
		t.Errorf("negative = %d, want 0", got)
	}
	if got := (&Job{}).RequestedCPUReserveCores(); got != 0 {
		t.Errorf("unset = %d, want 0", got)
	}
}
