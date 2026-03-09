package cmd

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/spf13/cobra"
)

var campaignCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Manage cloud GPU campaigns (batches of instances)",
	RunE:  runCampaignAutoRoute,
}

// runCampaignAutoRoute routes bare `weft campaign` to list if there are
// active campaigns, or to launch otherwise.
func runCampaignAutoRoute(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}

	for _, c := range campaigns {
		if c.Status == db.CampaignStatusRunning || c.Status == db.CampaignStatusLaunching {
			return runCampaignList(cmd, args)
		}
	}

	return runCampaignLaunch(cmd, args)
}

var campaignLaunchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Interactively select and launch cloud instances for unplaceable jobs",
	Long: `Shows jobs whose GPU constraints can't be satisfied by on-prem hosts, grouped by GPU class with checkboxes.
Select/deselect jobs, view cost estimates, and launch instances.

Use --dry-run to just print the plan without launching.`,
	RunE: runCampaignLaunch,
}

var campaignWatchCmd = &cobra.Command{
	Use:   "watch <campaign-id>",
	Short: "Watch campaign instance progress",
	Long: `Monitors all instances in a campaign, printing line-oriented status updates.

Use --tui for an interactive display.`,
	Args: cobra.ExactArgs(1),
	RunE: runCampaignWatch,
}

var campaignTerminateCmd = &cobra.Command{
	Use:     "terminate <campaign-id>",
	Aliases: []string{"cancel"},
	Short:   "Terminate all instances in a campaign",
	Args:    cobra.ExactArgs(1),
	RunE:    runCampaignTerminate,
}

var campaignListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all campaigns with instance counts",
	RunE:  runCampaignList,
}

var campaignShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show details of a campaign",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runCampaignShow,
}

var (
	campaignLaunchMaxSpend    string
	campaignLaunchMaxTime     string
	campaignLaunchGracePeriod string
	campaignLaunchDryRun      bool
	campaignLaunchNoWatch     bool
	campaignLaunchNoDonor     bool
	campaignLaunchYes         bool
	campaignLaunchJobs        string
	campaignLaunchGPU         string
	campaignWatchTUI          bool
)

func init() {
	rootCmd.AddCommand(campaignCmd)
	campaignCmd.AddCommand(campaignLaunchCmd)
	campaignCmd.AddCommand(campaignWatchCmd)
	campaignCmd.AddCommand(campaignTerminateCmd)
	campaignCmd.AddCommand(campaignListCmd)
	campaignCmd.AddCommand(campaignShowCmd)

	campaignLaunchCmd.Flags().StringVar(&campaignLaunchMaxSpend, "max-spend", "", "Maximum spend per instance (e.g., '$5.00')")
	campaignLaunchCmd.Flags().StringVar(&campaignLaunchMaxTime, "max-time", "", "Maximum time per instance (e.g., '2h')")
	campaignLaunchCmd.Flags().BoolVar(&campaignLaunchDryRun, "dry-run", false, "Print plan table and exit without launching")
	campaignLaunchCmd.Flags().BoolVar(&campaignLaunchNoWatch, "no-watch", false, "Launch and exit immediately (print instance IDs only)")
	campaignLaunchCmd.Flags().BoolVar(&campaignLaunchNoDonor, "no-donor", false, "Skip donor instance strategy (each instance downloads independently)")
	campaignLaunchCmd.Flags().BoolVarP(&campaignLaunchYes, "yes", "y", false, "Non-interactive: launch all groups without TUI confirmation")
	campaignLaunchCmd.Flags().StringVar(&campaignLaunchJobs, "jobs", "", "Comma-separated job IDs to include (default: all needs_rental jobs)")
	campaignLaunchCmd.Flags().StringVar(&campaignLaunchGPU, "gpu", "", "Filter by GPU class (e.g., 'RTX_4090', 'A100')")
	campaignLaunchCmd.Flags().StringVar(&campaignLaunchGracePeriod, "grace-period", "15m", "Keep instance alive after job failure (e.g., '15m', '1h'; '0' to disable)")

	campaignWatchCmd.Flags().BoolVar(&campaignWatchTUI, "tui", false, "Use interactive TUI display")
	campaignWatchCmd.Flags().Bool("plain", false, "Plain text output (default; accepted for clarity)")
	_ = campaignWatchCmd.Flags().MarkHidden("plain")
}

