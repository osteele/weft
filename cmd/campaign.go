package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ui/terminal"
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
	campaignLaunchMaxSpend          string
	campaignLaunchMaxTime           string
	campaignLaunchGracePeriod       string
	campaignLaunchDryRun            bool
	campaignLaunchWatch             bool
	campaignLaunchNoWatch           bool
	campaignLaunchNoDonor           bool
	campaignLaunchYes               bool
	campaignLaunchJobs              string
	campaignLaunchGPU               string
	campaignLaunchMaxGPUMem         int
	campaignLaunchStrategy          string
	campaignLaunchMinSurvival       float64
	campaignLaunchSkipWorkdirDelete bool
	campaignLaunchProject           string
	campaignLaunchTUI               bool
	campaignLaunchPlain             bool
	campaignLaunchAuto              bool
	campaignWatchTUI                bool
	campaignWatchPlain              bool
	campaignWatchAuto               bool
	campaignListTUI                 bool
	campaignListPlain               bool
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
	cmd.Flags().BoolVarP(&campaignLaunchWatch, "watch", "w", false, "Enter watch mode after launch (default in interactive terminals)")
	cmd.Flags().BoolVar(&campaignLaunchNoWatch, "no-watch", false, "Launch and exit immediately (print instance IDs only)")
	cmd.MarkFlagsMutuallyExclusive("watch", "no-watch")
	cmd.Flags().BoolVar(&campaignLaunchNoDonor, "no-donor", false, "Skip donor instance strategy (each instance downloads independently)")
	cmd.Flags().BoolVarP(&campaignLaunchYes, "yes", "y", false, "Non-interactive: launch all groups without TUI confirmation")
	cmd.Flags().StringVar(&campaignLaunchJobs, "jobs", "", "Comma-separated job IDs to include (default: all unplaced jobs)")
	cmd.Flags().StringVar(&campaignLaunchGPU, "gpu", "", "Filter by GPU class (e.g., 'RTX_4090', 'A100')")
	cmd.Flags().IntVar(&campaignLaunchMaxGPUMem, "max-gpu-mem", 0, "Maximum GPU memory in GB (overrides auto-derived ceiling from predictor; 0 = auto)")
	cmd.Flags().StringVar(&campaignLaunchGracePeriod, "grace-period", "", "Keep instance alive after job failure (default from config, e.g., '5m', '1h'; '0' to disable)")
	cmd.Flags().StringVar(&campaignLaunchStrategy, "strategy", "cheap", "Offer selection strategy: 'cheap' (minimize expected cost), 'fast' (minimize expected completion time), or 'fastest' (minimize happy-path runtime)")
	cmd.Flags().Float64Var(&campaignLaunchMinSurvival, "min-survival", 0.4, "Minimum survival probability (0-1); offers below this are skipped (0 to disable)")
	cmd.Flags().BoolVar(&campaignLaunchSkipWorkdirDelete, "skip-workdir-deletion", false, "Don't delete working directories after job completion (for debugging)")
	cmd.Flags().StringVar(&campaignLaunchProject, "project", "", "Filter unplaced jobs by project name")
	cmd.Flags().BoolVar(&campaignLaunchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&campaignLaunchPlain, "plain", false, "Force plain non-interactive mode")
	cmd.Flags().BoolVar(&campaignLaunchAuto, "auto", false, "Start watch with auto-pilot enabled (auto-relaunch, auto-place, auto-launch)")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
	cmd.MarkFlagsMutuallyExclusive("tui", "yes")
}

func addCampaignWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&campaignWatchTUI, "tui", false, "Force interactive TUI display")
	cmd.Flags().BoolVar(&campaignWatchPlain, "plain", false, "Force plain text output")
	cmd.Flags().BoolVar(&campaignWatchAuto, "auto", false, "Start with auto-pilot enabled (auto-relaunch, auto-place, auto-launch)")
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
	reportStartupPhase := newLaunchStartupReporter(cmd.ErrOrStderr())

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	agentdeploy.StartBackgroundPrewarm("linux", "amd64")

	if !launchInteractive {
		reportStartupPhase("Checking predictor status...")
		if err := ensurePredictorUsableFunc(cmd, cfg, "campaign planning"); err != nil {
			return err
		}
	}

	// For non-interactive and dry-run modes, reconcile synchronously.
	// For TUI mode, reconciliation runs in the background (see below).
	needsSyncReconcile := !launchInteractive || campaignLaunchDryRun
	if needsSyncReconcile {
		reportStartupPhase("Refreshing cloud state...")
		reconcileBeforeDisplay(database, FastCloudSyncTimeout)
	}

	reportStartupPhase("Loading unplaced jobs...")
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

	jobs = filterLaunchJobsByProject(jobs)

	// Pre-filter on-prem placement synchronously only for non-interactive launch
	// paths. The interactive TUI does this in the background so rental planning
	// can start immediately.
	if !launchInteractive {
		if len(jobs) > 0 {
			reportStartupPhase(fmt.Sprintf("Probing on-prem hosts for %d job(s)...", len(jobs)))
		}
		jobs = prefilterOnPremWithProgress(database, jobs, cfg, newLaunchProgressReporter(cmd.ErrOrStderr(), "Checking on-prem placement", len(jobs)), reportStartupPhase)
	}

	if len(jobs) == 0 {
		fmt.Println("No jobs need rental GPUs.")
		if n, err := db.CountJobsWaitingOnInstances(database); err == nil && n > 0 {
			fmt.Printf("(%d job(s) waiting on instances still setting up)\n", n)
		}
		return nil
	}

	r2Client, err := buildR2Client(cfg)
	if err != nil {
		slog.Warn("failed to build R2 client for disk estimation", "error", err)
	}

	reportStartupPhase("Preparing rental GPU groups...")
	groups := campaign.PrepareGroups(jobs, database, campaignLaunchGPU, r2Client)

	if len(groups) == 0 {
		if campaignLaunchGPU != "" {
			fmt.Printf("No unplaced jobs match GPU class %q.\n", campaignLaunchGPU)
		} else {
			fmt.Println("No jobs need rental GPUs.")
			if n, err := db.CountJobsWaitingOnInstances(database); err == nil && n > 0 {
				fmt.Printf("(%d job(s) waiting on instances still setting up)\n", n)
			}
		}
		return nil
	}

	// Apply --max-gpu-mem override to all groups
	if campaignLaunchMaxGPUMem > 0 {
		for i := range groups {
			groups[i].MaxGPUMemGB = campaignLaunchMaxGPUMem
		}
	}

	// Parse budget limits
	opts := parseLaunchOpts()

	// Dry run: print plan table
	if campaignLaunchDryRun {
		return runDryRunPlan(database, cfg, groups, opts.Strategy, opts.MinSurvival)
	}

	// Check R2 config before entering interactive mode
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")
	}

	// Non-interactive mode (explicit via --yes/--plain or automatic).
	if !launchInteractive {
		return runNonInteractiveLaunch(cmd, database, cfg, groups, opts, useTUI)
	}

	finalModel, err := runLaunchProgram(database, cfg, groups, opts, campaignLaunchGPU, !needsSyncReconcile, false, shouldWatch())
	if err != nil {
		return err
	}

	if finalModel.Err != nil {
		return finalModel.Err
	}

	// Segue into watch mode if instances were launched
	if len(finalModel.InstanceIDs) > 0 && shouldWatch() && !finalModel.InlineWatchUsed {
		fmt.Println()
		mode, watchIDs, projectFilter, err := resolveLaunchWatchTarget(database, cmd, finalModel.InstanceIDs)
		if err != nil {
			return err
		}
		return watchAndReport(database, useTUI, mode, watchIDs, campaign.SummarizeEstimates(finalModel.CostEstimates), campaignLaunchAuto, projectFilter)
	}

	// Inline watch already ran inside the TUI — print the exit report
	if len(finalModel.InstanceIDs) > 0 && finalModel.InlineWatchUsed {
		printWatchExitReport(database, finalModel.InstanceIDs)
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

func filterLaunchJobsForScope(jobs []*db.Job, projectFilter string) []*db.Job {
	if projectFilter != "" {
		return db.FilterJobsByProject(jobs, projectFilter)
	}
	return filterLaunchJobsByProject(jobs)
}

func refreshLaunchGroupsWithOnPrem(database *sql.DB, cfg *config.Config, gpuFilter string, projectFilter string, onProgress func(int, int), onPhase func(string)) ([]campaign.InstanceGroup, error) {
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}
	jobs = filterRentalLaunchJobs(jobs)
	jobs = filterLaunchJobsForScope(jobs, projectFilter)
	jobs = prefilterOnPremWithProgress(database, jobs, cfg, onProgress, onPhase)

	r2Client, err := buildR2Client(cfg)
	if err != nil {
		slog.Warn("failed to build R2 client for disk estimation", "error", err)
	}
	return campaign.PrepareGroups(jobs, database, gpuFilter, r2Client), nil
}

