package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
	"github.com/spf13/cobra"
)

var campaignCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Manage cloud GPU campaigns (batches of instances)",
}

var campaignLaunchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Interactively select and launch cloud instances for unplaceable jobs",
	Long: `Shows all needs_rental jobs grouped by GPU class with checkboxes.
Select/deselect jobs, view cost estimates, and launch instances.

Use --dry-run to just print the plan without launching.`,
	RunE: runCampaignLaunch,
}

var campaignWatchCmd = &cobra.Command{
	Use:   "watch <instance-id> [instance-id...]",
	Short: "Watch cloud instance progress",
	Long: `Monitors one or more cloud instances, printing line-oriented status updates.

Use --tui for an interactive display.`,
	Args: cobra.MinimumNArgs(1),
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
	campaignLaunchMaxSpend string
	campaignLaunchMaxTime  string
	campaignLaunchDryRun   bool
	campaignLaunchNoWatch  bool
	campaignLaunchYes      bool
	campaignLaunchJobs     string
	campaignWatchTUI       bool
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
	campaignLaunchCmd.Flags().BoolVarP(&campaignLaunchYes, "yes", "y", false, "Non-interactive: launch all groups without TUI confirmation")
	campaignLaunchCmd.Flags().StringVar(&campaignLaunchJobs, "jobs", "", "Comma-separated job IDs to include (default: all needs_rental jobs)")

	campaignWatchCmd.Flags().BoolVar(&campaignWatchTUI, "tui", false, "Use interactive TUI display")
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
	if len(groups) == 0 {
		fmt.Println("No jobs need rental GPUs.")
		return nil
	}

	// Parse budget limits
	opts := parseLaunchOpts()

	// Dry run: print plan table
	if campaignLaunchDryRun {
		return runDryRunPlan(cfg, groups)
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
	client := vastai.NewClient()
	model := newLaunchModel(database, client, cfg, groups, opts)
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
	client := vastai.NewClient()
	if err := client.Available(); err != nil {
		return fmt.Errorf("vastai CLI not available: %w", err)
	}

	// Fetch offers in parallel
	fmt.Println("Searching for GPU offers...")
	groupOffers := campaign.FetchGroupOffers(client, groups)

	// Print plan summary with cost estimates
	totalJobs := 0
	for _, g := range groups {
		totalJobs += len(g.Jobs)
	}
	fmt.Printf("%d jobs in %d GPU groups\n", totalJobs, len(groups))

	predCfg := buildPredictorConfig(cfg)
	estimates := campaign.EstimateCosts(groupOffers, &predCfg)
	fmt.Println(campaign.FormatCostTableWithEstimates(estimates))

	// Check all groups have offers
	for _, go_ := range groupOffers {
		if go_.Err != nil {
			return fmt.Errorf("offer search failed for %s: %w", go_.Group.GPUSpec(), go_.Err)
		}
		if go_.Offer == nil {
			return fmt.Errorf("no offers found for %s", go_.Group.GPUSpec())
		}
	}

	r2Cfg := vastai.R2Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	}

	createOpts := vastai.CreateOpts{
		Image:      cfg.Vastai.DefaultImage,
		DiskGB:     50,
		SSHEnabled: true,
	}
	if createOpts.Image == "" {
		createOpts.Image = vastai.DefaultImage
	}

	// Create campaign batch
	campaignRec := &db.Campaign{
		Status: db.CampaignStatusLaunching,
	}
	campaignID, err := db.CreateCampaign(database, campaignRec)
	if err != nil {
		return fmt.Errorf("create campaign: %w", err)
	}

	// Launch instances in parallel
	fmt.Printf("Launching %d instance(s)...\n", len(groups))
	var mu sync.Mutex
	var instanceIDs []int64
	var launchErrors []error
	var wg sync.WaitGroup

	for i, g := range groups {
		offer := *groupOffers[i].Offer

		wg.Add(1)
		go func(group campaign.InstanceGroup, ofr vastai.Offer) {
			defer wg.Done()

			cID, err := campaign.LaunchInstance(
				client, database, &campaignID, group, ofr, opts, r2Cfg, createOpts,
				func(phase string) {
					mu.Lock()
					fmt.Printf("  %s: %s\n", group.GPUSpec(), phase)
					mu.Unlock()
				},
			)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
			} else {
				instanceIDs = append(instanceIDs, cID)
			}
		}(g, offer)
	}

	wg.Wait()

	// Update campaign status
	if len(instanceIDs) == 0 && len(launchErrors) > 0 {
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return launchErrors[0]
	}
	_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusRunning)

	// Print results
	for _, e := range launchErrors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
	}
	fmt.Printf("Campaign %d: launched %d instance(s)\n", campaignID, len(instanceIDs))
	for _, id := range instanceIDs {
		fmt.Printf("  instance %d\n", id)
	}

	// Print next steps
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  weft campaign watch %d\n", campaignID)
	for _, id := range instanceIDs {
		fmt.Printf("  weft instance ssh %d\n", id)
	}

	// Segue into watch mode
	if !campaignLaunchNoWatch && term.IsTerminal(os.Stdout.Fd()) {
		fmt.Println()
		return watchInstances(database, instanceIDs)
	}

	return nil
}

func runDryRunPlan(cfg *config.Config, groups []campaign.InstanceGroup) error {
	client := vastai.NewClient()
	groupOffers := campaign.FetchGroupOffers(client, groups)

	predCfg := buildPredictorConfig(cfg)
	estimates := campaign.EstimateCosts(groupOffers, &predCfg)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "GROUP\tGPU\tJOBS\tJOB IDS\tMEM\tCOST/HR\tEST TIME\tEST COST\n")

	for i, est := range estimates {
		go_ := groupOffers[i]
		jobIDs := campaign.FormatJobIDs(go_.Group.Jobs, 5)

		gpuStr := go_.Group.GPUSpec()
		memStr := "—"
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

		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			i+1, gpuStr, len(go_.Group.Jobs), jobIDs, memStr, costStr, durStr, estCostStr)
	}
	w.Flush()

	total := campaign.TotalEstimatedCostFromEstimates(estimates)
	fmt.Printf("\nEstimated total: ~$%.2f\n", total)
	fmt.Println("To launch interactively: weft campaign launch")
	return nil
}

func runCampaignWatch(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var instanceIDs []int64
	for _, arg := range args {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		instanceIDs = append(instanceIDs, id)
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
		fmt.Fprintf(w, "    ID\tSTATUS\tGPU SPEC\tVASTAI ID\n")
		for _, inst := range instances {
			gpuSpec := inst.GPUSpec
			if gpuSpec == "" {
				gpuSpec = inst.GPUClass
			}
			vastaiID := inst.VastaiInstanceID
			if vastaiID == "" {
				vastaiID = "—"
			}
			fmt.Fprintf(w, "    %d\t%s\t%s\t%s\n", inst.ID, inst.Status, gpuSpec, vastaiID)
		}
		w.Flush()
	}

	return nil
}

func parseLaunchOpts() campaign.LaunchOpts {
	opts := campaign.LaunchOpts{}
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
	return opts
}
