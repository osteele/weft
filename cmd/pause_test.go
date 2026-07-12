package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

func TestPauseCommandRoutesToDraft(t *testing.T) {
	for _, args := range [][]string{
		{"pause", "wj42"},
		{"pause", "job", "wj42"},
		{"job", "pause", "wj42"},
	} {
		cmd, remaining, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd.RunE == nil {
			t.Fatalf("Find(%v) resolved to command without RunE", args)
		}
		if len(remaining) != 1 || remaining[0] != "wj42" {
			t.Fatalf("Find(%v) remaining = %v, want [wj42]", args, remaining)
		}
	}
}

func TestUnpauseCommandsRegistered(t *testing.T) {
	for _, args := range [][]string{
		{"unpause", "wj42"},
		{"unpause", "job", "wj42"},
		{"job", "unpause", "wj42"},
	} {
		cmd, remaining, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd.RunE == nil {
			t.Fatalf("Find(%v) resolved to command without RunE", args)
		}
		if len(remaining) != 1 || remaining[0] != "wj42" {
			t.Fatalf("Find(%v) remaining = %v, want [wj42]", args, remaining)
		}
	}
}

func TestRunPauseDraftsUnplacedQueuedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	if err := runPause(&cobra.Command{}, []string{ids.FormatJobID(jobID)}); err != nil {
		t.Fatalf("runPause: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.EffectiveStatus() != db.StatusDraft {
		t.Fatalf("status = %q, want draft", job.EffectiveStatus())
	}
}

func TestRunUnpauseQueuesHostlessDraftJob(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordDraftJobWithGPU(database, "", t.TempDir(), "echo draft", "draft", "")
	if err != nil {
		t.Fatalf("RecordDraftJobWithGPU: %v", err)
	}

	if err := runUnpause(&cobra.Command{}, []string{ids.FormatJobID(jobID)}); err != nil {
		t.Fatalf("runUnpause: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("status = %q, want queued", job.EffectiveStatus())
	}
}