func runCampaignLaunch(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	jobs, err := db.ListNeedsRentalJobs(database)
	if err != nil {
		return fmt.Errorf("list needs_rental jobs: %w", err)
	}

	// Filter by --jobs if specified
	if campaignLaunchJobs != "" {
		jobFilter := make(map[int64]bool)
		for _, idStr := range strings.Split(campaignLaunchJobs, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job ID %q: %w", idStr, err)
			}
			jobFilter[id] = true
		}
		var filtered []*db.Job
		for _, j := range jobs {
			if jobFilter[j.ID] {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}

	if len(jobs) == 0 {
		fmt.Println("No jobs need rental GPUs.")
		return nil
	}

	groups := campaign.GroupByGPUSupremum(jobs)

	// Filter by --gpu if specified
	if campaignLaunchGPU != "" {
		var filtered []campaign.InstanceGroup
		for _, g := range groups {
			if strings.EqualFold(g.GPUClass, campaignLaunchGPU) {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}

	if len(groups) == 0 {
		if campaignLaunchGPU != "" {
			fmt.Printf("No needs_rental jobs match GPU class %q.\n", campaignLaunchGPU)
		} else {
			fmt.Println("No jobs need rental GPUs.")
		}
		return nil
	}

	// Estimate disk needs from HF model inputs
	for i := range groups {
		groups[i].DiskGB = campaign.EstimateGroupDisk(groups[i], database)
	}

	// Parse budget limits
	opts := parseLaunchOpts()

	// Dry run: print plan table
	if campaignLaunchDryRun {
		return runDryRunPlan(database, cfg, groups)
	}

	// Check R2 config before entering interactive mode
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return fmt.Errorf("R2 not configured in ~/.config/weft/config.yaml (vastai.r2)")
	}

	// Non-interactive mode
	if campaignLaunchYes {
		return runNonInteractiveLaunch(database, cfg, groups, opts)
	}

	// Interactive TUI
	clients := buildCloudClients(cfg)
	predCfg := buildPredictorConfig(cfg)
	model := newLaunchModel(database, clients, cfg, groups, opts, &predCfg)
	p := tea.NewProgram(model)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	m := finalModel.(launchModel)
	if m.err != nil {
		return m.err
	}

	// Segue into watch mode if instances were launched
	if len(m.instanceIDs) > 0 && !campaignLaunchNoWatch {
		fmt.Println()
		return watchInstances(database, m.instanceIDs)
	}

	return nil
}

// runNonInteractiveLaunch launches all groups without TUI interaction.
func runNonInteractiveLaunch(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts) error {
	clients := buildCloudClients(cfg)
	if len(clients) == 0 {
		return fmt.Errorf("no cloud providers available (check vastai/runpod CLI)")
	}

	// Fetch offers in parallel
	fmt.Println("Searching for GPU offers...")
	groupOffers := campaign.FetchGroupOffers(clients, groups)

	// Print plan summary with cost estimates
	totalJobs := 0
	for _, g := range groups {
		totalJobs += len(g.Jobs)
	}
	fmt.Printf("%d jobs in %d GPU groups\n", totalJobs, len(groups))

	predCfg := buildPredictorConfig(cfg)
	overheadModel := buildOverheadModel(database)
	estimates := campaign.EstimateCosts(groupOffers, &predCfg, overheadModel, nil, nil)
	fmt.Println(campaign.FormatCostTableWithEstimates(estimates))

	// Auto-derive budget limits from estimates if not set by CLI
	if opts.ApplyAutoBudget(estimates) {
		printAutoBudget(opts)
	}

	// Check all groups have offers
	var offers []cloud.Offer
	for _, go_ := range groupOffers {
		if go_.Err != nil {
			return fmt.Errorf("offer search failed for %s: %w", go_.Group.GPUSpec(), go_.Err)
		}
		if go_.Offer == nil {
			return fmt.Errorf("no offers found for %s", go_.Group.GPUSpec())
		}
		offers = append(offers, *go_.Offer)
	}

	r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
	createOpts := cloud.DefaultCreateOpts(cfg.Vastai.DefaultImage)

	fmt.Printf("Launching %d instance(s)...\n", len(groups))

	result, err := campaign.LaunchCampaign(
		clients, database, groups, offers, estimates, opts, r2Cfg, createOpts,
		func(group campaign.InstanceGroup, phase string) {
			fmt.Printf("  %s: %s\n", group.GPUSpec(), phase)
		},
	)
	if err != nil {
		return err
	}

	if len(result.InstanceIDs) == 0 && len(result.Errors) > 0 {
		return result.Errors[0]
	}

	// Print results
	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
	}
	fmt.Printf("Campaign %d: launched %d instance(s)\n", result.CampaignID, len(result.InstanceIDs))
	for _, id := range result.InstanceIDs {
		fmt.Printf("  instance %d\n", id)
	}

	// Print next steps
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  weft campaign watch %d\n", result.CampaignID)

	// Segue into watch mode
	if !campaignLaunchNoWatch && term.IsTerminal(os.Stdout.Fd()) {
		fmt.Println()
		return watchInstances(database, result.InstanceIDs)
	}

	return nil
}

func runDryRunPlan(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup) error {
	clients := buildCloudClients(cfg)
	groupOffers := campaign.FetchGroupOffers(clients, groups)

	predCfg := buildPredictorConfig(cfg)
	overheadModel := buildOverheadModel(database)
	estimates := campaign.EstimateCosts(groupOffers, &predCfg, overheadModel, nil, nil)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "GROUP\tGPU\tJOBS\tJOB IDS\tMEM\tDISK\tCOST/HR\tEST TIME\tEST COST\n")

	for i, est := range estimates {
		go_ := groupOffers[i]
		jobIDs := campaign.FormatJobIDs(go_.Group.Jobs, 5)

		gpuStr := go_.Group.GPUSpec()
		memStr := "—"
		diskStr := fmt.Sprintf("%dGB", go_.Group.DiskGB)
		costStr := "—"
		durStr := "—"
		estCostStr := "—"
		if go_.Err != nil {
			gpuStr = fmt.Sprintf("%s (error: %v)", go_.Group.GPUSpec(), go_.Err)
		} else if go_.Offer != nil {
			gpuStr = campaign.FormatResolvedGPU(go_.Group.GPUSpec(), go_.Offer.GPUName)
			memStr = fmt.Sprintf("%dGB", int(go_.Offer.GPUMemGB))
			costStr = fmt.Sprintf("$%.2f/hr", go_.Offer.CostPerHour)
			durStr = campaign.FormatEstDuration(est.TotalTime, est.HasPrediction)
			estCostStr = fmt.Sprintf("~$%.2f", est.TotalCost)
		} else {
			gpuStr = fmt.Sprintf("%s (no offers)", go_.Group.GPUSpec())
		}

		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			i+1, gpuStr, len(go_.Group.Jobs), jobIDs, memStr, diskStr, costStr, durStr, estCostStr)
	}
	w.Flush()

	total := campaign.TotalEstimatedCostFromEstimates(estimates)
	fmt.Printf("\nEstimated total: ~$%.2f\n", total)
	fmt.Println("To launch interactively: weft campaign launch")
	return nil
}

