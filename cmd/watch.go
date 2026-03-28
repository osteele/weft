package cmd

import (
	"database/sql"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
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
	watchTUI    bool
	watchPlain  bool
	watchFollow bool
)

func init() {
	rootCmd.AddCommand(watchCmd)
	configureWatchFlags(watchCmd)
}

func configureWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&watchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&watchPlain, "plain", false, "Force plain text mode")
	cmd.Flags().BoolVarP(&watchFollow, "follow", "f", false, "Keep printing summaries even when nothing is active")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func runWatchCommand(cmd *cobra.Command, args []string) error {
	// If args look like job IDs, watch those specific jobs instead of
	// showing system-wide state.
	if len(args) > 0 {
		jobIDs, err := ParseJobIDs(args)
		if err != nil {
			return err
		}
		database, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()
		return watchJobsPlain(database, jobIDs, watchFollow)
	}

	useTUI, err := resolveCampaignTUIMode(watchTUI, watchPlain, hasCampaignTerminalIO(), inCampaignAgentContext())
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if useTUI {
		return runWatchLoop(database, cfg)
	}
	return watchAllPlain(database, cfg, watchFollow)
}

func runWatchLoop(database *sql.DB, cfg *config.Config) error {
	router := newWatchRouterModel(database, cfg, "")

	restore := logging.Suppress()
	p := tea.NewProgram(router, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	restore()

	// Clean up syncWorker from whichever model was active at exit
	if r, ok := finalModel.(watchRouterModel); ok {
		if w, ok := r.active.(watchModel); ok && w.syncWorker != nil {
			w.syncWorker.Stop()
		}
	}
	if err != nil {
		return fmt.Errorf("watch TUI error: %w", err)
	}
	return nil
}

func runLaunchProgram(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, gpuFilter string, reconciling bool, fromWatch bool, inlineWatchEnabled bool) (launchModel, error) {
	clients, providerErr := buildCloudClients(cfg)
	if providerErr != nil {
		return launchModel{err: providerErr}, nil
	}
	predCfg := buildPredictorConfig(cfg)
	model := newLaunchModel(database, clients, nil, cfg, groups, opts, &predCfg, gpuFilter, reconciling, fromWatch, inlineWatchEnabled)

	restore := logging.Suppress()
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	restore()
	if err != nil {
		return launchModel{}, fmt.Errorf("launch TUI error: %w", err)
	}

	m, ok := finalModel.(launchModel)
	if !ok {
		return launchModel{}, nil
	}
	return m, nil
}