// prefilterOnPrem tries to place unplaced jobs on on-prem hosts before launching
// rental instances. Returns the subset of jobs that still need rental.
func prefilterOnPrem(database *sql.DB, jobs []*db.Job, cfg *config.Config) []*db.Job {
	return prefilterOnPremWithProgress(database, jobs, cfg, nil, nil)
}

func prefilterOnPremWithProgress(database *sql.DB, jobs []*db.Job, cfg *config.Config, onProgress func(int, int), onPhase func(string)) []*db.Job {
	return placement.PrefilterOnPrem(database, jobs, cfg, placement.PrefilterCallbacks{
		OnProgress: onProgress,
		OnPhase:    onPhase,
		OnPlaced: func(jobID int64, host string) {
			fmt.Printf("Job #%d → %s (on-prem, skipping rental)\n", jobID, host)
		},
	})
}

// runNonInteractiveLaunch launches all groups without TUI interaction.
func runNonInteractiveLaunch(cmd *cobra.Command, database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, watchTUI bool) error {
	clients, providerErr := buildCloudClients(cfg)
	if providerErr != nil {
		clients = nil
	}
	survivalModel := buildSurvivalModel(database)
	overheadModel := buildOverheadModel(database)
	predCfg := buildPredictorConfig(cfg)
	fmt.Println("Searching for GPU offers...")
	reportPlanProgress := newPlanProgressPrinter(os.Stderr)
	prep, err := terminal.PrepareLaunchExecutionPlanWithProgress(
		database,
		clients,
		providerErr,
		groups,
		nil,
		opts.ScoringProfile(),
		opts.MinSurvival,
		&predCfg,
		overheadModel,
		survivalModel,
		reportPlanProgress,
	)
	if err != nil {
		return err
	}

	if len(prep.StrategyPlan.ReuseAssignments) > 0 {
		fmt.Print(campaign.FormatReuseAssignments(prep.StrategyPlan.ReuseAssignments))
		r2Client, err := newR2ClientFromConfig()
		if err != nil {
			return fmt.Errorf("R2 client for reuse: %w", err)
		}
		if err := executeReuseAssignments(database, r2Client, prep.StrategyPlan.ReuseAssignments); err != nil {
			return err
		}
		fmt.Println()
	}

	if prep.StrategyPlan.NewCandidate != nil {
		for _, go_ := range prep.StrategyPlan.NewCandidate.Offers {
			if go_.Err != nil {
				fmt.Fprintf(os.Stderr, "Warning: offer search failed for %s: %v\n", go_.Group.GPUSpec(), go_.Err)
				continue
			}
			if go_.Offer == nil {
				fmt.Fprintf(os.Stderr, "Warning: no offers found for %s — skipping %d job(s)\n", go_.Group.GPUSpec(), len(go_.Group.Jobs))
			}
		}
	}

	groupsToLaunch := prep.LaunchGroups
	offers := prep.Offers
	estimates := prep.Estimates
	totalJobs := 0
	for _, g := range groupsToLaunch {
		totalJobs += len(g.Jobs)
	}
	coveredJobs := len(prep.StrategyPlan.ReuseAssignments) + totalJobs
	if len(groupsToLaunch) == 0 {
		if len(prep.StrategyPlan.ReuseAssignments) == prep.RequestedJobs {
			fmt.Println("All selected jobs assigned to existing instances.")
			return nil
		}
		if len(prep.StrategyPlan.ReuseAssignments) > 0 {
			return fmt.Errorf("%d selected job(s) assigned to existing instances, but %d job(s) still need new instances", len(prep.StrategyPlan.ReuseAssignments), prep.RequestedJobs-len(prep.StrategyPlan.ReuseAssignments))
		}
		if providerErr != nil {
			return providerErr
		}
		return fmt.Errorf("no offers found for any GPU group")
	}
	if coveredJobs < prep.RequestedJobs {
		fmt.Fprintf(os.Stderr, "Warning: %d selected job(s) will be skipped (no reusable instance or cloud offer)\n", prep.RequestedJobs-coveredJobs)
	}

	fmt.Printf("%d jobs in %d GPU groups\n", totalJobs, len(groupsToLaunch))
	if len(estimates) > 0 {
		fmt.Println(campaign.FormatCostTableWithEstimates(estimates))
	}

	if opts.ApplyAutoBudget(estimates) {
		printAutoBudget(opts)
	}

	r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
	useLaunchProgressTUI := watchTUI && terminal.UseLaunchProgressTUI(false)
	var launchTUI *terminal.LaunchProgressTUI
	if useLaunchProgressTUI {
		launchTUI = terminal.StartLaunchProgressTUI(0, len(groupsToLaunch))
		defer func() { _ = launchTUI.Stop() }()
	}

	result, err := campaign.LaunchCampaign(
		clients, database, groupsToLaunch, offers, estimates, survivalModel, opts, r2Cfg,
		func(provider cloud.Provider) (cloud.CreateOpts, error) {
			return createOptsForProvider(cfg, provider)
		},
		func(event campaign.LaunchEvent) {
			if launchTUI != nil {
				launchTUI.SendEvent(event)
				return
			}
			switch event.Kind {
			case campaign.LaunchEventCampaignStatus:
				if strings.TrimSpace(event.Phase) != "" {
					fmt.Printf("  %s\n", event.Phase)
				}
			case campaign.LaunchEventGroupAssets:
				fmt.Printf("  %s: staging (%d/%d assets ready)\n", event.Group.GPUSpec(), event.AssetsReady, event.AssetsTotal)
			case campaign.LaunchEventGroupRetry:
				fmt.Printf("  %s: retrying with replacement offer (attempt %d/%d)\n", event.Group.GPUSpec(), event.RetryAttempt, event.RetryMax)
			case campaign.LaunchEventGroupPhase:
				if strings.TrimSpace(event.Phase) != "" {
					fmt.Printf("  %s: %s\n", event.Group.GPUSpec(), event.Phase)
				}
			}
		},
		func(id int64) {
			if launchTUI != nil {
				launchTUI.SetCampaign(id, len(groupsToLaunch))
				return
			}
			fmt.Printf("Campaign %d: launching %d instance(s)...\n", id, len(groupsToLaunch))
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
	if launchTUI != nil {
		_ = launchTUI.Stop()
	}
	fmt.Printf("Campaign %d: launched %d instance(s)\n", result.CampaignID, len(result.InstanceIDs))
	for _, id := range result.InstanceIDs {
		fmt.Printf("Launched instance %s\n", ids.FormatInstanceID(id))
	}

	// Print next steps
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  weft campaign watch %d\n", result.CampaignID)

	// Segue into watch mode.
	if shouldWatch() {
		fmt.Println()
		mode, watchIDs, projectFilter, err := resolveLaunchWatchTarget(database, cmd, result.InstanceIDs)
		if err != nil {
			return err
		}
		return watchAndReport(database, watchTUI, mode, watchIDs, campaign.SummarizeEstimates(estimates), campaignLaunchAuto, projectFilter)
	}

	return nil
}

func runDryRunPlan(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, strategy bidding.SelectionStrategy, minSurvival float64) error {
	clients, providerErr := buildCloudClients(cfg)
	if providerErr != nil {
		clients = nil
	}
	survivalModel := buildSurvivalModel(database)
	overheadModel := buildOverheadModel(database)
	predCfg := buildPredictorConfig(cfg)
	reusable, _ := campaign.FindReusableInstances(database)
	if providerErr != nil && len(reusable) == 0 {
		return providerErr
	}
	reportPlanProgress := newPlanProgressPrinter(os.Stderr)
	plans, _ := campaign.BuildProfilePlansWithProgress(
		database,
		clients,
		groups,
		reusable,
		&predCfg,
		overheadModel,
		survivalModel,
		[]bidding.ScoreProfile{strategy.Profile()},
		minSurvival,
		reportPlanProgress,
	)
	plan, ok := plans[strategy.Profile().ID]
	if !ok {
		return fmt.Errorf("could not build dry-run plan for strategy %s", strategy)
	}
	requestedJobs := 0
	for _, group := range groups {
		requestedJobs += len(group.Jobs)
	}
	if len(plan.ReuseAssignments) > 0 {
		fmt.Print(campaign.FormatReuseAssignments(plan.ReuseAssignments))
		fmt.Println()
	}

	var groupOffers []campaign.GroupOffer
	var estimates []campaign.CostEstimate
	if plan.NewCandidate != nil {
		groupOffers = plan.NewCandidate.Offers
		estimates = plan.NewCandidate.Estimates
	}

	printSurvivalRejections(groupOffers, minSurvival)
	if len(groupOffers) == 0 {
		if len(plan.ReuseAssignments) == requestedJobs {
			fmt.Println("All jobs can be assigned to existing instances. No new instances needed.")
			return nil
		}
		if len(plan.ReuseAssignments) > 0 {
			fmt.Printf("%d job(s) can be assigned to existing instances, but %d job(s) still need new instances.\n", len(plan.ReuseAssignments), requestedJobs-len(plan.ReuseAssignments))
			return nil
		}
		if providerErr != nil {
			return providerErr
		}
		fmt.Println("No new-instance offers found.")
		return nil
	}

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
	fmt.Println("To launch interactively: weft launch instances")
	return nil
}

func newLaunchStartupReporter(w io.Writer) func(string) {
	return func(message string) {
		if w == nil || message == "" {
			return
		}
		fmt.Fprintln(w, message)
	}
}

func newLaunchProgressReporter(w io.Writer, label string, total int) func(int, int) {
	if w == nil || total <= 0 {
		return nil
	}
	lastPrinted := 0
	lastAt := time.Time{}
	return func(current, total int) {
		if current <= 0 || current == lastPrinted {
			return
		}
		now := time.Now()
		if current != total && current != 1 && now.Sub(lastAt) < 1500*time.Millisecond {
			return
		}
		lastPrinted = current
		lastAt = now
		fmt.Fprintf(w, "%s (%d/%d)\n", label, current, total)
	}
}

func newPlanProgressPrinter(w io.Writer) campaign.PlanProgressFunc {
	if w == nil {
		return nil
	}
	last := ""
	return func(progress campaign.PlanProgress) {
		message := formatLaunchPlanProgress(progress)
		if message == "" || message == last {
			return
		}
		last = message
		fmt.Fprintln(w, message)
	}
}

func formatLaunchPlanProgress(progress campaign.PlanProgress) string {
	if progress.Phase == "" {
		return ""
	}
	message := "Building launch plan: " + progress.Phase
	if progress.Current > 0 && progress.Total > 0 {
		message += fmt.Sprintf(" (%d/%d)", progress.Current, progress.Total)
	}
	if progress.Detail != "" {
		message += " — " + progress.Detail
	}
	return message
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

	database, err := db.OpenForReading()
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

	return watchAndReport(database, useTUI, terminal.ModeCampaign, instanceIDs, nil, campaignWatchAuto, "")
}

type launchWatchScope string

const (
	launchWatchScopeAllInstances launchWatchScope = "all_instances"
	launchWatchScopeCampaign     launchWatchScope = "campaign"
	launchWatchScopeProject      launchWatchScope = "project"
)

func resolveLaunchWatchTarget(database *sql.DB, cmd *cobra.Command, launchedIDs []int64) (terminal.Mode, []int64, string, error) {
	scope, projectFilter := inferLaunchWatchScope(cmd)
	switch scope {
	case launchWatchScopeProject:
		ids, err := resolveProjectWatchInstanceIDs(database, projectFilter, launchedIDs)
		return terminal.ModeInstances, ids, projectFilter, err
	case launchWatchScopeCampaign:
		ids, err := resolveCampaignWatchInstanceIDs(database, launchedIDs)
		return terminal.ModeInstances, ids, "", err
	default:
		ids, err := resolveAllActiveWatchInstanceIDs(database, launchedIDs)
		return terminal.ModeInstances, ids, "", err
	}
}

func inferLaunchWatchScope(cmd *cobra.Command) (launchWatchScope, string) {
	if project := strings.TrimSpace(campaignLaunchProject); project != "" {
		return launchWatchScopeProject, project
	}

	if cmd == nil {
		return launchWatchScopeAllInstances, ""
	}

	name := cmd.Name()
	if name == "campaign" {
		return launchWatchScopeCampaign, ""
	}
	if name == "project" {
		return launchWatchScopeProject, ""
	}
	if name == "instance" || name == "instances" {
		return launchWatchScopeAllInstances, ""
	}

	if parent := cmd.Parent(); parent != nil {
		switch parent.Name() {
		case "campaign":
			return launchWatchScopeCampaign, ""
		case "project":
			return launchWatchScopeProject, ""
		case "instance", "instances":
			return launchWatchScopeAllInstances, ""
		}
	}

	path := " " + strings.ToLower(cmd.CommandPath()) + " "
	if strings.Contains(path, " campaign ") {
		return launchWatchScopeCampaign, ""
	}
	if strings.Contains(path, " project ") {
		return launchWatchScopeProject, ""
	}
	return launchWatchScopeAllInstances, ""
}

func resolveAllActiveWatchInstanceIDs(database *sql.DB, fallbackIDs []int64) ([]int64, error) {
	instances, err := db.ListRunningLaunches(database)
	if err != nil {
		return nil, fmt.Errorf("list running instances: %w", err)
	}
	ids := make([]int64, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}
	if len(ids) == 0 {
		ids = append(ids, fallbackIDs...)
	}
	return uniqueSortedInstanceIDs(ids), nil
}

func resolveCampaignWatchInstanceIDs(database *sql.DB, launchedIDs []int64) ([]int64, error) {
	campaignIDs := make(map[int64]struct{})
	for _, instanceID := range launchedIDs {
		inst, err := db.GetLaunch(database, instanceID)
		if err != nil {
			return nil, fmt.Errorf("get launch %s: %w", ids.FormatInstanceID(instanceID), err)
		}
		if inst == nil || inst.CampaignID == nil {
			continue
		}
		campaignIDs[*inst.CampaignID] = struct{}{}
	}

	if len(campaignIDs) != 1 {
		return expandInstanceReplacementChain(database, launchedIDs)
	}

	var campaignID int64
	for id := range campaignIDs {
		campaignID = id
	}
	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list campaign instances: %w", err)
	}
	ids := make([]int64, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}
	return expandInstanceReplacementChain(database, ids)
}

func resolveProjectWatchInstanceIDs(database *sql.DB, project string, launchedIDs []int64) ([]int64, error) {
	instances, err := db.ListRunningLaunches(database)
	if err != nil {
		return nil, fmt.Errorf("list running instances: %w", err)
	}

	var ids []int64
	for _, inst := range instances {
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, inst.ID)
		if err != nil {
			continue
		}
		for _, job := range jobs {
			if job != nil && strings.TrimSpace(job.Project) == project {
				ids = append(ids, inst.ID)
				break
			}
		}
	}

	if len(ids) == 0 {
		ids = append(ids, launchedIDs...)
	}
	return expandInstanceReplacementChain(database, ids)
}

