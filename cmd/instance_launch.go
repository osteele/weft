package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
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
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

const instanceLaunchLong = `Shows jobs whose GPU constraints can't be satisfied by on-prem hosts, grouped by GPU class with checkboxes.
Select/deselect jobs, view cost estimates, and launch instances.

Use --dry-run to just print the plan without launching.`

var (
	instanceLaunchMaxSpend          string
	instanceLaunchMaxTime           string
	instanceLaunchGracePeriod       string
	instanceLaunchDryRun            bool
	instanceLaunchWatch             bool
	instanceLaunchNoWatch           bool
	instanceLaunchNoDonor           bool
	instanceLaunchYes               bool
	instanceLaunchJobs              string
	instanceLaunchGPU               string
	instanceLaunchStrategy          string
	instanceLaunchMinSurvival       float64
	instanceLaunchSkipWorkdirDelete bool
	instanceLaunchProject           string
	instanceLaunchRunpodCloudType   string
	instanceLaunchTUI               bool
	instanceLaunchPlain             bool
	instanceLaunchDistinctMachines  bool
	instanceLaunchAvoid             []string
	instanceLaunchAffinity          []string
	instanceLaunchNoWait            bool
)

func addInstanceLaunchFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&instanceLaunchMaxSpend, "max-spend", "", "Maximum spend per instance (e.g., '$5.00')")
	cmd.Flags().StringVar(&instanceLaunchMaxTime, "max-time", "", "Maximum time per instance (e.g., '2h')")
	cmd.Flags().BoolVar(&instanceLaunchDryRun, "dry-run", false, "Print plan table and exit without launching")
	cmd.Flags().BoolVar(&instanceLaunchNoWait, "no-wait", false, "Return immediately if another placement pass owns the slot")
	cmd.Flags().BoolVarP(&instanceLaunchWatch, "watch", "w", false, "Enter watch mode after launch (default in interactive terminals)")
	cmd.Flags().BoolVar(&instanceLaunchNoWatch, "no-watch", false, "Launch and exit immediately (print instance IDs only)")
	cmd.MarkFlagsMutuallyExclusive("watch", "no-watch")
	cmd.Flags().BoolVar(&instanceLaunchNoDonor, "no-donor", false, "Skip donor instance strategy (each instance downloads independently)")
	cmd.Flags().BoolVarP(&instanceLaunchYes, "yes", "y", false, "Non-interactive: launch all groups without TUI confirmation")
	cmd.Flags().StringVar(&instanceLaunchJobs, "jobs", "", "Job IDs/ranges to include, comma-separated or repeated syntax (default: all unplaced jobs)")
	cmd.Flags().StringVar(&instanceLaunchGPU, "gpu", "", "Filter by GPU class (e.g., 'RTX_4090', 'A100')")
	cmd.Flags().StringVar(&instanceLaunchGracePeriod, "grace-period", "", "Keep instance alive after job failure (default from config, e.g., '5m', '1h'; '0' to disable)")
	cmd.Flags().StringVar(&instanceLaunchRunpodCloudType, "runpod-cloud-type", "", "RunPod cloud type for this launch: community or secure (default from config)")
	cmd.Flags().StringVar(&instanceLaunchStrategy, "strategy", "cheap", "Offer selection strategy: 'cheap' (minimize expected cost), 'fast' (minimize expected completion time), or 'fastest' (minimize happy-path runtime)")
	cmd.Flags().Float64Var(&instanceLaunchMinSurvival, "min-survival", 0.4, "Minimum Weft learned end-to-end survival probability (0-1; 0 disables; distinct from provider reliability)")
	cmd.Flags().BoolVar(&instanceLaunchSkipWorkdirDelete, "skip-workdir-deletion", false, "Don't delete working directories after job completion (for debugging)")
	cmd.Flags().StringVar(&instanceLaunchProject, "project", "", "Filter unplaced jobs by project name")
	cmd.Flags().BoolVar(&instanceLaunchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&instanceLaunchPlain, "plain", false, "Force plain non-interactive mode")
	cmd.Flags().BoolVar(&instanceLaunchDistinctMachines, "distinct-machines", false, "Launch on distinct provider physical machines")
	cmd.Flags().StringArrayVar(&instanceLaunchAvoid, "avoid", nil, "Machine, instance, or job to avoid for --distinct-machines (repeatable, comma-separated)")
	cmd.Flags().StringArrayVar(&instanceLaunchAffinity, "affinity", nil, "Machine, instance, or job whose Vast.ai physical machine must be used (repeatable, comma-separated)")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
	cmd.MarkFlagsMutuallyExclusive("tui", "yes")
}

