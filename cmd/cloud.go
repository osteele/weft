package cmd

import "github.com/spf13/cobra"

var cloudCmd = &cobra.Command{
	Use:   "cloud",
	Short: "Watch and manage cloud-backed workload activity",
	Long: `Watch and manage cloud-backed workload activity.

Available subcommands:
  watch  Watch the full active system state`,
}

var cloudWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch cloud instances, on-prem jobs, and unplaced jobs",
	Long: `Watch the full active system state.

In an interactive terminal this launches an instance-centric TUI. In plain mode
it prints periodic summaries of cloud instances, on-prem active jobs, and
unplaced jobs.`,
	RunE: runWatchCommand,
}

func init() {
	rootCmd.AddCommand(cloudCmd)
	cloudCmd.AddCommand(cloudWatchCmd)
	configureWatchFlags(cloudWatchCmd)
}
