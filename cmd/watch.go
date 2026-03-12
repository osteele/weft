package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch cloud instances, on-prem jobs, and unplaced jobs",
	Long: `Watch the full active system state.

In an interactive terminal this launches an instance-centric TUI. In plain mode
it prints periodic summaries of cloud instances, on-prem active jobs, and
unplaced jobs.`,
	RunE: runWatch,
}

var (
	watchTUI    bool
	watchPlain  bool
	watchFollow bool
)

func init() {
	rootCmd.AddCommand(watchCmd)
	watchCmd.Flags().BoolVar(&watchTUI, "tui", false, "Force interactive TUI mode")
	watchCmd.Flags().BoolVar(&watchPlain, "plain", false, "Force plain text mode")
	watchCmd.Flags().BoolVarP(&watchFollow, "follow", "f", false, "Keep printing summaries even when nothing is active")
	watchCmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func runWatch(cmd *cobra.Command, args []string) error {
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
	if err := ensureAgentFresh(); err != nil {
		return nil, "", err
	}

	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, "", fmt.Errorf("list unplaced jobs: %w", err)
	}
	if len(jobs) == 0 {
		return nil, "No jobs need rental GPUs.", nil
	}

	groups := campaign.GroupByGPUSupremum(jobs)
	for i := range groups {
		groups[i].DiskGB = campaign.EstimateGroupDisk(groups[i], database)
	}

	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return nil, "", fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")
	}

	opts := campaign.LaunchOpts{}
	if gracePeriod := cfg.DefaultGracePeriod(); gracePeriod != "0" {
		if d, err := time.ParseDuration(gracePeriod); err == nil {
			opts.GracePeriodSeconds = int(d.Seconds())
		}
	}

	finalModel, err := runLaunchProgram(database, cfg, groups, opts, "", true, true)
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

func runLaunchProgram(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, gpuFilter string, reconciling bool, fromWatch bool) (launchModel, error) {
	clients := buildCloudClients(cfg)
	predCfg := buildPredictorConfig(cfg)
	model := newLaunchModel(database, clients, cfg, groups, opts, &predCfg, gpuFilter, reconciling, fromWatch)

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
