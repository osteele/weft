package cmd

import (
	"errors"
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
	"github.com/spf13/cobra"
)

var (
	predictHost     string
	predictProject  string
	predictGPUClass string
)

var predictCmd = &cobra.Command{
	Use:   "predict <command>",
	Short: "Predict job duration and resource usage",
	Long: `Predict how long a job will take and how much memory it will use.

Uses trained machine learning models based on historical job data.
When models become stale, weft starts a background retrain and continues using
the current compatible model. If the on-disk model schema is incompatible with
the current code, prediction is blocked until the model is rebuilt.

Examples:
  weft predict --host cool30 'uv run python train.py --epochs 50'
  weft predict --host cool100 --project lm2 'python train.py'`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runPredict,
}

func runPredict(cmd *cobra.Command, args []string) error {
	command := args[0]

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pcfg := buildPredictorConfig(cfg)
	if pcfg.ProjectPath == "" {
		return fmt.Errorf("predictor not configured: set predictor.project_path in %s", config.ConfigPath())
	}

	result, err := predictor.Predict(pcfg, predictHost, predictProject, predictGPUClass, command)
	if err != nil {
		var unavailable *predictor.UnavailableError
		if errors.As(err, &unavailable) {
			return fmt.Errorf("%s", formatPredictorBlocked(unavailable.Status))
		}
		return err
	}
	status := predictor.GetStatus(pcfg)
	if status.BackgroundRebuildRunning {
		fmt.Fprintln(cmd.ErrOrStderr(), formatPredictorRefreshNotice(status))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Duration: %s\n", predictor.FormatDuration(result.DurationS))
	if meta := result.DurationMetadata; meta != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Runtime:  %s (confidence %.0f%%)\n", formatRuntimeSource(meta.Source), meta.Confidence*100)
		if meta.Bottleneck != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Bounded:  %s\n", meta.Bottleneck)
		}
		if meta.ScriptFamily != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Family:   %s\n", meta.ScriptFamily)
		}
		if meta.WorkloadFingerprint != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Fingerprint: %s\n", meta.WorkloadFingerprint)
		}
		if meta.ResidualCorrectionFactor > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Residual: %.2fx (%s)\n", meta.ResidualCorrectionFactor, meta.ResidualCorrectionSource)
		}
		for _, reason := range meta.OODReasons {
			fmt.Fprintf(cmd.OutOrStdout(), "OOD:      %s\n", reason)
		}
		if meta.Feasible != nil {
			if *meta.Feasible {
				fmt.Fprintln(cmd.OutOrStdout(), "Fits GPU: yes")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Fits GPU: no")
			}
		}
		if meta.MemoryHeadroomMiB > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Headroom: ~%.1f GiB\n", meta.MemoryHeadroomMiB/1024.0)
		}
		if meta.BenefitsFromAdditionalVRAM != nil {
			if *meta.BenefitsFromAdditionalVRAM {
				fmt.Fprintln(cmd.OutOrStdout(), "VRAM:     more capacity could help")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "VRAM:     surplus capacity does not add speed")
			}
		}
	}

	if result.MaxGPUMemMiB != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "GPU mem:  %s\n", predictor.FormatMemory(result.MaxGPUMemMiB, "GiB"))
	}

	if result.PeakRSSKB != nil {
		// Convert KB prediction to GiB for display
		rssGiB := &predictor.Prediction{
			Mean:  result.PeakRSSKB.Mean / (1024 * 1024),
			Std:   result.PeakRSSKB.Std / (1024 * 1024),
			Lower: result.PeakRSSKB.Lower / (1024 * 1024),
			Upper: result.PeakRSSKB.Upper / (1024 * 1024),
		}
		fmt.Fprintf(cmd.OutOrStdout(), "RSS:      ~%.1f GiB (95%% CI: %.1f – %.1f GiB)\n",
			rssGiB.Mean, rssGiB.Lower, rssGiB.Upper)
	}

	return nil
}

func buildPredictorConfig(cfg *config.Config) predictor.Config {
	return predictor.BuildConfig(
		cfg.Predictor.ProjectPath,
		cfg.Predictor.ModelDir,
		cfg.Predictor.RetrainInterval,
		cfg.Predictor.DBPaths,
	)
}

func formatRuntimeSource(source string) string {
	switch source {
	case "learned+analytical":
		return "learned + analytical"
	case "learned":
		return "learned only"
	case "empirical":
		return "empirical"
	default:
		if source == "" {
			return "unknown"
		}
		return source
	}
}
