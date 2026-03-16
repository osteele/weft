package cmd

import (
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
Models are automatically retrained when enough new jobs have completed.

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
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Duration: %s\n", predictor.FormatDuration(result.DurationS))

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
