package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/bidding"
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
	flashMessage := ""

	for {
		model := newWatchAllModel(database, cfg, flashMessage)

		origLogOutput := log.Writer()
		log.SetOutput(io.Discard)
		p := tea.NewProgram(model, tea.WithAltScreen())
		finalModel, err := p.Run()
		log.SetOutput(origLogOutput)
		if m, ok := finalModel.(watchAllModel); ok && m.syncWorker != nil {
			m.syncWorker.Stop()
		}
		if err != nil {
			return fmt.Errorf("watch TUI error: %w", err)
		}

		m, ok := finalModel.(watchAllModel)
		if !ok {
			return nil
		}
		if m.exitAction != watchExitLaunch {
			return nil
		}

		flashMessage = ""
		instanceIDs, message, err := runWatchLaunchPlanner(database, cfg)
		if err != nil {
			flashMessage = fmt.Sprintf("Launch error: %v", err)
			continue
		}
		if len(instanceIDs) > 0 {
			flashMessage = message
			continue
		}
		flashMessage = message
	}
}

func runWatchLaunchPlanner(database *sql.DB, cfg *config.Config) ([]int64, string, error) {
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, "", fmt.Errorf("list unplaced jobs: %w", err)
	}
	if len(jobs) == 0 {
		return nil, "No jobs need rental GPUs.", nil
	}

	groups := campaign.GroupByGPUSupremum(jobs)
	r2Client, err := buildR2Client(cfg)
	if err != nil {
		log.Printf("warning: build R2 client for disk estimation: %v", err)
	}
	for i := range groups {
		groups[i].DiskGB = campaign.EstimateGroupDisk(groups[i], database, r2Client)
	}

	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return nil, "", fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")
	}

	opts := campaign.LaunchOpts{Strategy: bidding.StrategyCheap}
	if gracePeriod := cfg.DefaultGracePeriod(); gracePeriod != "0" {
		if d, err := time.ParseDuration(gracePeriod); err == nil {
			opts.GracePeriodSeconds = int(d.Seconds())
		}
	}

	finalModel, err := runLaunchProgram(database, cfg, groups, opts, "", true, true, true)
	if err != nil {
		return nil, "", err
	}
	if finalModel.err != nil {
		return nil, "", finalModel.err
	}
	if len(finalModel.instanceIDs) == 0 {
		return nil, "Launch canceled.", nil
	}

	instanceText := make([]string, 0, len(finalModel.instanceIDs))
	for _, id := range finalModel.instanceIDs {
		instanceText = append(instanceText, fmt.Sprintf("%d", id))
	}
	message := "Launched instances: " + strings.Join(instanceText, ", ")
	if len(finalModel.partialErrors) > 0 {
		message = fmt.Sprintf("%s (%d failed)", message, len(finalModel.partialErrors))
	}
	return finalModel.instanceIDs, message, nil
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