func expandInstanceReplacementChain(database *sql.DB, seedIDs []int64) ([]int64, error) {
	if len(seedIDs) == 0 {
		return nil, nil
	}
	launches, err := db.ListLaunches(database)
	if err != nil {
		return nil, fmt.Errorf("list launches: %w", err)
	}

	byReplaced := make(map[int64][]int64)
	for _, launch := range launches {
		if launch == nil || launch.ReplacedInstanceID == nil {
			continue
		}
		replacedID := *launch.ReplacedInstanceID
		byReplaced[replacedID] = append(byReplaced[replacedID], launch.ID)
	}

	seen := make(map[int64]struct{})
	queue := make([]int64, 0, len(seedIDs))
	for _, id := range seedIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		queue = append(queue, id)
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range byReplaced[current] {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			queue = append(queue, child)
		}
	}

	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func uniqueSortedInstanceIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
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

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if !useTUI {
		reconcileBeforeDisplay(database, FastCloudSyncTimeout)
	}

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}

	if useTUI {
		return terminal.RunCampaignListTUI(database, campaigns)
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
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database, FastCloudSyncTimeout)

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

// shouldWatch returns true if the launch should segue into watch mode.
// --watch forces it on, --no-watch forces it off, default is on.
func shouldWatch() bool {
	if campaignLaunchWatch {
		return true
	}
	return !campaignLaunchNoWatch
}

