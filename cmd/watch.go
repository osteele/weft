package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch [job-id... | jobs | campaign | instance | system | project]",
	Short: "Watch jobs, instances, campaigns, or projects",
	Long: `Watch active system state.

With job IDs, watches those specific jobs until they reach a terminal state.
Without arguments, watches all cloud instances and on-prem jobs (same as
"weft watch instance" or "weft watch system"). Use a subcommand to watch a
specific resource type.

Subcommands:
  jobs        Watch job status changes (TUI or plain)
  campaign    Watch a campaign
  instance    Watch cloud instances
  system      Watch all system state (alias for instance)
  project     Watch a project`,
	RunE: runWatchCommand,
}

var (
	watchTUI              bool
	watchPlain            bool
	watchFollow           bool
	watchAuto             bool
	watchTransitionsOnly  bool
	watchJSONLines        bool
	watchUntilAnyTerminal bool
)

func init() {
	rootCmd.AddCommand(watchCmd)
	configureWatchFlags(watchCmd)
}

func configureWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&watchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&watchPlain, "plain", false, "Force plain text mode")
	cmd.Flags().BoolVarP(&watchFollow, "follow", "f", false, "Keep printing summaries even when nothing is active")
	cmd.Flags().BoolVar(&watchAuto, "auto", false, "Start with auto-pilot enabled (auto-relaunch, auto-place, auto-launch)")
	addWatchEventFlags(cmd)
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

// addWatchEventFlags registers the streaming/transition flags shared by all
// plain-mode watch commands and the related mutual-exclusivity rules.
// Callers must register --tui and --follow before calling this helper.
func addWatchEventFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&watchTransitionsOnly, "transitions-only", false, "Emit one line per status change instead of full snapshots")
	cmd.Flags().BoolVar(&watchJSONLines, "jsonl", false, "Emit JSON Lines output (one JSON object per line)")
	cmd.Flags().BoolVar(&watchUntilAnyTerminal, "until-any-terminal", false, "Exit on the first terminal status transition")
	cmd.MarkFlagsMutuallyExclusive("follow", "until-any-terminal")
	if cmd.Flags().Lookup("tui") != nil {
		cmd.MarkFlagsMutuallyExclusive("tui", "transitions-only")
		cmd.MarkFlagsMutuallyExclusive("tui", "jsonl")
	}
}

// watchPlainOptions assembles the plain-mode watch options from the shared
// flag globals.
func watchPlainOptions() terminal.WatchPlainOptions {
	return terminal.WatchPlainOptions{
		Follow:           watchFollow,
		TransitionsOnly:  watchTransitionsOnly,
		JSONLines:        watchJSONLines,
		UntilAnyTerminal: watchUntilAnyTerminal,
	}
}

func runWatchCommand(cmd *cobra.Command, args []string) error {
	// If args look like job IDs, watch those specific jobs instead of
	// showing system-wide state.
	if len(args) > 0 {
		jobIDs, err := ParseJobIDs(args)
		if err != nil {
			return err
		}
		database, err := db.OpenForReading()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()
		return terminal.WatchJobsPlain(database, jobIDs, watchPlainOptions())
	}

	useTUI, err := resolveCampaignTUIMode(watchTUI, watchPlain, hasCampaignTerminalIO(), inCampaignAgentContext())
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	agentdeploy.StartBackgroundPrewarm("linux", "amd64")

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if useTUI {
		return runWatchLoop(database, cfg, watchAuto)
	}
	return terminal.WatchAllPlain(database, cfg, watchPlainOptions())
}

func runWatchLoop(database *sql.DB, cfg *config.Config, autoMode bool) error {
	return terminal.RunWatchLoop(database, cfg, autoMode)
}

func runLaunchProgram(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, gpuFilter string, reconciling bool, fromWatch bool, inlineWatchEnabled bool) (terminal.LaunchResult, error) {
	return terminal.RunLaunchProgram(database, cfg, groups, opts, gpuFilter, reconciling, fromWatch, inlineWatchEnabled)
}