var runInstanceLaunchFunc = runInstanceLaunch

func runInstanceLaunch(cmd *cobra.Command, args []string) error {
	useTUI, err := resolveTUI(instanceLaunchTUI, instanceLaunchPlain)
	if err != nil {
		return err
	}
	launchInteractive := useTUI && !instanceLaunchYes
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
	if err := applyRunpodCloudTypeOverride(cfg, instanceLaunchRunpodCloudType); err != nil {
		return err
	}
	agentdeploy.StartBackgroundPrewarm("linux", "amd64")

	if !launchInteractive {
		reportStartupPhase("Checking predictor status...")
		if err := ensurePredictorUsableFunc(cmd, cfg, "instance planning"); err != nil {
			// The predictor is an optimization, never a gate: a broken,
			// disabled, or timed-out estimator degrades to heuristic
			// estimates rather than blocking placement (invariant
			// PredictorNeverBlocksPlacement). Surface the diagnosis and
			// proceed. Set predictor.enabled = false to silence it.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v; using heuristic estimates\n", err)
		}
	}

	// For non-interactive and dry-run modes, reconcile synchronously.
	// For TUI mode, reconciliation runs in the background (see below).
	needsSyncReconcile := !launchInteractive || instanceLaunchDryRun
	if needsSyncReconcile {
		reportStartupPhase("Refreshing cloud state...")
		reconcileBeforeDisplay(database, FastCloudSyncTimeout)
	}

	// Filter by --jobs if specified. The filter is also retained on the
	// launch model so the TUI's background reload paths (reconciliation,
	// on-prem refresh) keep honoring it instead of widening to every
	// unplaced job.
	jobIDFilter, err := parseLaunchJobIDFilter(instanceLaunchJobs)
	if err != nil {
		return err
	}

	reportStartupPhase("Loading unplaced jobs...")
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return fmt.Errorf("list unplaced jobs: %w", err)
	}
	jobs = filterRentalLaunchJobs(jobs)
	if instanceLaunchDistinctMachines && len(jobIDFilter) > 0 {
		var assignedJobs []*db.Job
		assignedJobs, err = explicitDistinctAssignedJobs(database, jobIDFilter)
		if err != nil {
			return err
		}
		if len(assignedJobs) > 0 && launchInteractive && !instanceLaunchDryRun {
			return fmt.Errorf("--distinct-machines selected %d queued job(s) already assigned to rental instances; rerun with --yes or --plain to re-plan them", len(assignedJobs))
		}
		jobs = mergeJobsByID(jobs, assignedJobs)
	}
	jobs = filterLaunchJobsByDependencies(database, jobs, printDeferredJob)
	jobs = db.FilterJobsByIDSet(jobs, jobIDFilter)

	jobs = filterLaunchJobsByProject(jobs)
	if instanceLaunchDistinctMachines && len(jobIDFilter) > 0 && !launchInteractive && !instanceLaunchDryRun {
		var relocated int
		jobs, relocated, err = unplaceDistinctMachineRentalAssignments(database, jobs)
		if err != nil {
			return err
		}
		if relocated > 0 {
			fmt.Fprintf(os.Stderr, "Re-planning %d queued rental-assigned job(s) for --distinct-machines.\n", relocated)
		}
	}

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
		printNoRentalLaunchNeeded(database)
		return nil
	}

	r2Client, err := buildR2Client(cfg)
	if err != nil {
		slog.Warn("failed to build R2 client for disk estimation", "error", err)
	}

	reportStartupPhase("Preparing rental GPU groups...")
	groups := campaign.PrepareGroupsWithConfig(jobs, database, cfg, instanceLaunchGPU, r2Client)

	if len(groups) == 0 {
		if instanceLaunchGPU != "" {
			fmt.Printf("No unplaced jobs match GPU class %q.\n", instanceLaunchGPU)
		} else {
			printNoRentalLaunchNeeded(database)
		}
		return nil
	}

	// Parse budget limits
	opts := parseLaunchOpts()
	if err := applyDistinctMachineLaunchOptions(database, &opts); err != nil {
		return err
	}

	// Dry run: print plan table
	if instanceLaunchDryRun {
		return runDryRunPlan(database, cfg, groups, opts)
	}

	// Check R2 config before entering interactive mode
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")
	}

	// Non-interactive mode (explicit via --yes/--plain or automatic).
	if !launchInteractive {
		return runNonInteractiveLaunch(cmd, database, cfg, groups, opts, useTUI)
	}

	finalModel, err := runLaunchProgram(database, cfg, groups, opts, instanceLaunchGPU, jobIDFilter, !needsSyncReconcile, false, shouldWatch())
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
		return watchAndReport(database, useTUI, mode, watchIDs, campaign.SummarizeEstimates(finalModel.CostEstimates), projectFilter)
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

