package cmd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Tabbed at-a-glance dashboard",
	Long: `Launch a tabbed dashboard with multiple views of the system:
  1 Pulse     · status counts + sparklines
  2 Timeline  · Gantt of running and queued jobs
  3 Fleet     · on-prem hosts and live cloud instances
  4 Focus     · cards for running jobs
  5 Tree      · jobs grouped by project, then experiment
  6 Alerts    · anomaly-first view (spend, stuck jobs, clustered failures, ...)
  7 History   · 24h instance-outcome heatmap
  8 Flow      · status → destination → provider (Sankey-style)
  9 Cost      · spend rate, top spenders, per-provider breakdown
  0 Usage     · LLM call counts, tokens, daily spend (Anthropic)

Read-only except for autopilot pause/resume (p). Usage is logged to
~/.cache/weft/dashboard-usage.log; see docs/guides/dashboard.md for details.`,
	RunE: runDashboard,
}

var (
	dashboardStartTab string
	dashboardCycle    string
)

func init() {
	rootCmd.AddCommand(dashboardCmd)
	dashboardCmd.Flags().StringVar(&dashboardStartTab, "start-tab", "", "Initial tab (1-9,0 or name: pulse/timeline/fleet/focus/tree/alerts/history/flow/cost/usage)")
	dashboardCmd.Flags().StringVar(&dashboardCycle, "cycle", "", "Start with cycle on at this interval (e.g. 15s)")
}

func runDashboard(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var opts terminal.DashboardOptions
	if dashboardStartTab != "" {
		opts.StartTabIndex = parseStartTab(dashboardStartTab)
	}
	if dashboardCycle != "" {
		if d, err := time.ParseDuration(dashboardCycle); err == nil && d > 0 {
			opts.StartCycle = d
		}
	}
	return terminal.RunDashboardTUI(database, cfg, opts)
}

func parseStartTab(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n - 1
	}
	switch s {
	case "pulse":
		return 0
	case "timeline":
		return 1
	case "fleet":
		return 2
	case "focus":
		return 3
	case "tree":
		return 4
	case "alerts":
		return 5
	case "history":
		return 6
	case "flow", "sankey":
		return 7
	case "cost":
		return 8
	case "usage":
		return 9
	}
	return 0
}
