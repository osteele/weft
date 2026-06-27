package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

// Filter-layer flags new to `weft place` / `weft jobs place`. The
// --project flag is reused from addInstanceLaunchFlags (shared global
// instanceLaunchProject); other launch-mechanics flags (--watch, --yes,
// --dry-run, --grace-period, ...) are also inherited from there, so the
// two entry points accept the same surface.
var (
	placeStatus string
	placeAll    bool
)

var placeCmd = &cobra.Command{
	Use:     "place [job-id]...",
	Aliases: []string{"submit"},
	Short:   "Launch cloud instances for queued jobs that are not assigned yet",
	Long: `Launch cloud instances for queued jobs that are not assigned yet.

Without arguments, all queued jobs with no assigned host or instance are considered. Filter with
positional job IDs, --project, or --status.

Use --watch to enter watch mode after launching; --yes to skip the
interactive confirmation; --dry-run to preview the plan without launching.

Examples:
  weft place                        # all unassigned queued jobs (interactive)
  weft place wj42 wj43              # only these jobs
  weft place --project myproj       # unplaced jobs from one project
  weft place --all --yes --watch    # launch everything, then watch`,
	Args: usageArgs(cobra.ArbitraryArgs),
	RunE: runPlace,
}

var jobsPlaceCmd = verbAlias("place [job-id]...", placeCmd)

func init() {
	jobsPlaceCmd.Aliases = placeCmd.Aliases
	rootCmd.AddCommand(placeCmd)
	jobCmd.AddCommand(jobsPlaceCmd)
	addPlaceFlags(placeCmd)
	addPlaceFlags(jobsPlaceCmd)
}

func addPlaceFlags(cmd *cobra.Command) {
	// Shared flags first so our new filter flags can override their help
	// text where the wording differs on the `place` surface.
	addInstanceLaunchFlags(cmd)
	cmd.Flags().StringVar(&placeStatus, "status", "", "Filter by effective status (default: queued; 'unplaced' accepted as alias)")
	cmd.Flags().BoolVar(&placeAll, "all", false, "All queued unplaced jobs (explicit; same as no arguments)")
	// Filter surface uses positional args / --project / --all; the legacy
	// comma-separated --jobs flag is hidden here to avoid two ways to do
	// the same thing.
	_ = cmd.Flags().MarkHidden("jobs")
}

func runPlace(cmd *cobra.Command, args []string) error {
	if err := validatePlaceFilters(cmd, args); err != nil {
		return err
	}

	jobIDs, err := resolvePlaceTargetJobIDs(args)
	if err != nil {
		return err
	}

	// Translate the filter layer into campaign-launch flag state so the
	// existing machinery (offer selection, donor strategy, watch, etc.)
	// runs unchanged.
	if len(jobIDs) > 0 {
		instanceLaunchJobs = joinJobIDs(jobIDs)
	}
	// --project is already bound to instanceLaunchProject via the shared
	// campaign-launch flag set, so no translation is needed here.

	return runPlaceWithAutopilotLease(cmd, func() error {
		return runInstanceLaunch(cmd, nil)
	})
}

func runPlaceWithAutopilotLease(cmd *cobra.Command, run func() error) (err error) {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database for placement lease: %w", err)
	}
	defer database.Close()

	runner := orchestration.NewAutopilotRunnerWithOptions(database, "manual-place", orchestration.AutopilotRunnerOptions{
		IgnorePaused: true,
	})
	if err := runner.TryAcquire(); err != nil {
		if errors.Is(err, orchestration.ErrAutopilotBusy) {
			return fmt.Errorf("autopilot is currently placing jobs; retry when it is idle")
		}
		return err
	}
	started := time.Now()
	defer func() {
		_ = runner.Release(time.Since(started), "manual place", err)
	}()
	return run()
}

func validatePlaceFilters(cmd *cobra.Command, args []string) error {
	if placeStatus != "" {
		normalized := strings.ToLower(strings.TrimSpace(placeStatus))
		switch normalized {
		case "queued", "unplaced":
			// Both are acceptable names for the same target set.
		default:
			return usageErrorf("--status %q is not supported; only 'queued' / 'unplaced' (the default) are accepted for placement", placeStatus)
		}
	}
	if placeAll {
		if len(args) > 0 {
			return usageErrorf("--all cannot be combined with job ID arguments")
		}
		if strings.TrimSpace(instanceLaunchProject) != "" {
			return usageErrorf("--all cannot be combined with --project")
		}
	}
	// --jobs is hidden on this surface and overwritten below when positional
	// args are given; reject the combination explicitly rather than silently
	// discarding the user's --jobs value.
	if len(args) > 0 && cmd.Flags().Changed("jobs") {
		return usageErrorf("pass job IDs positionally or via --jobs, not both")
	}
	return nil
}

func resolvePlaceTargetJobIDs(args []string) ([]int64, error) {
	if len(args) == 0 {
		// No explicit selection: default to all queued unplaced jobs
		// (optionally filtered by --project via instanceLaunchProject).
		return nil, nil
	}
	return ParseJobIDs(args)
}

// joinJobIDs produces the comma-separated form passed to the instance-launch
// --jobs flag. Sorts a copy so callers' slices are not mutated.
func joinJobIDs(jobIDs []int64) string {
	sorted := slices.Clone(jobIDs)
	slices.Sort(sorted)
	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}
