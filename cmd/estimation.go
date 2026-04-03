package cmd

import "github.com/spf13/cobra"

var estimationCmd = &cobra.Command{
	Use:   "estimation",
	Short: "Manage the job duration and resource estimation system",
	Long: `Manage the job estimation system, which predicts job duration and
resource usage from historical data.

Subcommands:
  train   Retrain job-estimator models from historical data
  status  Show predictor model readiness and refresh state`,
}

var estimationTrainCmd = &cobra.Command{
	Use:   "train",
	Short: retrainCmd.Short,
	Long:  retrainCmd.Long,
	RunE:  runRetrain,
}

func init() {
	rootCmd.AddCommand(estimationCmd)
	estimationCmd.AddCommand(estimationTrainCmd)
}
