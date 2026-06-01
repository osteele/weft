package cmd

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/credit"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var (
	instanceMarkCreditExhaustedSince          time.Duration
	instanceMarkCreditExhaustedDryRun         bool
	instanceMarkCreditExhaustedYes            bool
	instanceMarkCreditExhaustedIncludeGeneric bool
	instanceMarkCreditExhaustedAuto           bool
	instanceMarkCreditExhaustedAutoLookback   time.Duration
	instanceMarkCreditExhaustedAutoBurstWin   time.Duration
	instanceMarkCreditExhaustedAutoMinSilence time.Duration
)

// creditClusterBeforeWindow / creditClusterAfterWindow define how far
// before/after the strong-signal time range a generic "provider dead"
// row may sit and still be counted as part of the credit-exhaustion
// cluster. Vast destroys running rentals at the moment funds cross zero
// and the subsequent CreateInstance calls begin returning empty
// responses, so destroys precede strong signals by seconds-to-minutes —
// 15 minutes back is generous, 5 minutes forward covers tick jitter.
const (
	creditClusterBeforeWindow = 15 * time.Minute
	creditClusterAfterWindow  = 5 * time.Minute
)

var instanceMarkCreditExhaustedCmd = &cobra.Command{
	Use:   "mark-credit-exhausted [id...]",
	Short: "Reclassify terminated instances as account_credit_exhausted",
	Long: `Reclassify already-terminated cloud instances to record that they failed
because the provider account ran out of credit. Use after an incident where
the provider destroyed running instances for non-payment; weft has no
programmatic signal that distinguishes those from generic provider failures,
so the operator must apply this label retroactively.

The reclassified rows are excluded from the bidding survival model so that
credit-exhaustion failures don't poison machine/SKU/region priors.

Selection:
  - Positional arguments: one or more instance IDs (always reclassified
    if eligible — operator is explicitly naming them).
  - --since DURATION: failed/canceled launches whose ended_at falls within
    the window. By default, only launches whose termination_detail matches
    a credit-exhaustion signature are included: explicit credit-out
    phrases from the provider, plus generic 'provider dead' rows that
    cluster in time with the explicit signals (within ~15 min before /
    ~5 min after). Unrelated failures in the window (dud detection,
    bootstrap timeout, upload stall, etc.) are skipped.
  - --include-generic: also reclassify --since rows that match no credit
    signature but have a generic termination_reason. Use when you know
    every eligible row in the window was credit-related and the detail
    strings are sparse.
  - --auto: auto-detect the most recent credit-exhaustion incident on any
    provider by finding a mass-destroy burst followed by a silent gap (no
    same-provider instance reached running) until a recovery launch came
    up. Provider-specific: a burst on one provider is matched against
    silence and recovery on that same provider only — cross-provider
    matches are never proposed. Rejects bursts followed by a quick
    recovery (regional outage) or by < 90s of silence (transient).
    Mutually exclusive with --since and positional IDs.

Eligibility:
  - Status must be 'failed' or 'canceled'.
  - Current termination_reason must be generic (provider_failure,
    infra_failure, unknown, or empty). Specific reasons such as
    disk_full, job_failure, weft_bug, or completed cannot be overwritten.

The previous termination_detail is preserved (prefixed with a timestamped
note recording the manual reclassification).

Examples:
  weft instance mark-credit-exhausted --auto --dry-run
  weft instance mark-credit-exhausted --auto
  weft instance mark-credit-exhausted --since 24h --dry-run
  weft instance mark-credit-exhausted --since 24h
  weft instance mark-credit-exhausted wi1 wi2 wi3
  weft instance mark-credit-exhausted --since 6h wi42`,
	Args: cobra.ArbitraryArgs,
	RunE: runInstanceMarkCreditExhausted,
}

