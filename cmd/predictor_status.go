package cmd

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
	"github.com/spf13/cobra"
)

var estimationStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show predictor model readiness and refresh state",
	Long: `Show whether the configured predictor models are ready, stale, rebuilding,
or blocked by a schema mismatch.`,
	RunE: runEstimationStatus,
}

var ensurePredictorUsableFunc = ensurePredictorUsable
var predictorEnsureReadyFunc = predictor.EnsureReady
var predictorGetStatusFunc = predictor.GetStatus

var predictorReadinessTimeout = 15 * time.Second

func init() {
	estimationCmd.AddCommand(estimationStatusCmd)
}

func ensurePredictorUsable(cmd *cobra.Command, cfg *config.Config, scope string) error {
	pcfg := buildPredictorConfig(cfg)
	if !pcfg.Configured() {
		return nil
	}

	if err := runWithTimeout(predictorReadinessTimeout, func() error {
		return predictorEnsureReadyFunc(pcfg)
	}); err != nil {
		var unavailable *predictor.UnavailableError
		if errors.As(err, &unavailable) {
			return fmt.Errorf("%s: %s", scope, formatPredictorBlocked(unavailable.Status))
		}
		return fmt.Errorf("%s: predictor check failed: %w", scope, err)
	}

	status, err := valueWithTimeout(predictorReadinessTimeout, func() predictor.Status {
		return predictorGetStatusFunc(pcfg)
	})
	if err != nil {
		return fmt.Errorf("%s: predictor check failed: %w", scope, err)
	}
	if status.BackgroundRebuildRunning {
		fmt.Fprintln(cmd.ErrOrStderr(), formatPredictorRefreshNotice(status))
	}
	return nil
}

func runEstimationStatus(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pcfg := buildPredictorConfig(cfg)
	if !pcfg.Configured() {
		fmt.Fprintln(cmd.OutOrStdout(), "Predictor: not configured")
		fmt.Fprintf(cmd.OutOrStdout(), "Set predictor.project_path in %s\n", config.ConfigPath())
		return nil
	}

	status, statusErr := valueWithTimeout(predictorReadinessTimeout, func() predictor.Status {
		return predictorGetStatusFunc(pcfg)
	})
	if statusErr != nil {
		status = predictor.Status{
			Configured: true,
			ModelDir:   pcfg.ModelDirPath(),
		}
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Predictor\t%s\n", predictorStateLabel(status))
	fmt.Fprintf(w, "Model dir\t%s\n", status.ModelDir)
	if statusErr != nil {
		fmt.Fprintf(w, "Status check\t%s\n", statusErr)
		return w.Flush()
	}
	if status.SchemaIncompatible {
		fmt.Fprintf(w, "Schema\tincompatible\n")
		fmt.Fprintf(w, "Reason\t%s\n", status.SchemaReason)
	} else {
		fmt.Fprintf(w, "Schema\tcompatible\n")
	}
	if status.ModelAvailable {
		fmt.Fprintf(w, "Trained at\t%s\n", displayPredictorTime(status.TrainedAt))
		fmt.Fprintf(w, "Training jobs\t%d\n", status.JobCount)
	} else if status.MetaError != "" {
		fmt.Fprintf(w, "Model metadata\t%s\n", status.MetaError)
	}
	if status.CurrentJobCount > 0 {
		fmt.Fprintf(w, "Current jobs\t%d\n", status.CurrentJobCount)
	}
	if status.RetrainInterval > 0 {
		fmt.Fprintf(w, "Retrain interval\t%d\n", status.RetrainInterval)
	}
	if status.ModelAvailable {
		fmt.Fprintf(w, "New completed jobs\t%d\n", status.NewCompletedJobs)
	}
	if status.CountError != "" {
		fmt.Fprintf(w, "Training data count\t%s\n", status.CountError)
	}
	if status.BackgroundRebuildRunning {
		fmt.Fprintf(w, "Background rebuild\trunning\n")
		if status.BackgroundRebuildReason != "" {
			fmt.Fprintf(w, "Rebuild reason\t%s\n", status.BackgroundRebuildReason)
		}
		if !status.BackgroundRebuildStartedAt.IsZero() {
			fmt.Fprintf(w, "Rebuild started\t%s\n", status.BackgroundRebuildStartedAt.Format(time.RFC3339))
		}
	} else {
		fmt.Fprintf(w, "Background rebuild\tidle\n")
	}
	return w.Flush()
}

func runWithTimeout(timeout time.Duration, fn func() error) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- fn()
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func valueWithTimeout[T any](timeout time.Duration, fn func() T) (T, error) {
	resultCh := make(chan T, 1)
	go func() {
		resultCh <- fn()
	}()
	select {
	case result := <-resultCh:
		return result, nil
	case <-time.After(timeout):
		var zero T
		return zero, fmt.Errorf("timed out after %s", timeout)
	}
}

func predictorStateLabel(status predictor.Status) string {
	switch {
	case !status.Configured:
		return "not configured"
	case status.SchemaIncompatible:
		return "blocked"
	case status.BackgroundRebuildRunning:
		return "ready (rebuilding in background)"
	case status.Ready && status.Stale:
		return "ready (stale check pending)"
	case status.Ready:
		return "ready"
	default:
		return "not ready"
	}
}

func formatPredictorBlocked(status predictor.Status) string {
	message := fmt.Sprintf("predictor is blocked because the stored model schema is incompatible (%s)", status.SchemaReason)
	if status.BackgroundRebuildRunning {
		message += "; background rebuild is in progress"
		if !status.BackgroundRebuildStartedAt.IsZero() {
			message += fmt.Sprintf(" since %s", status.BackgroundRebuildStartedAt.Format(time.RFC3339))
		}
	}
	return message + ". Retry after the rebuild completes or run `weft retrain --if-schema-changed`."
}

func formatPredictorRefreshNotice(status predictor.Status) string {
	parts := []string{"Predictor rebuild in progress"}
	if status.BackgroundRebuildReason != "" {
		parts[0] += ": " + status.BackgroundRebuildReason
	}
	if status.TrainedAt != "" {
		parts = append(parts, "using current model trained at "+displayPredictorTime(status.TrainedAt))
	}
	if status.NewCompletedJobs > 0 {
		parts = append(parts, fmt.Sprintf("%d new completed jobs queued for retraining", status.NewCompletedJobs))
	}
	return strings.Join(parts, "; ") + "."
}

func displayPredictorTime(value string) string {
	if value == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return t.Format(time.RFC3339)
}