func runCampaignWatch(cmd *cobra.Command, args []string) error {
	campaignID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid campaign ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}
	if len(instances) == 0 {
		return fmt.Errorf("campaign %d has no instances", campaignID)
	}

	var instanceIDs []int64
	for _, inst := range instances {
		instanceIDs = append(instanceIDs, inst.ID)
	}

	if campaignWatchTUI && term.IsTerminal(os.Stdout.Fd()) {
		return watchInstances(database, instanceIDs)
	}

	return watchInstancesPlain(database, instanceIDs)
}

func runCampaignTerminate(cmd *cobra.Command, args []string) error {
	campaignID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid campaign ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get all instances for this campaign
	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}
	if len(instances) == 0 {
		return fmt.Errorf("campaign %d has no instances", campaignID)
	}

	// Collect instance IDs and terminate in parallel
	var ids []int64
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}

	terminated, errors := terminateInstancesParallel(database, ids)
	for _, e := range errors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
	}

	// Update campaign status
	if err := db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusCancelled); err != nil {
		return fmt.Errorf("update campaign status: %w", err)
	}

	fmt.Printf("Cancelled campaign %d, terminated %d instances\n", campaignID, terminated)
	return nil
}

func runCampaignList(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database)

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}

	if len(campaigns) == 0 {
		fmt.Println("No campaigns.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tINSTANCES\tCREATED\n")

	for _, c := range campaigns {
		created := time.Unix(c.CreatedAt, 0).Format("01/02 15:04")

		instances, _ := db.GetCampaignInstances(database, c.ID)

		fmt.Fprintf(w, "%d\t%s\t%d\t%s\n",
			c.ID, c.Status, len(instances), created)
	}
	w.Flush()
	return nil
}