func init() {
	instanceMarkCreditExhaustedCmd.Flags().DurationVar(&instanceMarkCreditExhaustedSince, "since", 0,
		"Reclassify rows ended within this window (e.g. 1h, 24h). By default only signature-matching rows are included; see --include-generic.")
	instanceMarkCreditExhaustedCmd.Flags().BoolVar(&instanceMarkCreditExhaustedDryRun, "dry-run", false,
		"Print the rows that would be reclassified without writing anything")
	instanceMarkCreditExhaustedCmd.Flags().BoolVarP(&instanceMarkCreditExhaustedYes, "yes", "y", false,
		"Skip interactive confirmation when reclassifying multiple rows")
	instanceMarkCreditExhaustedCmd.Flags().BoolVar(&instanceMarkCreditExhaustedIncludeGeneric, "include-generic", false,
		"With --since, also reclassify rows whose detail does not match a credit signature")
	instanceMarkCreditExhaustedCmd.Flags().BoolVar(&instanceMarkCreditExhaustedAuto, "auto", false,
		"Auto-detect the most recent credit-exhaustion incident (mutually exclusive with --since and positional IDs)")
	instanceMarkCreditExhaustedCmd.Flags().DurationVar(&instanceMarkCreditExhaustedAutoLookback, "auto-lookback", credit.DefaultLookback,
		"With --auto, how far back to search for incidents")
	instanceMarkCreditExhaustedCmd.Flags().DurationVar(&instanceMarkCreditExhaustedAutoBurstWin, "auto-burst-window", credit.DefaultBurstWindow,
		"With --auto, max time span for a burst of failures to count as simultaneous")
	instanceMarkCreditExhaustedCmd.Flags().DurationVar(&instanceMarkCreditExhaustedAutoMinSilence, "auto-min-silence", credit.DefaultMinSilence,
		"With --auto, minimum silence after a burst before declaring credit exhaustion (rejects regional-outage bursts)")
}

