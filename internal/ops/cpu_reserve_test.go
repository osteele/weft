package ops

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

// TestCPUReserveCoresEndToEnd covers the persistence path the CPU gate
// depends on: RecordQueuedJob stores the reserve, the stored job carries it
// across a queue-entry rebuild (describe/requeue/edit), and the rebuilt
// command job carries it onto the wire payload.
func TestCPUReserveCoresEndToEnd(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:            "host",
		WorkingDir:      "/tmp",
		Command:         "echo ok",
		CPUReserveCores: 2,
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.CPUReserveCores == nil || *job.CPUReserveCores != 2 {
		t.Fatalf("stored reserve = %#v, want 2", job.CPUReserveCores)
	}

	entry, err := queueEntryForJob(database, job, nil, "")
	if err != nil {
		t.Fatalf("queueEntryForJob: %v", err)
	}
	if entry.CPUReserveCores != 2 {
		t.Fatalf("queueEntryForJob CPUReserveCores = %d, want 2", entry.CPUReserveCores)
	}

	commandJob := commandJobForQueueEntry(entry)
	if commandJob.CPUReserveCores != 2 {
		t.Fatalf("commandJobForQueueEntry CPUReserveCores = %d, want 2", commandJob.CPUReserveCores)
	}
}
