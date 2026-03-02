package remediation

import (
	"log"
	"os"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestAttemptRemediation_NoMatch(t *testing.T) {
	ctx := RemediationContext{
		Job:        &db.Job{ID: 1, Host: "cool30"},
		LogContent: "Training complete! Loss: 0.01",
		Logger:     log.New(os.Stderr, "", 0),
	}
	result := AttemptRemediation(ctx)
	if result != nil {
		t.Errorf("expected nil result for clean log, got %+v", result)
	}
}

func TestAttemptRemediation_RetryLimitReached(t *testing.T) {
	ctx := RemediationContext{
		Job: &db.Job{
			ID:         1,
			Host:       "cool30",
			RetryCount: 1, // already retried once
		},
		LogContent: "ModuleNotFoundError: No module named 'transformers'",
		Logger:     log.New(os.Stderr, "", 0),
	}

	// This will try to diagnose but won't write to DB (no DB provided)
	// We're testing the logic path, not the DB write
	result := AttemptRemediation(ctx)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Retried {
		t.Error("should not retry when retry limit is reached")
	}
	if result.Diagnosis.Pattern != "missing_import" {
		t.Errorf("expected missing_import, got %s", result.Diagnosis.Pattern)
	}
}

func TestAttemptRemediation_CodingAgentNotConfigured(t *testing.T) {
	ctx := RemediationContext{
		Job: &db.Job{
			ID:   1,
			Host: "cool30",
		},
		LogContent: "ModuleNotFoundError: No module named 'transformers'",
		Logger:     log.New(os.Stderr, "", 0),
		Config:     &config.Config{}, // no coding agent configured
	}

	result := AttemptRemediation(ctx)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Retried {
		t.Error("should not retry code errors without a coding agent")
	}
	if result.Action != "diagnosis only (no coding agent configured)" {
		t.Errorf("unexpected action: %s", result.Action)
	}
}

func TestAttemptRemediation_EnvironmentNotRemediable(t *testing.T) {
	ctx := RemediationContext{
		Job: &db.Job{
			ID:   1,
			Host: "cool30",
		},
		LogContent: "torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB",
		Logger:     log.New(os.Stderr, "", 0),
	}

	result := AttemptRemediation(ctx)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Retried {
		t.Error("should not retry environment errors")
	}
	if result.Action != "diagnosis only (not remediable)" {
		t.Errorf("unexpected action: %s", result.Action)
	}
}
