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
