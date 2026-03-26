package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestFormatWatchInstanceBlockKeepsAttemptsInlineInRunOrder(t *testing.T) {
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
	historicalIdx := strings.Index(out, "  249")
	if currentIdx == -1 || historicalIdx == -1 {
		t.Fatalf("expected both jobs in output, got:\n%s", out)
	}
	if strings.Contains(out, historicalCloudInstanceJobsHeader) {
		t.Fatalf("expected attempts to remain inline, got:\n%s", out)
	}
	if !(currentIdx < historicalIdx) {
		t.Fatalf("expected jobs to stay in run order, got:\n%s", out)
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
	idx283 := strings.Index(out, "  283")
	if idx253 == -1 || idx283 == -1 {
		t.Fatalf("expected final current job and failed attempt in output, got:\n%s", out)
	}
	if strings.Contains(out, historicalCloudInstanceJobsHeader) {
		t.Fatalf("expected attempts to remain inline, got:\n%s", out)
	}
	if !(idx253 < idx283) {
		t.Fatalf("expected failed attempts to stay in chronological order, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockPrefersLiveProviderRate(t *testing.T) {
	now := time.Unix(7200, 0)
	launchedAt := int64(0)
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:               125,
			Status:           db.CloudInstanceStatusRunning,
			Provider:         "vastai",
			CostPerHourCents: 150,
			LaunchedAt:       &launchedAt,
			GPUSpec:          "A100",
		},
		Instance: &cloud.Instance{CostPerHour: 2.25},
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{plain: true, now: now})
	if !strings.Contains(out, "Cost: $4.50 (uptime: 2h0m0s, rate: $2.25/hr)") {
		t.Fatalf("expected live provider rate in output, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockAbbreviatesLongProjectNames(t *testing.T) {
	instanceID := int64(142)
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:       instanceID,
			Status:   db.CloudInstanceStatusRunning,
			Provider: "vastai",
			GPUSpec:  "A100",
		},
		Jobs: []*db.Job{
			{
				ID:              175,
				Status:          db.StatusQueued,
				CloudInstanceID: &instanceID,
				Project:         "llm-performance-models",
				Description:     "EXP-042: vLLM cross-GPU profiles (A100)",
			},
		},
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{plain: true})
	if !strings.Contains(out, "llm-performance-models") {
		t.Fatalf("expected full project label, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockUsesLivePhaseForActiveJobStatus(t *testing.T) {
	instanceID := int64(142)
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:       instanceID,
			Status:   db.CloudInstanceStatusRunning,
			Provider: "vastai",
			GPUSpec:  "A100",
		},
		InstancePhase: "uploading-results:205",
		Jobs: []*db.Job{
			{ID: 205, Status: db.StatusQueued, Project: "adaptive-escalation", Description: "phase 1 retry"},
			{ID: 226, Status: db.StatusRunning, Project: "head-type-ontology", Description: "stale running row"},
		},
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{plain: true})
	if !strings.Contains(out, "Phase: uploading logs/results (job 205)") {
		t.Fatalf("expected live phase in output, got:\n%s", out)
	}
	if !strings.Contains(out, "  205  uploading") {
		t.Fatalf("expected active phase job to show uploading status, got:\n%s", out)
	}
	// Non-active jobs show their DB status as-is — no demotion to queued.
	if !strings.Contains(out, "  226  running") {
		t.Fatalf("expected non-active job to retain DB status, got:\n%s", out)
	}
}

func TestFormatPreviousInstanceLineSingleDonor(t *testing.T) {
	now := time.Unix(3600, 0)
	launchedAt := int64(0)
	donors := []*db.CloudInstance{
		{
			ID:                226,
			Status:            db.CloudInstanceStatusFailed,
			TerminationReason: "infra_failure",
			CostPerHourCents:  30,
			LaunchedAt:        &launchedAt,
		},
	}

	line := formatPreviousInstanceLine(donors, now)
	if !strings.Contains(line, "Instance 226") {
		t.Fatalf("expected Instance 226, got: %s", line)
	}
	if !strings.Contains(line, "infra_failure") {
		t.Fatalf("expected infra_failure, got: %s", line)
	}
	if !strings.Contains(line, "$0.30") {
		t.Fatalf("expected cost, got: %s", line)
	}
	if !strings.HasPrefix(line, "  Previous:") {
		t.Fatalf("expected Previous: prefix, got: %s", line)
	}
}

func TestFormatPreviousInstanceLineChain(t *testing.T) {
	now := time.Unix(3600, 0)
	donors := []*db.CloudInstance{
		{ID: 228, Status: db.CloudInstanceStatusFailed, TerminationReason: "infra_failure"},
		{ID: 226, Status: db.CloudInstanceStatusFailed, TerminationReason: "bootstrap_timeout"},
	}

	line := formatPreviousInstanceLine(donors, now)
	if !strings.Contains(line, "Instance 228 (infra_failure)") {
		t.Fatalf("expected Instance 228 (infra_failure), got: %s", line)
	}
	if !strings.Contains(line, "Instance 226 (bootstrap_timeout)") {
		t.Fatalf("expected Instance 226 (bootstrap_timeout), got: %s", line)
	}
	if !strings.Contains(line, " → ") {
		t.Fatalf("expected arrow separator, got: %s", line)
	}
}

func TestFormatPreviousInstanceLineEmpty(t *testing.T) {
	line := formatPreviousInstanceLine(nil, time.Now())
	if line != "" {
		t.Fatalf("expected empty string for nil donors, got: %s", line)
	}
}

func TestFormatWatchInstanceBlockIncludesPreviousLine(t *testing.T) {
	donorID := int64(226)
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:              228,
			Status:          db.CloudInstanceStatusRunning,
			Provider:        "vastai",
			GPUSpec:         "A100",
			DonorInstanceID: &donorID,
		},
		Jobs: []*db.Job{
			{ID: 419, Status: db.StatusQueued, Project: "llm-performance-models", Description: "EXP-068 batch sweep A100"},
		},
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{
		plain: true,
		donorInstances: []*db.CloudInstance{
			{ID: 226, Status: db.CloudInstanceStatusFailed, TerminationReason: "infra_failure"},
		},
	})
	if !strings.Contains(out, "Previous: Instance 226") {
		t.Fatalf("expected Previous line in block output, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockUsesRunningPhaseForQueuedActiveJob(t *testing.T) {
	instanceID := int64(140)
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:       instanceID,
			Status:   db.CloudInstanceStatusRunning,
			Provider: "vastai",
			GPUSpec:  "A40",
		},
		InstancePhase: "running:249",
		Jobs: []*db.Job{
			{ID: 249, Status: db.StatusQueued, Project: "llm-performance-models", Description: "spec decoding"},
			{ID: 295, Status: db.StatusQueued, Project: "llm-performance-models", Description: "framework overhead"},
		},
	}

	out := formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{plain: true})
	if !strings.Contains(out, "Phase: running job 249") {
		t.Fatalf("expected live running phase in output, got:\n%s", out)
	}
	if !strings.Contains(out, "  249  running") {
		t.Fatalf("expected active queued row to render as running, got:\n%s", out)
	}
	if strings.Contains(out, "  249  queued") {
		t.Fatalf("did not expect active job to stay queued, got:\n%s", out)
	}
}
