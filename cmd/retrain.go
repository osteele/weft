package cmd

import (
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/predictor"
	"github.com/spf13/cobra"
)

var retrainCmd = &cobra.Command{
	Use:   "retrain",
	Short: "Retrain job-estimator models from historical data",
	Long: `Force retrain the job-estimator ML models using all configured job databases.

Models are stored in ~/.cache/weft/models/ and are used by placement scoring
to predict job duration and resource usage per host.

The predictor must be configured in ~/.config/weft/config.toml:
  predictor:
    project_path: /path/to/job-estimator`,
	RunE: runRetrain,
}

var retrainIfSchemaChanged bool

func runRetrain(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pcfg := buildPredictorConfig(cfg)
	if !pcfg.Configured() {
		if retrainIfSchemaChanged {
			fmt.Fprintln(cmd.OutOrStdout(), "Predictor not configured; skipping schema check.")
			return nil
		}
		return fmt.Errorf("predictor not configured: set predictor.project_path in %s", config.ConfigPath())
	}
	if retrainIfSchemaChanged {
		status := predictor.CheckModelSchema(pcfg)
		if !status.Changed {
			fmt.Fprintln(cmd.OutOrStdout(), "Predictor models already match the current schema.")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Predictor model schema changed: %s\n", status.Reason)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Training models from %d database(s)...\n", len(pcfg.DBPaths))
	for _, db := range pcfg.DBPaths {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", db)
	}

	if err := predictor.Train(pcfg); err != nil {
		return fmt.Errorf("training failed: %w", err)
	}

	meta, err := predictor.ReadMeta(pcfg)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "\nModels trained (could not read metadata: %v)\n", err)
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\nModels trained successfully:\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  Jobs:       %d\n", meta.JobCount)
	fmt.Fprintf(cmd.OutOrStdout(), "  Trained at: %s\n", meta.TrainedAt)
	fmt.Fprintf(cmd.OutOrStdout(), "  Models:     %d\n", len(meta.Models))

	return nil
}

func init() {
	rootCmd.AddCommand(retrainCmd)
	retrainCmd.Flags().BoolVar(
		&retrainIfSchemaChanged,
		"if-schema-changed",
		false,
		"Only retrain when the stored predictor model schema is incompatible with the current code",
	)
	estimationTrainCmd.Flags().BoolVar(
		&retrainIfSchemaChanged,
		"if-schema-changed",
		false,
		"Only retrain when the stored predictor model schema is incompatible with the current code",
	)
}
