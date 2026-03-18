package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/spf13/cobra"
)

var campaignCmd = &cobra.Command{
	Use:     "campaign",
	Aliases: []string{"campaigns"},
	Short:   "Manage cloud GPU campaigns (batches of instances)",
}

var campaignLaunchCmd = &cobra.Command{
	Use:     "launch",
	Aliases: []string{"run", "start"},
	Short:   "Interactively select and launch cloud instances for unplaceable jobs",
	Long: `Shows jobs whose GPU constraints can't be satisfied by on-prem hosts, grouped by GPU class with checkboxes.
Select/deselect jobs, view cost estimates, and launch instances.

Use --dry-run to just print the plan without launching.`,
	RunE: runCampaignLaunch,
}

var campaignWatchCmd = &cobra.Command{
	Use:   "watch [campaign-id]",
	Short: "Watch campaign instance progress",
	Long: `Monitors all instances in a campaign.

Defaults to TUI in an interactive terminal, otherwise plain text.
If no campaign ID is provided, watches the most recent campaign.
Use --tui or --plain to override.`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
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
	campaignLaunchStrategy    string
	campaignLaunchTUI         bool
	campaignLaunchPlain       bool
	campaignWatchTUI          bool
	campaignWatchPlain        bool
	campaignListTUI           bool
	campaignListPlain         bool
)

func init() {
	rootCmd.AddCommand(campaignCmd)
	campaignCmd.AddCommand(campaignLaunchCmd)
	campaignCmd.AddCommand(campaignWatchCmd)
	campaignCmd.AddCommand(campaignDiagnoseCmd)
	campaignCmd.AddCommand(campaignTerminateCmd)
	campaignCmd.AddCommand(campaignListCmd)
	campaignCmd.AddCommand(campaignShowCmd)
	campaignCmd.AddCommand(campaignStatsCmd)

	addCampaignLaunchFlags(campaignLaunchCmd)

	addCampaignWatchFlags(campaignWatchCmd)
	addCampaignListFlags(campaignListCmd)
}

func addCampaignLaunchFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&campaignLaunchMaxSpend, "max-spend", "", "Maximum spend per instance (e.g., '$5.00')")
	cmd.Flags().StringVar(&campaignLaunchMaxTime, "max-time", "", "Maximum time per instance (e.g., '2h')")
	cmd.Flags().BoolVar(&campaignLaunchDryRun, "dry-run", false, "Print plan table and exit without launching")
	cmd.Flags().BoolVar(&campaignLaunchNoWatch, "no-watch", false, "Launch and exit immediately (print instance IDs only)")
	cmd.Flags().BoolVar(&campaignLaunchNoDonor, "no-donor", false, "Skip donor instance strategy (each instance downloads independently)")
	cmd.Flags().BoolVarP(&campaignLaunchYes, "yes", "y", false, "Non-interactive: launch all groups without TUI confirmation")
	cmd.Flags().StringVar(&campaignLaunchJobs, "jobs", "", "Comma-separated job IDs to include (default: all unplaced jobs)")
	cmd.Flags().StringVar(&campaignLaunchGPU, "gpu", "", "Filter by GPU class (e.g., 'RTX_4090', 'A100')")
	cmd.Flags().StringVar(&campaignLaunchGracePeriod, "grace-period", "", "Keep instance alive after job failure (default from config, e.g., '5m', '1h'; '0' to disable)")
	cmd.Flags().StringVar(&campaignLaunchStrategy, "strategy", "cost", "Offer selection strategy: 'cost' (minimize expected cost) or 'fast' (minimize wall-clock time)")
	cmd.Flags().BoolVar(&campaignLaunchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&campaignLaunchPlain, "plain", false, "Force plain non-interactive mode")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
	cmd.MarkFlagsMutuallyExclusive("tui", "yes")
}

func addCampaignWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&campaignWatchTUI, "tui", false, "Force interactive TUI display")
	cmd.Flags().BoolVar(&campaignWatchPlain, "plain", false, "Force plain text output")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func addCampaignListFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&campaignListTUI, "tui", false, "Force interactive list with drill-down to watch")
	cmd.Flags().BoolVar(&campaignListPlain, "plain", false, "Force plain table output")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func runCampaignLaunch(cmd *cobra.Command, args []string) error {
	useTUI, err := resolveCampaignTUI(campaignLaunchTUI, campaignLaunchPlain)
	if err != nil {
		return err
	}
	launchInteractive := useTUI && !campaignLaunchYes

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// For non-interactive and dry-run modes, reconcile synchronously.
	// For TUI mode, reconciliation runs in the background (see below).
	needsSyncReconcile := !launchInteractive || campaignLaunchDryRun
	if needsSyncReconcile {
		for _, warning := range reconcileBeforeDisplay(database, FastCloudSyncTimeout) {
			fmt.Fprintln(cmd.ErrOrStderr(), warning)
		}
	}

	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return fmt.Errorf("list unplaced jobs: %w", err)
	}
	jobs = filterRentalLaunchJobs(jobs)

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

	groups = campaign.FilterByGPUClass(groups, campaignLaunchGPU)

	if len(groups) == 0 {
		if campaignLaunchGPU != "" {
			fmt.Printf("No unplaced jobs match GPU class %q.\n", campaignLaunchGPU)
		} else {
			fmt.Println("No jobs need rental GPUs.")
		}
		return nil
	}

	// Check for reusable cloud instances (grace or running)
	reusable, _ := campaign.FindReusableInstances(database)
	var reuseAssignments []campaign.ReuseAssignment
	if len(reusable) > 0 {
		// Flatten all jobs for reuse matching
		var allJobs []*db.Job
		for _, g := range groups {
			allJobs = append(allJobs, g.Jobs...)
		}
		var remainingJobs []*db.Job
		reuseAssignments, remainingJobs = campaign.PlanReuse(allJobs, reusable)

		if len(reuseAssignments) > 0 {
			// Re-group remaining jobs for provisioning
			groups = campaign.GroupByGPUSupremum(remainingJobs)
			groups = campaign.FilterByGPUClass(groups, campaignLaunchGPU)
		}
	}

	// Estimate disk needs from HF model inputs
	r2Client, err := buildR2Client(cfg)
	if err != nil {
		log.Printf("warning: build R2 client for disk estimation: %v", err)
	}
	for i := range groups {
		groups[i].DiskGB = campaign.EstimateGroupDisk(groups[i], database, r2Client)
	}

	// Parse budget limits
	opts := parseLaunchOpts()

	// Dry run: print plan table
	if campaignLaunchDryRun {
		return runDryRunPlanWithReuse(database, cfg, groups, reuseAssignments, opts.Strategy)
	}

	if len(groups) == 0 && len(reuseAssignments) == 0 {
		fmt.Println("No jobs need rental GPUs.")
		return nil
	}

	// Check R2 config before entering interactive mode
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")
	}

	// Non-interactive mode (explicit via --yes/--plain or automatic).
	if !launchInteractive {
		return runNonInteractiveLaunch(database, cfg, groups, opts, reuseAssignments, useTUI)
	}

	finalModel, err := runLaunchProgram(database, cfg, groups, opts, campaignLaunchGPU, !needsSyncReconcile, false, !campaignLaunchNoWatch)
	if err != nil {
		return err
	}

	if finalModel.err != nil {
		return finalModel.err
	}

	// Segue into watch mode if instances were launched
	if len(finalModel.instanceIDs) > 0 && !campaignLaunchNoWatch && !finalModel.inlineWatchUsed {
		fmt.Println()
		if useTUI {
			return watchInstances(database, finalModel.instanceIDs)
		}
		return watchInstancesPlain(database, finalModel.instanceIDs)
	}

	return nil
}

