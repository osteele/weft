package cmd

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	authorizePriceUpToFlag     string
	authorizePriceForClassFlag bool
	authorizePriceListFlag     bool
	authorizePriceClearFlag    bool
	authorizePriceNoteFlag     string
)

var jobAuthorizePriceCmd = &cobra.Command{
	Use:   "authorize-price <job-id> [--up-to $X/hr] [--for-class]",
	Short: "Authorize cloud-rental spend above the history-derived anchor",
	Long: `Authorize a per-job (default) or per-class (with --for-class) hourly
ceiling that overrides the placement gate's history-derived anchor.

The gate computes an anchor from cost_per_hour_cents over recent successful
launches in the same (gpu_class, gpu_mem_gb) bucket; offers up to anchor*1.5
auto-approve. Offers above the ceiling block placement with
"requires_price_authorization" until you explicitly authorize the spend here.

Per-job (default) sticks to the named job only. --for-class raises the
class-level anchor for every future job in the same bucket.

Examples:
  # Authorize this one job at up to $2.50/hr
  weft job authorize-price wj2404 --up-to 2.50

  # Raise the class anchor for all A100 80GB jobs to $2/hr (sticky)
  weft job authorize-price wj2404 --up-to 2.00 --for-class

  # List existing class-level authorizations
  weft job authorize-price --list

  # Clear a per-job authorization
  weft job authorize-price wj2404 --clear`,
	RunE: runJobAuthorizePrice,
}

func init() {
	jobAuthorizePriceCmd.Flags().StringVar(&authorizePriceUpToFlag, "up-to", "", "Ceiling in dollars per hour (e.g. \"2.50\")")
	jobAuthorizePriceCmd.Flags().BoolVar(&authorizePriceForClassFlag, "for-class", false, "Set a class-level (gpu_class, gpu_mem_gb) sticky authorization instead of per-job")
	jobAuthorizePriceCmd.Flags().BoolVar(&authorizePriceListFlag, "list", false, "List existing class-level authorizations")
	jobAuthorizePriceCmd.Flags().BoolVar(&authorizePriceClearFlag, "clear", false, "Clear the authorization (per-job by default, or class-level with --for-class)")
	jobAuthorizePriceCmd.Flags().StringVar(&authorizePriceNoteFlag, "note", "", "Optional note recorded with class-level authorizations")
	jobCmd.AddCommand(jobAuthorizePriceCmd)
}

func runJobAuthorizePrice(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if authorizePriceListFlag {
		return printClassAuthorizations(cmd, database)
	}

	if len(args) == 0 {
		return fmt.Errorf("expected a job id; pass --list to list class-level authorizations")
	}
	jobIDs, err := ParseJobIDs(args[:1])
	if err != nil {
		return err
	}
	if len(jobIDs) != 1 {
		return fmt.Errorf("expected exactly one job id, got %d", len(jobIDs))
	}
	jobID := jobIDs[0]
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("read job wj%d: %w", jobID, err)
	}
	if job == nil {
		return fmt.Errorf("job wj%d not found", jobID)
	}

	if authorizePriceClearFlag {
		return clearAuthorization(cmd, database, job)
	}

	if authorizePriceUpToFlag == "" {
		return fmt.Errorf("--up-to is required when setting an authorization (use --clear to remove)")
	}
	cents, err := parseDollarsPerHourToCents(authorizePriceUpToFlag)
	if err != nil {
		return err
	}

	if authorizePriceForClassFlag {
		if job.GPUClass == "" || job.GPUMemGB == nil || *job.GPUMemGB <= 0 {
			return fmt.Errorf("job wj%d has no gpu_class / gpu_mem_gb to scope a class-level authorization", jobID)
		}
		if err := db.UpsertClassPriceAuthorization(database, job.GPUClass, *job.GPUMemGB, cents, "", authorizePriceNoteFlag); err != nil {
			return fmt.Errorf("upsert class authorization: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Authorized %s ≥%dGB up to $%.2f/hr (class-level, sticky for future jobs in this bucket).\n",
			job.GPUClass, *job.GPUMemGB, float64(cents)/100.0)
		clearPendingPriceAuthBlock(database, job)
		return nil
	}
	if err := db.SetJobPriceAuthorization(database, jobID, cents); err != nil {
		return fmt.Errorf("set per-job authorization: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Authorized wj%d up to $%.2f/hr (per-job override).\n", jobID, float64(cents)/100.0)
	clearPendingPriceAuthBlock(database, job)
	return nil
}

func clearAuthorization(cmd *cobra.Command, database *sql.DB, job *db.Job) error {
	if authorizePriceForClassFlag {
		if job.GPUClass == "" || job.GPUMemGB == nil || *job.GPUMemGB <= 0 {
			return fmt.Errorf("job wj%d has no gpu_class / gpu_mem_gb to scope a class-level clear", job.ID)
		}
		if err := db.DeleteClassPriceAuthorization(database, job.GPUClass, *job.GPUMemGB); err != nil {
			return fmt.Errorf("delete class authorization: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Cleared class-level authorization for %s ≥%dGB.\n", job.GPUClass, *job.GPUMemGB)
		return nil
	}
	if err := db.ClearJobPriceAuthorization(database, job.ID); err != nil {
		return fmt.Errorf("clear per-job authorization: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Cleared per-job authorization for wj%d.\n", job.ID)
	return nil
}

func printClassAuthorizations(cmd *cobra.Command, database *sql.DB) error {
	rows, err := db.ListClassPriceAuthorizations(database)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No class-level price authorizations.")
		return nil
	}
	for _, a := range rows {
		fmt.Fprintf(cmd.OutOrStdout(), "%s ≥%dGB  up to $%.2f/hr  set %s",
			a.GPUClass, a.GPUMemGB, float64(a.UpToCents)/100.0,
			a.CreatedAt.Format("2006-01-02"),
		)
		if a.Note != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  note=%q", a.Note)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return nil
}

// parseDollarsPerHourToCents parses a "$X.YY" string into integer cents.
// Accepts a leading "$" and trailing "/hr" or "/h" for ergonomics.
func parseDollarsPerHourToCents(s string) (int, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "$")
	raw = strings.TrimSuffix(raw, "/hr")
	raw = strings.TrimSuffix(raw, "/h")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty price")
	}
	dollars, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q as dollars: %w", s, err)
	}
	if dollars <= 0 {
		return 0, fmt.Errorf("price must be positive (got %s)", s)
	}
	return int(dollars*100 + 0.5), nil
}

// clearPendingPriceAuthBlock removes the placement_blocked and
// placement_reasons rows the gate may have left on the job, so the next
// autopilot pass treats it as freshly-unplaced rather than re-reading the
// stale "requires_price_authorization" message that has now been resolved.
func clearPendingPriceAuthBlock(database *sql.DB, job *db.Job) {
	_ = db.SetJobPlacementBlocked(database, job.ID, "")
	_ = db.SetJobPlacementReasons(database, job.ID, nil)
}
