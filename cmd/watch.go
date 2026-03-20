package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"log"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch [campaign|instance|project]",
	Short: "Watch instances, campaigns, or projects",
	Long: `Watch active system state.

Without a subcommand, watches all cloud instances and on-prem jobs (same as
"weft watch instance"). Use a subcommand to watch a specific resource type.`,
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

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	p := tea.NewProgram(router, tea.WithAltScreen())
	finalModel, err := p.Run()
	log.SetOutput(origLogOutput)

	// Clean up syncWorker from whichever model was active at exit
	if r, ok := finalModel.(watchRouterModel); ok {
		if w, ok := r.active.(watchAllModel); ok && w.syncWorker != nil {
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

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	p := tea.NewProgram(model, tea.WithAltScreen())
	finalModel, err := p.Run()
	log.SetOutput(origLogOutput)
	if err != nil {
		return launchModel{}, fmt.Errorf("launch TUI error: %w", err)
	}

	m, ok := finalModel.(launchModel)
	if !ok {
		return launchModel{}, nil
	}
	return m, nil
}