func explicitDistinctAssignedJobs(database *sql.DB, jobIDFilter map[int64]bool) ([]*db.Job, error) {
	ids := sortedJobFilterIDs(jobIDFilter)
	selectedByID, err := db.GetJobsByIDs(database, ids)
	if err != nil {
		return nil, fmt.Errorf("load explicit jobs: %w", err)
	}
	selected := make([]*db.Job, 0, len(selectedByID))
	for _, id := range ids {
		job := selectedByID[id]
		if isDistinctRelocationCandidate(job) {
			selected = append(selected, job)
		}
	}
	return selected, nil
}

func isDistinctRelocationCandidate(job *db.Job) bool {
	return job != nil &&
		job.EffectiveStatus() == db.StatusQueued &&
		job.IsRentalJob() &&
		!job.HasTag(db.TagInventory)
}

func mergeJobsByID(base, extra []*db.Job) []*db.Job {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[int64]struct{}, len(base)+len(extra))
	merged := make([]*db.Job, 0, len(base)+len(extra))
	for _, job := range base {
		if job == nil {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		merged = append(merged, job)
	}
	for _, job := range extra {
		if job == nil {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		merged = append(merged, job)
	}
	return merged
}

func sortedJobFilterIDs(jobIDFilter map[int64]bool) []int64 {
	if len(jobIDFilter) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(jobIDFilter))
	for id, selected := range jobIDFilter {
		if selected {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func unplaceDistinctMachineRentalAssignments(database *sql.DB, jobs []*db.Job) ([]*db.Job, int, error) {
	refreshed := make([]*db.Job, 0, len(jobs))
	relocated := 0
	for _, job := range jobs {
		if !isDistinctRelocationCandidate(job) {
			refreshed = append(refreshed, job)
			continue
		}
		if _, err := ops.UnplaceQueuedJob(database, job, ops.ExecuteOptions{}); err != nil {
			return nil, relocated, err
		}
		reloaded, err := db.GetJobByID(database, job.ID)
		if err != nil {
			return nil, relocated, fmt.Errorf("reload unplaced job %s: %w", ids.FormatJobID(job.ID), err)
		}
		refreshed = append(refreshed, reloaded)
		relocated++
	}
	return refreshed, relocated, nil
}

// filterLaunchJobsByDependencies excludes jobs whose --after / --after-any
// dependencies have not yet terminated successfully (or, for --after-any,
// terminated at all). This makes --after act as a placement gate for
// rentals: downstream jobs are not launched until the upstream job finishes,
// without forcing co-location on a single instance.
//
// If onDefer is non-nil, it is called once for each job that is excluded,
// with the deferral reason.
func filterLaunchJobsByDependencies(database *sql.DB, jobs []*db.Job, onDefer func(job *db.Job, reason string)) []*db.Job {
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		deps := queueblock.ParseJobDependencies(job.DepSpec)
		if len(deps) == 0 {
			filtered = append(filtered, job)
			continue
		}
		reason, ok := queueblock.JobDependenciesSatisfied(database, deps)
		if ok {
			filtered = append(filtered, job)
			continue
		}
		if onDefer != nil {
			onDefer(job, reason)
		}
	}
	return filtered
}

func printDeferredJob(job *db.Job, reason string) {
	fmt.Printf("Job %s deferred: %s\n", ids.FormatJobID(job.ID), reason)
}

// parseLaunchJobIDFilter parses the --jobs flag into a set of job IDs.
// Returns nil for the empty string.
func parseLaunchJobIDFilter(spec string) (map[int64]bool, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	jobIDs, err := ParseJobIDs([]string{spec})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(jobIDs))
	for _, id := range jobIDs {
		out[id] = true
	}
	return out, nil
}

func filterLaunchJobsForScope(jobs []*db.Job, projectFilter string) []*db.Job {
	if projectFilter != "" {
		return db.FilterJobsByProject(jobs, projectFilter)
	}
	return filterLaunchJobsByProject(jobs)
}

func refreshLaunchGroupsWithOnPrem(database *sql.DB, cfg *config.Config, gpuFilter string, projectFilter string, jobIDFilter map[int64]bool, onProgress func(int, int), onPhase func(string)) ([]campaign.InstanceGroup, error) {
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}
	jobs = filterRentalLaunchJobs(jobs)
	jobs = filterLaunchJobsByDependencies(database, jobs, nil)
	jobs = filterLaunchJobsForScope(jobs, projectFilter)
	jobs = db.FilterJobsByIDSet(jobs, jobIDFilter)
	jobs = prefilterOnPremWithProgress(database, jobs, cfg, onProgress, onPhase)

	r2Client, err := buildR2Client(cfg)
	if err != nil {
		slog.Warn("failed to build R2 client for disk estimation", "error", err)
	}
	return campaign.PrepareGroupsWithConfig(jobs, database, cfg, gpuFilter, r2Client), nil
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
	if len(opts.AffinityMachines) > 0 && providerErr == nil {
		clients, providerErr = campaign.MachineIDClients(clients, "machine-constrained launches")
	} else if opts.DistinctMachines && providerErr == nil {
		clients, providerErr = campaign.DistinctMachineClients(clients)
	}
	survivalModel := buildSurvivalModel(database)
	overheadModel := buildOverheadModel(database)
	predCfg := buildPredictorConfig(cfg)
	fmt.Println("Searching for GPU offers...")
	reportPlanProgress := newPlanProgressPrinter(os.Stderr)
	planOptions := campaign.LaunchExecutionPlanOptions{
		DistinctMachines:       opts.DistinctMachines,
		InitialClaimedMachines: campaign.DistinctMachineAvoidanceKeys(opts.AvoidMachines),
		MachineAffinity:        campaign.VastAIMachineKeys(opts.AffinityMachines),
	}
	var prep campaign.LaunchExecutionPlan
	var err error
	if opts.DistinctMachines {
		prep, err = campaign.PrepareNewInstanceLaunchPlanWithOptions(
			database, clients, providerErr, groups, nil,
			opts.ScoringProfile(), opts.MinSurvival, cfg.CampaignReliability(),
			&predCfg, overheadModel, survivalModel, reportPlanProgress, planOptions,
		)
	} else {
		prep, err = campaign.PrepareLaunchExecutionPlanWithProgressAndOptions(
			database,
			clients,
			providerErr,
			groups,
			nil,
			opts.ScoringProfile(),
			opts.MinSurvival,
			cfg.CampaignReliability(),
			&predCfg,
			overheadModel,
			survivalModel,
			reportPlanProgress,
			planOptions,
		)
	}
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
	opts.PlacementAlternatives = campaign.PlacementAlternativesByLaunchGroup(prep.LaunchGroupOffers)
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
		if line := campaign.FormatNewsvendorRecommendation(campaign.RecommendInstanceCount(estimates, opts.ScoringProfile())); line != "" {
			fmt.Println(line)
		}
	}

	if opts.ApplyAutoBudget(estimates) {
		printAutoBudget(opts)
	}

	r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
	var launchTUI *terminal.LaunchProgressTUI
	if watchTUI {
		launchTUI = terminal.StartLaunchProgressTUI(0, len(groupsToLaunch))
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
			fmt.Printf("Launching %d instance(s) in batch %d...\n", len(groupsToLaunch), id)
		},
		nil,
	)
	if launchTUI != nil {
		if err == nil {
			if stopErr := launchTUI.Complete(result.InstanceIDs); stopErr != nil {
				return stopErr
			}
		} else {
			_ = launchTUI.Stop()
		}
	}
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
	fmt.Printf("Launched %d instance(s) in batch %d\n", len(result.InstanceIDs), result.CampaignID)
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
		return watchAndReport(database, watchTUI, mode, watchIDs, campaign.SummarizeEstimates(estimates), projectFilter)
	}

	return nil
}

