package remediation

import (
	"log/slog"
	"os"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestAttemptRemediation_NoMatchStoresUnknown(t *testing.T) {
	ctx := RemediationContext{
		Job:        &db.Job{ID: 1, Host: "host-beta"},
		LogContent: "Training complete! Loss: 0.01",
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	result := AttemptRemediation(ctx)
	if result == nil {
		t.Fatal("expected unknown diagnosis for failed attempt")
	}
	if result.Diagnosis.Pattern != "unknown" {
		t.Errorf("expected unknown, got %s", result.Diagnosis.Pattern)
	}
}

func TestAttemptRemediation_RetryLimitReached(t *testing.T) {
	ctx := RemediationContext{
		Job: &db.Job{
			ID:         1,
			Host:       "host-beta",
			RetryCount: 1, // already retried once
		},
		LogContent: "ModuleNotFoundError: No module named 'transformers'",
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
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
	if result.Diagnosis.Pattern != "module_not_found" {
		t.Errorf("expected module_not_found, got %s", result.Diagnosis.Pattern)
	}
}

func TestAttemptRemediation_CodingAgentNotConfigured(t *testing.T) {
	ctx := RemediationContext{
		Job: &db.Job{
			ID:   1,
			Host: "host-beta",
		},
		LogContent: "ModuleNotFoundError: No module named 'transformers'",
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
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
			Host: "host-beta",
		},
		LogContent: "torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB",
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
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