func filterRentalLaunchJobs(jobs []*db.Job) []*db.Job {
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && !job.HasTag(db.TagInventory) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

// runNonInteractiveLaunch launches all groups without TUI interaction.
func runNonInteractiveLaunch(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, reuseAssignments []campaign.ReuseAssignment, watchTUI bool) error {
	// Submit reuse assignments first
	if len(reuseAssignments) > 0 {
		fmt.Print(campaign.FormatReuseAssignments(reuseAssignments))
		r2Client, err := newR2ClientFromConfig()
		if err != nil {
			return fmt.Errorf("R2 client for reuse: %w", err)
		}
		if err := executeReuseAssignments(database, r2Client, reuseAssignments); err != nil {
			return err
		}
	}

	if len(groups) == 0 {
		fmt.Println("All jobs assigned to existing instances.")
		return nil
	}

	clients, err := buildCloudClients(cfg)
	if err != nil {
		return err
	}

	// Fetch offers in parallel (with survival model for cost-optimal bidding)
	fmt.Println("Searching for GPU offers...")
	survivalModel := buildSurvivalModel(database)
	groupOffers := campaign.FetchGroupOffers(clients, groups, survivalModel, 1.0, 0.5, opts.Strategy)

	// Print plan summary with cost estimates
	totalJobs := 0
	for _, g := range groups {
		totalJobs += len(g.Jobs)
	}
	fmt.Printf("%d jobs in %d GPU groups\n", totalJobs, len(groups))

	predCfg := buildPredictorConfig(cfg)
	overheadModel := buildOverheadModel(database)
	estimates := campaign.EstimateCosts(groupOffers, &predCfg, overheadModel, nil, survivalModel, nil)
	fmt.Println(campaign.FormatCostTableWithEstimates(estimates))

	// Auto-derive budget limits from estimates if not set by CLI
	if opts.ApplyAutoBudget(estimates) {
		printAutoBudget(opts)
	}

	// Filter groups to those with valid offers, warning about failures
	var filteredGroups []campaign.InstanceGroup
	var offers []cloud.Offer
	var filteredEstimates []campaign.CostEstimate
	for i, go_ := range groupOffers {
		if go_.Err != nil {
			fmt.Fprintf(os.Stderr, "Warning: offer search failed for %s: %v\n", go_.Group.GPUSpec(), go_.Err)
			continue
		}
		if go_.Offer == nil {
			fmt.Fprintf(os.Stderr, "Warning: no offers found for %s — skipping %d job(s)\n", go_.Group.GPUSpec(), len(go_.Group.Jobs))
			continue
		}
		filteredGroups = append(filteredGroups, go_.Group)
		offers = append(offers, *go_.Offer)
		filteredEstimates = append(filteredEstimates, estimates[i])
	}
	if len(filteredGroups) == 0 {
		return fmt.Errorf("no offers found for any GPU group")
	}
	groups = filteredGroups
	estimates = filteredEstimates

	r2Cfg := cfg.Vastai.R2.ToCloudR2Config()

	result, err := campaign.LaunchCampaign(
		clients, database, groups, offers, estimates, survivalModel, opts, r2Cfg,
		func(provider cloud.Provider) (cloud.CreateOpts, error) {
			return createOptsForProvider(cfg, provider)
		},
		func(group campaign.InstanceGroup, phase string) {
			if strings.EqualFold(group.GPUClass, "campaign") {
				fmt.Printf("  %s\n", phase)
				return
			}
			fmt.Printf("  %s: %s\n", group.GPUSpec(), phase)
		},
		func(id int64) {
			fmt.Printf("Campaign %d: launching %d instance(s)...\n", id, len(groups))
		},
		nil,
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

	// Segue into watch mode.
	if !campaignLaunchNoWatch {
		fmt.Println()
		if watchTUI {
			return watchInstances(database, result.InstanceIDs)
		}
		return watchInstancesPlain(database, result.InstanceIDs)
	}

	return nil
}

func runDryRunPlanWithReuse(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, reuseAssignments []campaign.ReuseAssignment, strategy bidding.SelectionStrategy) error {
	if len(reuseAssignments) > 0 {
		fmt.Print(campaign.FormatReuseAssignments(reuseAssignments))
		fmt.Println()
	}

	if len(groups) == 0 {
		fmt.Println("All jobs can be assigned to existing instances. No new instances needed.")
		return nil
	}

	return runDryRunPlan(database, cfg, groups, strategy)
}

func runDryRunPlan(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, strategy bidding.SelectionStrategy) error {
	clients, err := buildCloudClients(cfg)
	if err != nil {
		return err
	}
	survivalModel := buildSurvivalModel(database)
	groupOffers := campaign.FetchGroupOffers(clients, groups, survivalModel, 1.0, 0.5, strategy)

	predCfg := buildPredictorConfig(cfg)
	overheadModel := buildOverheadModel(database)
	estimates := campaign.EstimateCosts(groupOffers, &predCfg, overheadModel, nil, survivalModel, nil)

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
			durStr = campaign.FormatEstDuration(est.TotalTime, len(est.JobDurations) > 0)
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

func resolveCampaignWatchID(database *sql.DB, args []string) (int64, error) {
	if len(args) > 0 {
		return parseCampaignID(args[0])
	}

	c, err := db.GetMostRecentCampaign(database)
	if err != nil {
		return 0, fmt.Errorf("get most recent campaign: %w", err)
	}
	if c == nil {
		return 0, fmt.Errorf("no campaigns found")
	}
	return c.ID, nil
}

func runCampaignWatch(cmd *cobra.Command, args []string) error {
	useTUI, err := resolveCampaignTUI(campaignWatchTUI, campaignWatchPlain)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	campaignID, err := resolveCampaignWatchID(database, args)
	if err != nil {
		return err
	}

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

	if useTUI {
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
	useTUI, err := resolveCampaignTUI(campaignListTUI, campaignListPlain)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if !useTUI {
		for _, warning := range reconcileBeforeDisplay(database, FastCloudSyncTimeout) {
			fmt.Fprintln(cmd.ErrOrStderr(), warning)
		}
	}

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}

	if useTUI {
		return runCampaignListTUI(database, campaigns)
	}

	if len(campaigns) == 0 {
		fmt.Println("No campaigns.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tINSTANCES\tEST. COST\tACTUAL COST\tCREATED\n")

	for _, c := range campaigns {
		created := time.Unix(c.CreatedAt, 0).Format("01/02 15:04")

		instances, _ := db.GetCampaignInstances(database, c.ID)

		estCost := campaign.FormatEstimatedCostCents(c.EstimatedCostCents)
		actualCost := campaignActualCost(instances)

		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\n",
			c.ID, c.Status, len(instances), estCost, actualCost, created)
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

	for _, warning := range reconcileBeforeDisplay(database, FastCloudSyncTimeout) {
		fmt.Fprintln(cmd.ErrOrStderr(), warning)
	}

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
		NoDonor:  campaignLaunchNoDonor,
		Strategy: bidding.SelectionStrategy(campaignLaunchStrategy),
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
	gracePeriod := campaignLaunchGracePeriod
	if gracePeriod == "" {
		if cfg, err := config.Load(); err == nil {
			gracePeriod = cfg.DefaultGracePeriod()
		} else {
			gracePeriod = "5m"
		}
	}
	if gracePeriod != "0" {
		if d, err := time.ParseDuration(gracePeriod); err == nil {
			opts.GracePeriodSeconds = int(d.Seconds())
		}
	}
	return opts
}

// executeReuseAssignments submits jobs to their assigned instances via R2.
// Groups assignments by instance to batch submissions.
func executeReuseAssignments(database *sql.DB, r2Client *r2.Client, assignments []campaign.ReuseAssignment) error {
	// Group by instance ID
	byInstance := make(map[int64][]*db.Job)
	for _, a := range assignments {
		byInstance[a.Instance.Instance.ID] = append(byInstance[a.Instance.Instance.ID], a.Job)
	}

	ctx := context.Background()
	for instanceID, jobs := range byInstance {
		jobIDs := make([]string, len(jobs))
		for i, j := range jobs {
			jobIDs[i] = fmt.Sprintf("#%d", j.ID)
		}
		fmt.Printf("Submitting %s to instance %d...\n", strings.Join(jobIDs, ", "), instanceID)

		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, jobs); err != nil {
			return fmt.Errorf("submit to instance %d: %w", instanceID, err)
		}

		// Auto-extend grace if deadline is close
		inst, _ := db.GetCloudInstance(database, instanceID)
		if inst != nil && inst.Status == db.CloudInstanceStatusGrace && inst.GraceDeadline != nil {
			remaining := time.Until(time.Unix(*inst.GraceDeadline, 0))
			if remaining < campaign.MinGraceRemaining {
				extendDur := 15 * time.Minute
				extendKey := r2keys.GraceExtend(instanceID)
				_ = r2Client.PutObject(ctx, extendKey, strings.NewReader(extendDur.String()), "text/plain")
				newDeadline := time.Now().Add(extendDur).Unix()
				_ = db.ExtendCloudInstanceGrace(database, instanceID, newDeadline)
				fmt.Printf("  Auto-extended grace period by %s\n", extendDur)
			}
		}

		fmt.Printf("  Submitted %d job(s) to instance %d\n", len(jobs), instanceID)
	}
	return nil
}

// reconcileBeforeDisplay checks running cloud instances against the provider
// and marks dead ones as failed, then auto-closes campaigns where all instances
// are terminal. Called before displaying campaign data.
func reconcileBeforeDisplay(database *sql.DB, timeout time.Duration) []string {
	cfg, _ := config.Load()
	result, completed := syncCloudStateWithTimeout(cfg, database, campaign.NewReconciler(), timeout, false)
	if !completed {
		return []string{fmt.Sprintf("Warning: cloud sync timed out after %s; campaign data may be stale.", timeout)}
	}
	if result.ReconcileResult != nil && result.ReconcileResult.Reconciled > 0 {
		fmt.Printf("Reconciled %d dead instance(s)\n", result.ReconcileResult.Reconciled)
	}
	return nil
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

// buildSurvivalModel queries historical cloud instance data and builds a
// Beta-Binomial survival model for cost-optimal bidding. Returns nil if no data or on error.
func buildSurvivalModel(database *sql.DB) *bidding.SurvivalModel {
	outcomes, err := bidding.LoadInstanceOutcomes(database)
	if err != nil {
		log.Printf("warning: could not query instance outcomes: %v", err)
		return nil
	}
	return bidding.BuildSurvivalModel(outcomes)
}

// campaignActualCost computes the total actual cost for a set of cloud instances
// based on uptime and cost_per_hour_cents. Returns a formatted string.
func campaignActualCost(instances []*db.CloudInstance) string {
	var totalCents float64
	for _, inst := range instances {
		if inst.CostPerHourCents == 0 || inst.LaunchedAt == nil {
			continue
		}
		var end time.Time
		if inst.EndedAt != nil {
			end = time.Unix(*inst.EndedAt, 0)
		} else {
			end = time.Now()
		}
		uptime := end.Sub(time.Unix(*inst.LaunchedAt, 0))
		totalCents += uptime.Hours() * float64(inst.CostPerHourCents)
	}
	return campaign.FormatCostCents(totalCents)
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