func parseLaunchOpts() campaign.LaunchOpts {
	strategy := bidding.SelectionStrategy(campaignLaunchStrategy)
	switch strategy {
	case bidding.StrategyCheap, bidding.StrategyFast, bidding.StrategyFastest:
	default:
		fmt.Fprintf(os.Stderr, "warning: unknown strategy %q, using %q\n", campaignLaunchStrategy, bidding.StrategyCheap)
		strategy = bidding.StrategyCheap
	}
	opts := campaign.LaunchOpts{
		NoDonor:             campaignLaunchNoDonor,
		Strategy:            strategy,
		MinSurvival:         campaignLaunchMinSurvival,
		SkipWorkdirDeletion: campaignLaunchSkipWorkdirDelete,
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
	cfg, cfgErr := config.Load()
	gracePeriod := campaignLaunchGracePeriod
	if gracePeriod == "" {
		if cfgErr == nil {
			gracePeriod = cfg.DefaultGracePeriod()
		} else {
			gracePeriod = "5m"
		}
	}
	if cfgErr == nil {
		opts.GPUWarmup = cfg.Campaign.GPUWarmup
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
		fmt.Printf("Submitting %s to instance %s...\n", strings.Join(jobIDs, ", "), ids.FormatInstanceID(instanceID))

		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, jobs); err != nil {
			return fmt.Errorf("submit to instance %s: %w", ids.FormatInstanceID(instanceID), err)
		}

		// Auto-extend grace if deadline is close
		inst, _ := db.GetLaunch(database, instanceID)
		if inst != nil && inst.Status == db.LaunchStatusGrace && inst.GraceDeadline != nil {
			remaining := time.Until(time.Unix(*inst.GraceDeadline, 0))
			if remaining < campaign.MinGraceRemaining {
				extendDur := 15 * time.Minute
				_, _ = controlplane.SendGraceExtend(ctx, r2Client, instanceID, extendDur)
				newDeadline := time.Now().Add(extendDur).Unix()
				_ = db.ExtendLaunchGrace(database, instanceID, newDeadline)
				fmt.Printf("  Auto-extended grace period by %s\n", extendDur)
			}
		}

		fmt.Printf("  Submitted %d job(s) to instance %s\n", len(jobs), ids.FormatInstanceID(instanceID))
	}
	return nil
}

