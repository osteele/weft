package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/remediation"
)

func TestParseLaunchOpts(t *testing.T) {
	tests := []struct {
		name           string
		maxSpend       string
		maxTime        string
		wantSpendCents int
		wantTimeSecs   int
	}{
		{"empty", "", "", 0, 0},
		{"spend with dollar sign", "$5.00", "", 500, 0},
		{"spend without dollar sign", "10.50", "", 1050, 0},
		{"time 2h", "", "2h", 0, 7200},
		{"time 30m", "", "30m", 0, 1800},
		{"both", "$2.50", "1h", 250, 3600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set globals (restored after test)
			oldSpend, oldTime := campaignLaunchMaxSpend, campaignLaunchMaxTime
			defer func() {
				campaignLaunchMaxSpend = oldSpend
				campaignLaunchMaxTime = oldTime
			}()

			campaignLaunchMaxSpend = tt.maxSpend
			campaignLaunchMaxTime = tt.maxTime

			opts := parseLaunchOpts()

			if opts.MaxSpendCents != tt.wantSpendCents {
				t.Errorf("MaxSpendCents = %d, want %d", opts.MaxSpendCents, tt.wantSpendCents)
			}
			if opts.MaxTimeSeconds != tt.wantTimeSecs {
				t.Errorf("MaxTimeSeconds = %d, want %d", opts.MaxTimeSeconds, tt.wantTimeSecs)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func TestFilterJobsByIDs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 10, Status: db.StatusQueued},
		{ID: 20, Status: db.StatusQueued},
		{ID: 30, Status: db.StatusQueued},
	}

	// Filter to jobs 10 and 30
	filter := map[int64]bool{10: true, 30: true}
	var filtered []*db.Job
	for _, j := range jobs {
		if filter[j.ID] {
			filtered = append(filtered, j)
		}
	}

	if len(filtered) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(filtered))
	}
	if filtered[0].ID != 10 || filtered[1].ID != 30 {
		t.Errorf("expected jobs [10, 30], got [%d, %d]", filtered[0].ID, filtered[1].ID)
	}
}

func TestFilterRentalLaunchJobsExcludesInventoryJobs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Tags: []string{db.TagInventory}},
		{ID: 2, Tags: []string{db.TagRental}},
		{ID: 3, Host: ""},
	}

	filtered := filterRentalLaunchJobs(jobs)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 rental-eligible jobs, got %d", len(filtered))
	}
	for _, job := range filtered {
		if job.HasTag(db.TagInventory) {
			t.Fatalf("inventory job should be excluded: %+v", job)
		}
	}
}

func TestNonInteractiveLaunchGrouping(t *testing.T) {
	// Verify that jobs are grouped correctly for non-interactive launch
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "H100", GPUMemGB: intPtr(80)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "H100", GPUMemGB: intPtr(80)},
		{ID: 3, Status: db.StatusQueued, GPUClass: "A100", GPUMemGB: intPtr(40)},
	}

	groups := campaign.GroupByGPUSupremum(jobs)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// H100 group should have 2 jobs
	h100Jobs := 0
	a100Jobs := 0
	for _, g := range groups {
		switch g.GPUClass {
		case "H100":
			h100Jobs = len(g.Jobs)
		case "A100":
			a100Jobs = len(g.Jobs)
		}
	}
	if h100Jobs != 2 {
		t.Errorf("H100 group: expected 2 jobs, got %d", h100Jobs)
	}
	if a100Jobs != 1 {
		t.Errorf("A100 group: expected 1 job, got %d", a100Jobs)
	}
}

func TestParseLaunchOptsTimeDuration(t *testing.T) {
	oldTime := campaignLaunchMaxTime
	defer func() { campaignLaunchMaxTime = oldTime }()

	campaignLaunchMaxTime = "1h30m"
	opts := parseLaunchOpts()

	expected := int((1*time.Hour + 30*time.Minute).Seconds())
	if opts.MaxTimeSeconds != expected {
		t.Errorf("MaxTimeSeconds = %d, want %d", opts.MaxTimeSeconds, expected)
	}
}

func TestResolveCampaignWatchIDDefaultsToMostRecent(t *testing.T) {
	database := db.SetupTestDB(t)

	if _, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusCompleted}); err != nil {
		t.Fatalf("CreateCampaign(first): %v", err)
	}
	secondID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign(second): %v", err)
	}

	got, err := resolveCampaignWatchID(database, nil)
	if err != nil {
		t.Fatalf("resolveCampaignWatchID: %v", err)
	}
	if got != secondID {
		t.Fatalf("resolveCampaignWatchID = %d, want %d", got, secondID)
	}
}

func TestResolveCampaignDiagnoseIDDefaultsToMostRecentFailed(t *testing.T) {
	database := db.SetupTestDB(t)

	if _, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusCompleted}); err != nil {
		t.Fatalf("CreateCampaign(completed): %v", err)
	}
	if _, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusFailed}); err != nil {
		t.Fatalf("CreateCampaign(failed-1): %v", err)
	}
	if _, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning}); err != nil {
		t.Fatalf("CreateCampaign(running): %v", err)
	}
	secondFailedID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusFailed})
	if err != nil {
		t.Fatalf("CreateCampaign(failed-2): %v", err)
	}

	got, err := resolveCampaignDiagnoseID(database, nil)
	if err != nil {
		t.Fatalf("resolveCampaignDiagnoseID: %v", err)
	}
	if got != secondFailedID {
		t.Fatalf("resolveCampaignDiagnoseID = %d, want %d", got, secondFailedID)
	}
}

func TestBuildCampaignDiagnosisReport(t *testing.T) {
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusFailed})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	providerFailedID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(provider_failure): %v", err)
	}
	if err := db.UpdateLaunchStatus(database, providerFailedID, db.LaunchStatusFailed, db.TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(provider_failure): %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, tombstoned, command, working_dir) VALUES (1, 0, 'python train.py', '/tmp')`,
	); err != nil {
		t.Fatalf("insert orphaned job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusQueued, providerFailedID)
	if err := db.CloseLaunchAttempts(database, providerFailedID, db.AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("CloseLaunchAttempts(orphaned): %v", err)
	}

	jobFailureID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(job failure): %v", err)
	}
	if err := db.UpdateLaunchStatus(database, jobFailureID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(job failure): %v", err)
	}

	diagJSON, err := remediation.MarshalDiagnosis(&remediation.ErrorDiagnosis{
		Pattern:  "gpu_oom",
		Category: "environment",
		Message:  "GPU out of memory",
	})
	if err != nil {
		t.Fatalf("MarshalDiagnosis: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, tombstoned, command, working_dir) VALUES (2, 0, 'python train.py', '/tmp')`,
	); err != nil {
		t.Fatalf("insert failed job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ?, failure_reason = ?, error_diagnosis = ? WHERE job_id = 2 AND end_time IS NULL`,
		db.StatusFailed, jobFailureID, "gpu_oom", diagJSON)

	report, err := buildCampaignDiagnosisReport(database, campaignID)
	if err != nil {
		t.Fatalf("buildCampaignDiagnosisReport: %v", err)
	}

	out := formatCampaignDiagnosisReport(report)
	if !strings.Contains(out, "provider terminated the instance") {
		t.Fatalf("diagnosis output missing provider failure summary:\n%s", out)
	}
	if !strings.Contains(out, "Job #1: orphaned after the instance terminated") {
		t.Fatalf("diagnosis output missing orphaned job detail:\n%s", out)
	}
	if !strings.Contains(out, "GPU out of memory") {
		t.Fatalf("diagnosis output missing GPU OOM detail:\n%s", out)
	}
}
