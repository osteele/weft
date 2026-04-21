package cmd

import (
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// Filter-layer flags new to `weft place` / `weft jobs place`. The
// --project flag is reused from addCampaignLaunchFlags (shared global
// campaignLaunchProject); other launch-mechanics flags (--watch, --yes,
// --dry-run, --grace-period, ...) are also inherited from there, so the
// two entry points accept the same surface.
var (
	placeStatus string
	placeAll    bool
)

var placeCmd = &cobra.Command{
	Use:     "place [job-id]...",
	Aliases: []string{"submit"},
	Short:   "Launch cloud instances for queued unplaced jobs",
	Long: `Place queued unplaced jobs by launching cloud instances for them.

Without arguments, all queued unplaced jobs are considered. Filter with
positional job IDs, --project, or --status.

Use --watch to enter watch mode after launching; --yes to skip the
interactive confirmation; --dry-run to preview the plan without launching.

Examples:
  weft place                        # all queued unplaced jobs (interactive)
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
	addCampaignLaunchFlags(cmd)
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
		campaignLaunchJobs = joinJobIDs(jobIDs)
	}
	// --project is already bound to campaignLaunchProject via the shared
	// campaign-launch flag set, so no translation is needed here.

	return runCampaignLaunch(cmd, nil)
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
		if strings.TrimSpace(campaignLaunchProject) != "" {
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
		// (optionally filtered by --project via campaignLaunchProject).
		return nil, nil
	}
	return ParseJobIDs(args)
}

// joinJobIDs produces the comma-separated numeric form consumed by the
// campaign-launch --jobs flag (strconv.ParseInt upstream). Sorts a copy so
// callers' slices are not mutated.
func joinJobIDs(jobIDs []int64) string {
	sorted := slices.Clone(jobIDs)
	slices.Sort(sorted)
	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}