// reconcileBeforeDisplay checks running cloud instances against the provider
// and marks dead ones as failed, then auto-closes campaigns where all instances
// are terminal. Called before displaying campaign data.
func reconcileBeforeDisplay(database *sql.DB, timeout time.Duration) {
	cfg, _ := config.Load()
	result, completed := syncCloudStateWithTimeout(cfg, database, campaign.NewReconciler(), timeout, false)
	if !completed {
		fmt.Fprintf(os.Stderr, "Warning: %s.\n", degraded.CloudSyncTimedOutShowingLastKnownState(timeout.String()))
	}
	if result.ReconcileResult != nil && result.ReconcileResult.Reconciled > 0 {
		fmt.Printf("Reconciled %d dead instance(s)\n", result.ReconcileResult.Reconciled)
	}
}

// buildOverheadModel queries historical cloud instance data and builds a
// Bayesian overhead model. Returns nil if no data or on error.
func buildOverheadModel(database *sql.DB) *estimate.OverheadModel {
	obs, err := db.QueryOverheadObservations(database)
	if err != nil {
		slog.Warn("could not query overhead observations", "error", err)
		return nil
	}
	return estimate.BuildOverheadModel(obs)
}

func printSurvivalRejections(groupOffers []campaign.GroupOffer, minSurvival float64) {
	for _, go_ := range groupOffers {
		for _, rg := range go_.RejectedGroups {
			fmt.Fprintf(os.Stderr, "Skipped %d %s offers (%.0f%% survival, below %.0f%% floor)\n",
				rg.Count, rg.GPUFamily, rg.SurvivalProb*100, minSurvival*100)
		}
	}
}

// buildSurvivalModel queries historical cloud instance data and builds a
// Beta-Binomial survival model for cost-optimal bidding. Returns nil if no data or on error.
func buildSurvivalModel(database *sql.DB) *bidding.SurvivalModel {
	outcomes, err := bidding.LoadInstanceOutcomes(database)
	if err != nil {
		slog.Warn("could not query instance outcomes", "error", err)
		return nil
	}
	return bidding.BuildSurvivalModel(outcomes)
}

// campaignActualCost computes the total actual cost for a set of cloud instances
// based on uptime and cost_per_hour_cents. Returns a formatted string.
func campaignActualCost(instances []*db.Launch) string {
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