func runInstanceMarkCreditExhausted(cmd *cobra.Command, args []string) error {
	if instanceMarkCreditExhaustedAuto {
		if len(args) > 0 || instanceMarkCreditExhaustedSince > 0 || instanceMarkCreditExhaustedIncludeGeneric {
			return usageErrorf("--auto is mutually exclusive with --since, --include-generic, and positional IDs")
		}
	} else if len(args) == 0 && instanceMarkCreditExhaustedSince <= 0 {
		return usageErrorf("specify at least one instance ID, --since DURATION, or --auto")
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	out := cmd.OutOrStdout()
	var plan *markCreditExhaustedPlan
	if instanceMarkCreditExhaustedAuto {
		plan, err = collectAutoDetectedTargets(database, out)
		if err != nil {
			return err
		}
		// --auto's own "no incident detected" message has already been
		// printed; avoid the generic "No eligible instances found." that
		// would otherwise contradict it.
		if plan == nil || len(plan.targets) == 0 {
			return nil
		}
	} else {
		plan, err = collectMarkCreditExhaustedTargets(database, args,
			instanceMarkCreditExhaustedSince, instanceMarkCreditExhaustedIncludeGeneric)
		if err != nil {
			return err
		}
		if plan == nil || (len(plan.targets) == 0 && len(plan.rejected) == 0) {
			fmt.Fprintln(out, "No eligible instances found.")
			return nil
		}
	}

	if len(plan.targets) > 0 {
		printMarkCreditExhaustedPreview(out, plan.targets)
	}
	if len(plan.rejected) > 0 {
		fmt.Fprintf(out, "\nExcluded %d row(s) from --since window (no credit signature in detail).\n",
			len(plan.rejected))
		fmt.Fprintln(out, "Pass --include-generic to reclassify these anyway, or name specific IDs to override.")
		printMarkCreditExhaustedRejections(out, plan.rejected)
	}

	if len(plan.targets) == 0 {
		return nil
	}

	if instanceMarkCreditExhaustedDryRun {
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "Dry run: %d instance(s) would be reclassified as %s.\n",
			len(plan.targets), db.TerminationReasonAccountCreditExhausted)
		return nil
	}

	// Prompt when the operator did not name every row explicitly. --since
	// and --auto are both heuristic selectors, so even a single matched
	// row deserves confirmation; only when the operator named every ID
	// explicitly AND there is just one target do we skip the prompt.
	// --yes always skips.
	needsPrompt := !instanceMarkCreditExhaustedYes &&
		(instanceMarkCreditExhaustedAuto ||
			instanceMarkCreditExhaustedSince > 0 ||
			len(plan.targets) > 1)
	if needsPrompt {
		if !confirmMarkCreditExhausted(cmd.InOrStdin(), out, len(plan.targets)) {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	now := time.Now().UTC()
	detailPrefix := fmt.Sprintf("reclassified as %s at %s",
		db.TerminationReasonAccountCreditExhausted, now.Format(time.RFC3339))

	var failures []string
	updated := 0
	for _, t := range plan.targets {
		if err := db.ReclassifyLaunchTerminationReason(database, t.ID,
			db.TerminationReasonAccountCreditExhausted, detailPrefix); err != nil {
			failures = append(failures,
				fmt.Sprintf("  %s: %v", ids.FormatInstanceID(t.ID), err))
			continue
		}
		updated++
	}

	fmt.Fprintf(out,
		"\nReclassified %d instance(s) as %s.\n",
		updated, db.TerminationReasonAccountCreditExhausted)
	if len(failures) > 0 {
		fmt.Fprintf(out, "\n%d instance(s) could not be reclassified:\n", len(failures))
		for _, f := range failures {
			fmt.Fprintln(out, f)
		}
		return fmt.Errorf("%d reclassification(s) failed", len(failures))
	}
	return nil
}

// markCreditExhaustedPlan is the result of validating CLI inputs and
// running the credit-signature filter against the --since window.
type markCreditExhaustedPlan struct {
	targets  []*db.Launch         // rows that will be reclassified
	rejected []rejectedReclassify // rows excluded by signature filter (--since only)
}

// collectAutoDetectedTargets runs the credit incident detector and turns the
// resulting Incident.Members into a reclassify plan. It also prints the
// incident banner and any rejection diagnostics so the operator can see why
// neighboring candidate bursts were skipped.
func collectAutoDetectedTargets(database *sql.DB, out io.Writer) (*markCreditExhaustedPlan, error) {
	cfg := credit.Config{
		Lookback:    instanceMarkCreditExhaustedAutoLookback,
		BurstWindow: instanceMarkCreditExhaustedAutoBurstWin,
		MinSilence:  instanceMarkCreditExhaustedAutoMinSilence,
	}
	res, err := credit.DetectFromDB(database, cfg, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("detect credit-exhaustion incident: %w", err)
	}
	if res.Incident == nil {
		fmt.Fprintf(out, "No credit-exhaustion incident detected in the last %s.\n",
			durationOrDefault(instanceMarkCreditExhaustedAutoLookback, credit.DefaultLookback))
		if len(res.Rejections) > 0 {
			fmt.Fprintln(out, "\nRejected candidate bursts:")
			printIncidentRejections(out, res.Rejections)
		}
		return &markCreditExhaustedPlan{}, nil
	}
	printIncidentBanner(out, res.Incident)
	if len(res.Rejections) > 0 {
		fmt.Fprintf(out, "\nAlso rejected %d earlier candidate(s):\n", len(res.Rejections))
		printIncidentRejections(out, res.Rejections)
		fmt.Fprintln(out)
	}
	return &markCreditExhaustedPlan{targets: res.Incident.Members}, nil
}

func printIncidentBanner(w io.Writer, inc *credit.Incident) {
	fmt.Fprintf(w, "Detected credit-exhaustion incident on %s:\n", inc.Provider)
	burstSpan := inc.BurstEnd.Sub(inc.BurstStart).Truncate(time.Second)
	fmt.Fprintf(w, "  Burst:    %d instances destroyed within %s, peak %s\n",
		inc.BurstCount, burstSpan, inc.BurstEnd.Local().Format("2006-01-02 15:04:05 MST"))
	if inc.Ongoing {
		fmt.Fprintf(w, "  Silence:  %s and counting (no %s instance has reached running)\n",
			inc.Silence.Truncate(time.Second), inc.Provider)
		fmt.Fprintf(w, "  Recovery: not yet — rerun later if more failures arrive\n")
	} else {
		fmt.Fprintf(w, "  Silence:  %s (no %s instance reached running)\n",
			inc.Silence.Truncate(time.Second), inc.Provider)
		if inc.RecoveryLaunch != nil {
			fmt.Fprintf(w, "  Recovery: %s  (%s)\n",
				inc.Recovery.Local().Format("2006-01-02 15:04:05 MST"),
				ids.FormatInstanceID(inc.RecoveryLaunch.ID))
		} else {
			fmt.Fprintf(w, "  Recovery: %s\n",
				inc.Recovery.Local().Format("2006-01-02 15:04:05 MST"))
		}
	}
	fmt.Fprintf(w, "  Window:   %s → %s\n\n",
		inc.WindowStart.Local().Format("2006-01-02 15:04:05 MST"),
		inc.WindowEnd.Local().Format("2006-01-02 15:04:05 MST"))
}

func printIncidentRejections(w io.Writer, rejections []credit.Rejection) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tBURST PEAK\tCOUNT\tREASON\tDETAIL")
	for _, r := range rejections {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			r.Provider,
			r.BurstEnd.Local().Format("2006-01-02 15:04"),
			r.BurstCount,
			r.Reason,
			r.Detail,
		)
	}
	_ = tw.Flush()
}

func durationOrDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// rejectedReclassify carries a launch the smart filter excluded and the
// human-readable reason — surfaced in the preview so the operator can see
// why a row in the time window was skipped.
type rejectedReclassify struct {
	launch *db.Launch
	reason string
}

// collectMarkCreditExhaustedTargets builds the reclassify plan. Explicit
// IDs are always included after eligibility checks. The --since window is
// filtered by credit signature unless includeGeneric is true: strong
// signatures (CreateInstance credit errors) are always included; generic
// "provider dead" rows are included only when clustered in time with the
// strong signals.
func collectMarkCreditExhaustedTargets(database *sql.DB, args []string, since time.Duration, includeGeneric bool) (*markCreditExhaustedPlan, error) {
	plan := &markCreditExhaustedPlan{}
	seen := make(map[int64]*db.Launch)

	for _, arg := range args {
		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			return nil, fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		launch, err := db.GetLaunch(database, id)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", ids.FormatInstanceID(id), err)
		}
		if launch == nil {
			return nil, fmt.Errorf("instance %s not found", ids.FormatInstanceID(id))
		}
		if launch.Status != db.LaunchStatusFailed && launch.Status != db.LaunchStatusCancelled {
			return nil, fmt.Errorf("instance %s has status %q; reclassify only allowed on failed/canceled instances",
				ids.FormatInstanceID(id), launch.Status)
		}
		if !db.IsReclassifyEligibleReason(launch.TerminationReason) {
			return nil, fmt.Errorf("instance %s has termination_reason %q which is not eligible for reclassification",
				ids.FormatInstanceID(id), launch.TerminationReason)
		}
		seen[id] = launch
	}

	if since > 0 {
		sinceUnix := time.Now().Add(-since).Unix()
		launches, err := db.ListReclassifyEligibleLaunches(database, sinceUnix)
		if err != nil {
			return nil, fmt.Errorf("query eligible launches: %w", err)
		}
		included, rejected := filterCreditExhaustionCandidates(launches, includeGeneric)
		for _, l := range included {
			if _, ok := seen[l.ID]; !ok {
				seen[l.ID] = l
			}
		}
		// Surface rejections only for rows the operator didn't also name
		// explicitly — those are already going through.
		for _, r := range rejected {
			if _, named := seen[r.launch.ID]; !named {
				plan.rejected = append(plan.rejected, r)
			}
		}
	}

	plan.targets = make([]*db.Launch, 0, len(seen))
	for _, l := range seen {
		plan.targets = append(plan.targets, l)
	}
	sortLaunchesNewestFirst(plan.targets)
	sort.Slice(plan.rejected, func(i, j int) bool {
		ai, aj := int64(0), int64(0)
		if plan.rejected[i].launch.EndedAt != nil {
			ai = *plan.rejected[i].launch.EndedAt
		}
		if plan.rejected[j].launch.EndedAt != nil {
			aj = *plan.rejected[j].launch.EndedAt
		}
		if ai != aj {
			return ai > aj
		}
		return plan.rejected[i].launch.ID > plan.rejected[j].launch.ID
	})
	return plan, nil
}