func runDryRunPlan(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts) error {
	clients, providerErr := buildCloudClients(cfg)
	if providerErr != nil {
		clients = nil
	}
	if len(opts.AffinityMachines) > 0 && providerErr == nil {
		clients, providerErr = campaign.MachineIDClients(clients, "machine-constrained launches")
	} else if opts.DistinctMachines && providerErr == nil {
		clients, providerErr = campaign.DistinctMachineClients(clients)
	}
	survivalModel := buildSurvivalModel(database)
	overheadModel := buildOverheadModel(database)
	predCfg := buildPredictorConfig(cfg)
	var reusable []campaign.InstanceCapacity
	if !opts.DistinctMachines {
		reusable, _ = campaign.FindReusableInstances(database)
	}
	if providerErr != nil && len(reusable) == 0 {
		return providerErr
	}
	reportPlanProgress := newPlanProgressPrinter(os.Stderr)
	plans, _ := campaign.BuildProfilePlansWithProgressAndOptions(
		database,
		clients,
		groups,
		reusable,
		&predCfg,
		overheadModel,
		survivalModel,
		[]bidding.ScoreProfile{opts.ScoringProfile()},
		cfg.CampaignReliability(),
		opts.MinSurvival,
		reportPlanProgress,
		campaign.PlanOptions{
			DistinctMachines:       opts.DistinctMachines,
			InitialClaimedMachines: campaign.DistinctMachineAvoidanceKeys(opts.AvoidMachines),
			MachineAffinity:        campaign.VastAIMachineKeys(opts.AffinityMachines),
		},
	)
	plan, ok := plans[opts.ScoringProfile().ID]
	if !ok {
		return fmt.Errorf("could not build dry-run plan for strategy %s", opts.Strategy)
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

	printSurvivalRejections(groupOffers, opts.MinSurvival)
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
			if go_.Group.HasPreemptibleJob() {
				costStr = fmt.Sprintf("$%.2f/hr (int, bid $%.2f)", go_.Offer.CostPerHour, go_.Offer.CostPerHour)
			}
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
	if line := campaign.FormatNewsvendorRecommendation(campaign.RecommendInstanceCount(estimates, opts.ScoringProfile())); line != "" {
		fmt.Println(line)
	}
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

func printNoRentalLaunchNeeded(database *sql.DB) {
	fmt.Println(noRentalLaunchNeededMessage(database))
}

func noRentalLaunchNeededMessage(database *sql.DB) string {
	msg := "No unplaced jobs need new rental GPUs."
	if n, err := db.CountJobsWaitingOnInstances(database); err == nil && n > 0 {
		msg += " " + pluralizeQueuedJobCount(n) + " already assigned to a rental instance and not started yet."
	}
	return msg
}

func pluralizeQueuedJobCount(n int) string {
	if n == 1 {
		return "1 queued job is"
	}
	return fmt.Sprintf("%d queued jobs are", n)
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
	if project := strings.TrimSpace(instanceLaunchProject); project != "" {
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

// shouldWatch returns true if the launch should segue into watch mode.
// --watch forces it on, --no-watch forces it off, default is on.
func shouldWatch() bool {
	if instanceLaunchWatch {
		return true
	}
	return !instanceLaunchNoWatch
}

func applyRunpodCloudTypeOverride(cfg *config.Config, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	cloudType, err := cloud.NormalizeRunpodCloudType(raw)
	if err != nil {
		return err
	}
	cfg.Runpod.CloudType = cloudType
	return nil
}

func parseLaunchOpts() campaign.LaunchOpts {
	strategy := bidding.SelectionStrategy(instanceLaunchStrategy)
	switch strategy {
	case bidding.StrategyCheap, bidding.StrategyFast, bidding.StrategyFastest:
	default:
		fmt.Fprintf(os.Stderr, "warning: unknown strategy %q, using %q\n", instanceLaunchStrategy, bidding.StrategyCheap)
		strategy = bidding.StrategyCheap
	}
	opts := campaign.LaunchOpts{
		NoDonor:             instanceLaunchNoDonor,
		Strategy:            strategy,
		MinSurvival:         instanceLaunchMinSurvival,
		SkipWorkdirDeletion: instanceLaunchSkipWorkdirDelete,
		DistinctMachines:    instanceLaunchDistinctMachines,
	}
	if instanceLaunchMaxSpend != "" {
		cleaned := strings.TrimPrefix(instanceLaunchMaxSpend, "$")
		var dollars float64
		if _, err := fmt.Sscanf(cleaned, "%f", &dollars); err == nil {
			opts.MaxSpendCents = int(dollars * 100)
		}
	}
	if instanceLaunchMaxTime != "" {
		if d, err := time.ParseDuration(instanceLaunchMaxTime); err == nil {
			opts.MaxTimeSeconds = int(d.Seconds())
		}
	}
	cfg, cfgErr := config.Load()
	gracePeriod := instanceLaunchGracePeriod
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

func applyDistinctMachineLaunchOptions(database *sql.DB, opts *campaign.LaunchOpts) error {
	if opts == nil {
		return nil
	}
	if len(instanceLaunchAvoid) > 0 && !opts.DistinctMachines {
		return fmt.Errorf("--avoid requires --distinct-machines")
	}
	if opts.DistinctMachines {
		avoidMachines, warnings, err := db.ResolveAvoidMachineIDs(database, instanceLaunchAvoid)
		if err != nil {
			return fmt.Errorf("resolve --avoid: %w", err)
		}
		for _, warning := range warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
		}
		opts.AvoidMachines = avoidMachines
	}
	if len(instanceLaunchAffinity) > 0 {
		affinityMachines, warnings, err := db.ResolveAffinityMachineIDs(database, instanceLaunchAffinity)
		if err != nil {
			return fmt.Errorf("resolve --affinity: %w", err)
		}
		for _, warning := range warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
		}
		if len(affinityMachines) == 0 {
			return fmt.Errorf("--affinity did not resolve to any machine_id")
		}
		opts.AffinityMachines = affinityMachines
	}
	if conflict := conflictingMachine(opts.AvoidMachines, opts.AffinityMachines); conflict != "" {
		return fmt.Errorf("--affinity conflicts with --avoid for machine_id %s", conflict)
	}
	return nil
}

func conflictingMachine(avoidMachines, affinityMachines []string) string {
	avoid := campaign.MachineRefKeys(avoidMachines)
	for _, ref := range affinityMachines {
		for key := range campaign.MachineRefKeys([]string{ref}) {
			if _, ok := avoid[key]; ok {
				return key
			}
		}
	}
	return ""
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
