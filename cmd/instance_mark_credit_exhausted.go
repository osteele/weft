package cmd

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var (
	instanceMarkCreditExhaustedSince  time.Duration
	instanceMarkCreditExhaustedDryRun bool
	instanceMarkCreditExhaustedYes    bool
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
  - Positional arguments: one or more instance IDs.
  - --since DURATION: every failed/canceled launch whose ended_at falls
    within the window and whose current termination_reason is generic
    (provider_failure, infra_failure, unknown, or empty).
  - Both can be combined; the union is reclassified.

Eligibility:
  - Status must be 'failed' or 'canceled'.
  - Current termination_reason must be generic (see above). Specific reasons
    such as disk_full, job_failure, weft_bug, or completed cannot be
    overwritten.

The previous termination_detail is preserved (prefixed with a timestamped
note recording the manual reclassification).

Examples:
  weft instance mark-credit-exhausted --since 24h --dry-run
  weft instance mark-credit-exhausted --since 24h
  weft instance mark-credit-exhausted wi1 wi2 wi3
  weft instance mark-credit-exhausted --since 6h wi42`,
	Args: cobra.ArbitraryArgs,
	RunE: runInstanceMarkCreditExhausted,
}

func init() {
	instanceMarkCreditExhaustedCmd.Flags().DurationVar(&instanceMarkCreditExhaustedSince, "since", 0,
		"Reclassify every eligible instance whose ended_at falls within this window (e.g. 1h, 24h)")
	instanceMarkCreditExhaustedCmd.Flags().BoolVar(&instanceMarkCreditExhaustedDryRun, "dry-run", false,
		"Print the rows that would be reclassified without writing anything")
	instanceMarkCreditExhaustedCmd.Flags().BoolVarP(&instanceMarkCreditExhaustedYes, "yes", "y", false,
		"Skip interactive confirmation when reclassifying multiple rows")
}

func runInstanceMarkCreditExhausted(cmd *cobra.Command, args []string) error {
	if len(args) == 0 && instanceMarkCreditExhaustedSince <= 0 {
		return usageErrorf("specify at least one instance ID or --since DURATION")
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	targets, err := collectMarkCreditExhaustedTargets(database, args, instanceMarkCreditExhaustedSince)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(targets) == 0 {
		fmt.Fprintln(out, "No eligible instances found.")
		return nil
	}

	printMarkCreditExhaustedPreview(out, targets)

	if instanceMarkCreditExhaustedDryRun {
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "Dry run: %d instance(s) would be reclassified as %s.\n",
			len(targets), db.TerminationReasonAccountCreditExhausted)
		return nil
	}

	// Prompt when the operator did not name every row explicitly: --since
	// can match a single unrelated row, and silently mutating it has no
	// undo. Skip the prompt only when the operator named all IDs by hand
	// (no --since) and a single target was found, or when --yes was given.
	needsPrompt := !instanceMarkCreditExhaustedYes &&
		(instanceMarkCreditExhaustedSince > 0 || len(targets) > 1)
	if needsPrompt {
		if !confirmMarkCreditExhausted(cmd.InOrStdin(), out, len(targets)) {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	now := time.Now().UTC()
	detailPrefix := fmt.Sprintf("reclassified as %s at %s",
		db.TerminationReasonAccountCreditExhausted, now.Format(time.RFC3339))

	var failures []string
	updated := 0
	for _, t := range targets {
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

// collectMarkCreditExhaustedTargets builds the deduplicated list of launches
// to reclassify, validating explicit IDs and merging in the --since window.
func collectMarkCreditExhaustedTargets(database *sql.DB, args []string, since time.Duration) ([]*db.Launch, error) {
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
		for _, l := range launches {
			if _, ok := seen[l.ID]; !ok {
				seen[l.ID] = l
			}
		}
	}

	out := make([]*db.Launch, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := int64(0), int64(0)
		if out[i].EndedAt != nil {
			ai = *out[i].EndedAt
		}
		if out[j].EndedAt != nil {
			aj = *out[j].EndedAt
		}
		if ai != aj {
			return ai > aj
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
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
