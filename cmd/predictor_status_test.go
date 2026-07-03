package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
	"github.com/spf13/cobra"
)

func TestEnsurePredictorUsableTimesOut(t *testing.T) {
	originalTimeout := predictorReadinessTimeout
	originalEnsure := predictorEnsureReadyFunc
	originalStatus := predictorGetStatusFunc
	t.Cleanup(func() {
		predictorReadinessTimeout = originalTimeout
		predictorEnsureReadyFunc = originalEnsure
		predictorGetStatusFunc = originalStatus
	})

	predictorReadinessTimeout = 10 * time.Millisecond
	predictorEnsureReadyFunc = func(predictor.Config) error {
		select {}
	}
	predictorGetStatusFunc = func(predictor.Config) predictor.Status {
		t.Fatal("status should not be queried after readiness timeout")
		return predictor.Status{}
	}

	enabled := true
	cfg := &config.Config{
		Predictor: config.PredictorConfig{
			Enabled:     &enabled,
			ProjectPath: t.TempDir(),
		},
	}
	err := ensurePredictorUsable(&cobra.Command{}, cfg, "instance planning")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error = %q, want timeout", err)
	}
}

func TestEnsurePredictorUsableStatusTimesOut(t *testing.T) {
	originalTimeout := predictorReadinessTimeout
	originalEnsure := predictorEnsureReadyFunc
	originalStatus := predictorGetStatusFunc
	t.Cleanup(func() {
		predictorReadinessTimeout = originalTimeout
		predictorEnsureReadyFunc = originalEnsure
		predictorGetStatusFunc = originalStatus
	})

	predictorReadinessTimeout = 10 * time.Millisecond
	predictorEnsureReadyFunc = func(predictor.Config) error {
		return nil
	}
	predictorGetStatusFunc = func(predictor.Config) predictor.Status {
		select {}
	}

	enabled := true
	cfg := &config.Config{
		Predictor: config.PredictorConfig{
			Enabled:     &enabled,
			ProjectPath: t.TempDir(),
		},
	}
	err := ensurePredictorUsable(&cobra.Command{}, cfg, "instance planning")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error = %q, want timeout", err)
	}
}
