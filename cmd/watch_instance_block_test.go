package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestFormatWatchInstanceBlockSeparatesHistoricalAttempts(t *testing.T) {
	instanceID := int64(124)
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:       instanceID,
			Status:   db.CloudInstanceStatusRunning,
			Provider: "vastai",
			GPUSpec:  "A40",
		},
		Jobs: []*db.Job{
			{ID: 199, Status: db.StatusRunning, CloudInstanceID: &instanceID, Description: "current"},
			{ID: 249, Status: db.StatusQueued, Description: "historical"},
		},
		JobAttemptOutcomes: map[int64]string{
			249: db.AttemptOutcomeFailed,
		},
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{plain: true})

	currentIdx := strings.Index(out, "  199")
	historyHeaderIdx := strings.Index(out, historicalCloudInstanceJobsHeader)
	historicalIdx := strings.Index(out, "  249")
	if currentIdx == -1 || historyHeaderIdx == -1 || historicalIdx == -1 {
		t.Fatalf("expected current job, historical section, and historical job in output, got:\n%s", out)
	}
	if !(currentIdx < historyHeaderIdx && historyHeaderIdx < historicalIdx) {
		t.Fatalf("expected current jobs before historical attempts, got:\n%s", out)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("expected historical attempt outcome in output, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockTreatsOpenAttemptJobsAsCurrent(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir, description, campaign_job_index, project)
		 VALUES
		 (203, ?, ?, 0, 'running', 'run 203', '/workspace/markov-attention', 'EXP-110', 0, 'markov-attention'),
		 (249, NULL, '', 0, 'queued', 'run 249', '/workspace/llm-performance-models', 'EXP-044', 1, 'llm-performance-models'),
		 (250, NULL, '', 0, 'queued', 'run 250', '/workspace/llm-performance-models', 'EXP-043', 2, 'llm-performance-models'),
		 (253, NULL, '', 0, 'queued', 'run 253', '/workspace/llm-performance-models', 'EXP-041', 3, 'llm-performance-models'),
		 (283, NULL, '', 0, 'queued', 'run 283', '/workspace/markov-attention', 'EXP-115', 4, 'markov-attention')`,
		instanceID, "",
	); err != nil {
		t.Fatalf("insert jobs: %v", err)
	}

	for _, jobID := range []int64{203, 249, 250, 253, 283} {
		if err := db.InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
			t.Fatalf("InsertJobCloudAttempt(%d): %v", jobID, err)
		}
	}
	if _, err := database.Exec(
		`UPDATE job_cloud_attempts
		 SET ended_at = strftime('%s', 'now'), outcome = ?
		 WHERE job_id = ? AND cloud_instance_id = ? AND ended_at IS NULL`,
		db.AttemptOutcomeFailed, 283, instanceID,
	); err != nil {
		t.Fatalf("close historical attempt: %v", err)
	}

	jobs, err := db.GetCloudInstanceJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstanceJobsIncludingAttempts: %v", err)
	}
	outcomes, err := db.GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByInstance: %v", err)
	}

	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:       instanceID,
			Status:   db.CloudInstanceStatusRunning,
			Provider: "vastai",
			GPUSpec:  "A40",
		},
		Jobs:               jobs,
		JobAttemptOutcomes: outcomes,
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{plain: true})
	for _, expected := range []string{
		"  Jobs: 1/5 resolved",
		"  203",
		"  249",
		"  250",
		"  253",
		"  283",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("expected %q in output, got:\n%s", expected, out)
		}
	}

	idx253 := strings.Index(out, "  253")
	headerIdx := strings.Index(out, historicalCloudInstanceJobsHeader)
	idx283 := strings.Index(out, "  283")
	if idx253 == -1 || headerIdx == -1 || idx283 == -1 {
		t.Fatalf("expected final current job, historical header, and historical job in output, got:\n%s", out)
	}
	if !(idx253 < headerIdx && headerIdx < idx283) {
		t.Fatalf("expected open-attempt jobs to remain in the current group, got:\n%s", out)
	}
}