// filterCreditExhaustionCandidates partitions eligible launches into
// reclassify targets and rejections based on credit-detail signatures.
// When includeGeneric is true, every launch is included unconditionally.
func filterCreditExhaustionCandidates(launches []*db.Launch, includeGeneric bool) (included []*db.Launch, rejected []rejectedReclassify) {
	if includeGeneric {
		included = append(included, launches...)
		return included, nil
	}

	var strong []*db.Launch
	var cluster []*db.Launch
	var none []*db.Launch
	for _, l := range launches {
		switch db.ClassifyCreditSignal(l.TerminationDetail) {
		case db.CreditSignalStrong:
			strong = append(strong, l)
		case db.CreditSignalCluster:
			cluster = append(cluster, l)
		default:
			none = append(none, l)
		}
	}
	included = append(included, strong...)

	if len(strong) == 0 {
		for _, l := range cluster {
			rejected = append(rejected, rejectedReclassify{
				launch: l,
				reason: "provider-dead row, but no credit signature in window to anchor a cluster",
			})
		}
		for _, l := range none {
			rejected = append(rejected, rejectedReclassify{
				launch: l,
				reason: "no credit signature in termination_detail",
			})
		}
		return included, rejected
	}

	// Anchor the cluster window on the strong-signal min/max ended_at.
	var minStrong, maxStrong int64 = math.MaxInt64, math.MinInt64
	for _, l := range strong {
		if l.EndedAt == nil {
			continue
		}
		if *l.EndedAt < minStrong {
			minStrong = *l.EndedAt
		}
		if *l.EndedAt > maxStrong {
			maxStrong = *l.EndedAt
		}
	}
	if minStrong == math.MaxInt64 {
		// All strong rows lack ended_at — refuse to anchor.
		for _, l := range cluster {
			rejected = append(rejected, rejectedReclassify{
				launch: l,
				reason: "no anchorable strong signal (missing ended_at)",
			})
		}
		for _, l := range none {
			rejected = append(rejected, rejectedReclassify{
				launch: l,
				reason: "no credit signature in termination_detail",
			})
		}
		return included, rejected
	}
	windowStart := minStrong - int64(creditClusterBeforeWindow.Seconds())
	windowEnd := maxStrong + int64(creditClusterAfterWindow.Seconds())
	for _, l := range cluster {
		if l.EndedAt == nil {
			rejected = append(rejected, rejectedReclassify{
				launch: l,
				reason: "missing ended_at; cannot place in cluster window",
			})
			continue
		}
		if *l.EndedAt >= windowStart && *l.EndedAt <= windowEnd {
			included = append(included, l)
		} else {
			delta := time.Duration(0)
			if *l.EndedAt < windowStart {
				delta = time.Duration(windowStart-*l.EndedAt) * time.Second
			} else {
				delta = time.Duration(*l.EndedAt-windowEnd) * time.Second
			}
			rejected = append(rejected, rejectedReclassify{
				launch: l,
				reason: fmt.Sprintf("provider-dead %s outside credit-cluster window", delta.Truncate(time.Second)),
			})
		}
	}
	for _, l := range none {
		rejected = append(rejected, rejectedReclassify{
			launch: l,
			reason: "no credit signature in termination_detail",
		})
	}
	return included, rejected
}

func sortLaunchesNewestFirst(launches []*db.Launch) {
	sort.Slice(launches, func(i, j int) bool {
		ai, aj := int64(0), int64(0)
		if launches[i].EndedAt != nil {
			ai = *launches[i].EndedAt
		}
		if launches[j].EndedAt != nil {
			aj = *launches[j].EndedAt
		}
		if ai != aj {
			return ai > aj
		}
		return launches[i].ID > launches[j].ID
	})
}

func printMarkCreditExhaustedRejections(w io.Writer, rejected []rejectedReclassify) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INSTANCE\tENDED\tREASON\tWHY EXCLUDED\tDETAIL")
	for _, r := range rejected {
		t := r.launch
		ended := "-"
		if t.EndedAt != nil {
			ended = time.Unix(*t.EndedAt, 0).Local().Format("2006-01-02 15:04")
		}
		reason := t.TerminationReason
		if reason == "" {
			reason = "(empty)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ids.FormatInstanceID(t.ID),
			ended,
			reason,
			r.reason,
			truncateDetail(t.TerminationDetail, 60),
		)
	}
	_ = tw.Flush()
}

func printMarkCreditExhaustedPreview(w io.Writer, targets []*db.Launch) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INSTANCE\tPROVIDER\tGPU\tENDED\tREASON\tDETAIL")
	for _, t := range targets {
		ended := "-"
		if t.EndedAt != nil {
			ended = time.Unix(*t.EndedAt, 0).Local().Format("2006-01-02 15:04")
		}
		reason := t.TerminationReason
		if reason == "" {
			reason = "(empty)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ids.FormatInstanceID(t.ID),
			fallbackString(t.Provider, "-"),
			fallbackString(t.GPUSpec, "-"),
			ended,
			reason,
			truncateDetail(t.TerminationDetail, 60),
		)
	}
	_ = tw.Flush()
}

func truncateDetail(s string, n int) string {
	if s == "" {
		return "-"
	}
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func fallbackString(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func confirmMarkCreditExhausted(in io.Reader, out io.Writer, n int) bool {
	fmt.Fprintf(out, "\nReclassify %d instance(s) as %s? [y/N] ",
		n, db.TerminationReasonAccountCreditExhausted)
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes"
}
