package services

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/osteele/weft/internal/coordinatorrelay"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
)

func TestRelayUpdateAppliesTags(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{"old"}); err != nil {
		t.Fatalf("set initial tags: %v", err)
	}

	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "hostA" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		return "", "connection timed out", fmt.Errorf("exit status 255")
	})
	t.Cleanup(cleanupSSH)

	processor := &RelayProcessor{
		db:     database,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	req := &coordinatorrelay.Request{
		JobID: jobID,
		Update: &coordinatorrelay.UpdateJobPayload{
			Tags: []string{"benchmark", "exp-012"},
		},
	}
	var ack coordinatorrelay.Ack
	if err := processor.handleUpdate(context.Background(), req, &ack); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	want := []string{"benchmark", "exp-012"}
	if len(job.Tags) != len(want) {
		t.Fatalf("tags len = %d, want %d (%v)", len(job.Tags), len(want), job.Tags)
	}
	for i := range want {
		if job.Tags[i] != want[i] {
			t.Fatalf("tags[%d] = %q, want %q", i, job.Tags[i], want[i])
		}
	}
}

func TestRelayUpdateClearsTags(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{"benchmark"}); err != nil {
		t.Fatalf("set initial tags: %v", err)
	}

	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "hostA" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		return "", "connection timed out", fmt.Errorf("exit status 255")
	})
	t.Cleanup(cleanupSSH)

	processor := &RelayProcessor{
		db:     database,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	req := &coordinatorrelay.Request{
		JobID: jobID,
		Update: &coordinatorrelay.UpdateJobPayload{
			ClearTags: true,
		},
	}
	var ack coordinatorrelay.Ack
	if err := processor.handleUpdate(context.Background(), req, &ack); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if len(job.Tags) != 0 {
		t.Fatalf("tags = %v, want empty", job.Tags)
	}
}