func runCampaignShow(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database)

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid campaign ID %q: %w", args[0], err)
	}

	c, err := db.GetCampaign(database, id)
	if err != nil {
		return fmt.Errorf("get campaign: %w", err)
	}
	if c == nil {
		return fmt.Errorf("campaign %d not found", id)
	}

	fmt.Printf("Campaign %d\n", c.ID)
	fmt.Printf("  Status:     %s\n", c.Status)
	fmt.Printf("  Created:    %s\n", time.Unix(c.CreatedAt, 0).Format(time.RFC3339))
	if c.EndedAt != nil {
		fmt.Printf("  Ended:      %s\n", time.Unix(*c.EndedAt, 0).Format(time.RFC3339))
	}

	// Show instances
	instances, err := db.GetCampaignInstances(database, c.ID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}

	if len(instances) > 0 {
		fmt.Printf("\n  Instances (%d):\n", len(instances))
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "    ID\tROLE\tSTATUS\tGPU SPEC\tPROVIDER ID\tTIMING\n")
		for _, inst := range instances {
			gpuSpec := inst.GPUSpec
			if gpuSpec == "" {
				gpuSpec = inst.GPUClass
			}
			providerID := inst.EffectiveProviderID()
			if providerID == "" {
				providerID = "—"
			}
			role := inst.InstanceRole
			if role == "" {
				role = "worker"
			}
			timing := "—"
			if inst.SeedDownloadSecs != nil {
				timing = fmt.Sprintf("download: %ds", *inst.SeedDownloadSecs)
			} else if inst.SeedCopySecs != nil {
				timing = fmt.Sprintf("copy: %ds", *inst.SeedCopySecs)
			}
			fmt.Fprintf(w, "    %d\t%s\t%s\t%s\t%s\t%s\n", inst.ID, role, inst.Status, gpuSpec, providerID, timing)
		}
		w.Flush()
	}

	return nil
}

func parseLaunchOpts() campaign.LaunchOpts {
	opts := campaign.LaunchOpts{
		NoDonor: campaignLaunchNoDonor,
	}
	if campaignLaunchMaxSpend != "" {
		cleaned := strings.TrimPrefix(campaignLaunchMaxSpend, "$")
		var dollars float64
		if _, err := fmt.Sscanf(cleaned, "%f", &dollars); err == nil {
			opts.MaxSpendCents = int(dollars * 100)
		}
	}
	if campaignLaunchMaxTime != "" {
		if d, err := time.ParseDuration(campaignLaunchMaxTime); err == nil {
			opts.MaxTimeSeconds = int(d.Seconds())
		}
	}
	if campaignLaunchGracePeriod != "" && campaignLaunchGracePeriod != "0" {
		if d, err := time.ParseDuration(campaignLaunchGracePeriod); err == nil {
			opts.GracePeriodSeconds = int(d.Seconds())
		}
	}
	return opts
}

// reconcileBeforeDisplay checks running cloud instances against the provider
// and marks dead ones as failed, then auto-closes campaigns where all instances
// are terminal. Called before displaying campaign data.
func reconcileBeforeDisplay(database *sql.DB) {
	cfg, _ := config.Load()
	if clients := buildCloudClients(cfg); len(clients) > 0 {
		// Build R2 client for completion detection (nil if unconfigured)
		r2Client, _ := buildR2Client(cfg)
		if n, err := campaign.ReconcileCloudInstances(database, clients, r2Client); err != nil {
			log.Printf("reconcile: %v", err)
		} else if n > 0 {
			fmt.Printf("Reconciled %d dead instance(s)\n", n)
		}
	}
	// Sync cloud job results from R2 (completed/failed markers)
	syncCloudJobResults(cfg, database, false)

	if err := campaign.ReconcileCampaigns(database); err != nil {
		log.Printf("reconcile campaigns: %v", err)
	}
}

// buildOverheadModel queries historical cloud instance data and builds a
// Bayesian overhead model. Returns nil if no data or on error.
func buildOverheadModel(database *sql.DB) *estimate.OverheadModel {
	obs, err := db.QueryOverheadObservations(database)
	if err != nil {
		log.Printf("warning: could not query overhead observations: %v", err)
		return nil
	}
	return estimate.BuildOverheadModel(obs)
}

// printAutoBudget prints the auto-derived budget limits.
func printAutoBudget(opts campaign.LaunchOpts) {
	if opts.MaxSpendCents > 0 {
		fmt.Printf("Auto budget: max spend $%.2f (10× estimate, min $20)\n", float64(opts.MaxSpendCents)/100)
	}
	if opts.MaxTimeSeconds > 0 {
		d := time.Duration(opts.MaxTimeSeconds) * time.Second
		fmt.Printf("Auto budget: max time %s (10× estimate, min 8h)\n", d.Round(time.Minute))
	}
}
