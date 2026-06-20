package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	diskCalibProject  string
	diskCalibCoverage float64
	diskCalibJSON     bool
	diskCalibRows     bool
)

var jobDiskCalibrationCmd = &cobra.Command{
	Use:   "disk-calibration",
	Short: "Calibrate disk-size safety constants against actual usage",
	Long: `Compare the disk estimator against actual recorded disk usage to put the
safety constants on an empirical footing.

The unit of measurement is the cloud instance (launch), because a fresh
container has a dedicated HF cache and a dedicated filesystem. For each launch
this recomputes the estimate with the CURRENT algorithm (not the value persisted
at launch time) over the group of jobs that ran on it, and compares it against
what the instance actually used:

  - HF multiplier:  actual HF cache size (job_phase_timings.cache_hf_post_bytes)
                    divided by the recomputed raw HF byte sum (the union of all
                    declared models across the launch's jobs) that HFCacheMultiplier
                    multiplies. This isolates the 1.5x constant.
  - Whole estimate: actual peak disk divided by the recomputed full estimate for
                    the launch's job group. Informs the overall safety stack
                    (1.15x + 5GB), but is noisier because uv/source components are
                    not recomputable once a job's working tree is gone.

On-prem jobs are excluded: they share an HF cache and probe the whole host
filesystem, so neither signal reflects the job's own footprint. Read-only; no
cloud launches.`,
	RunE: runJobDiskCalibration,
}

func init() {
	jobDiskCalibrationCmd.Flags().StringVar(&diskCalibProject, "project", "", "Limit to a single project")
	jobDiskCalibrationCmd.Flags().Float64Var(&diskCalibCoverage, "coverage", 0.99, "Target coverage quantile for the recommended constant")
	jobDiskCalibrationCmd.Flags().BoolVar(&diskCalibJSON, "json", false, "Print machine-readable JSON (includes per-launch rows)")
	jobDiskCalibrationCmd.Flags().BoolVar(&diskCalibRows, "rows", false, "Print the per-launch rows table (text mode)")
	jobCmd.AddCommand(jobDiskCalibrationCmd)
}

// diskCalibRow is one launch's recomputed-estimate-vs-actual comparison.
type diskCalibRow struct {
	LaunchID         int64   `json:"launch_id"`
	Jobs             []int64 `json:"jobs"`
	Project          string  `json:"project,omitempty"`
	Reason           string  `json:"termination_reason,omitempty"`
	UnionRawHFBytes  int64   `json:"union_raw_hf_bytes"`
	HFCachePostBytes int64   `json:"hf_cache_post_bytes"`
	HFRatio          float64 `json:"hf_ratio,omitempty"`
	EstimateGB       int     `json:"estimate_gb"`
	PeakDiskBytes    int64   `json:"peak_disk_bytes"`
	WholeRatio       float64 `json:"whole_ratio,omitempty"`
}

// residualSummary is the quantile summary of a residual-ratio distribution.
type residualSummary struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

type diskCalibrationReport struct {
	Coverage float64 `json:"coverage"`

	HFMultiplier             *residualSummary `json:"hf_multiplier"`
	CurrentHFMultiplier      float64          `json:"current_hf_multiplier"`
	RecommendedHFMult        float64          `json:"recommended_hf_multiplier,omitempty"`
	HFRecommendationReliable bool             `json:"hf_recommendation_reliable"`

	WholeEstimate        *residualSummary `json:"whole_estimate"`
	CurrentEmpiricalMult float64          `json:"current_empirical_multiplier"`
	WholeUndersizedCount int              `json:"whole_estimate_undersized_count"`
	WholeUndersizedJobs  []int64          `json:"whole_estimate_undersized_launches,omitempty"`

	LaunchesTotal        int `json:"launches_with_telemetry"`
	ExcludedUntrusted    int `json:"excluded_untrustworthy_termination"`
	DiskFullCount        int `json:"disk_full_count"`
	MalformedRefLaunches int `json:"malformed_ref_launches"`
	SkippedNoUnionHF     int `json:"skipped_no_resolvable_hf"`
	SkippedUnresolvedHF  int `json:"skipped_partially_unresolved_hf"`
	SkippedAnomalyHF     int `json:"skipped_implausible_hf_cache"`
	SkippedAnomalyPeak   int `json:"skipped_implausible_peak"`

	Rows []diskCalibRow `json:"rows,omitempty"`
}

// launchRow holds the raw per-launch telemetry pulled before recomputation.
type launchRow struct {
	launchID    int64
	reason      string
	project     string
	jobIDs      []int64
	hfCachePost int64 // max cache_hf_post_bytes over the launch's jobs
	diskUsed    int64 // max post-job disk_used_bytes over the launch's jobs
	summaryPeak int64 // max timeseries peak over the launch's jobs
}

func runJobDiskCalibration(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	launches, err := loadDiskCalibLaunches(database, diskCalibProject)
	if err != nil {
		return err
	}

	bound := campaign.DiskTelemetryPlausibilityBytes
	report := diskCalibrationReport{
		Coverage:             diskCalibCoverage,
		CurrentHFMultiplier:  campaign.HFCacheMultiplier,
		CurrentEmpiricalMult: campaign.EmpiricalDiskSafetyMultiplier,
		LaunchesTotal:        len(launches),
	}

	var hfRatios, wholeRatios []float64
	for _, lr := range launches {
		// Only trust instances that actually ran their workload. Excludes
		// disk_full (truncated cache / capped peak) and infrastructure failures
		// (partial or no download).
		if !trustworthyLaunchTermination(lr.reason) {
			if lr.reason == db.TerminationReasonDiskFull {
				report.DiskFullCount++
			} else {
				report.ExcludedUntrusted++
			}
			continue
		}

		group, unionInputs := loadLaunchGroup(database, lr.jobIDs)
		if len(group.Jobs) == 0 {
			continue
		}

		// Flag launches carrying structurally invalid HF model refs (e.g.
		// "hf:gpt2 9") so the data-quality noise is attributed, not silent.
		for _, ref := range unionInputs {
			if a, ok := dataloc.ParseAssetRef(ref); ok && a.Kind == dataloc.AssetHFModel && !dataloc.IsHFModelID(a.ID) {
				report.MalformedRefLaunches++
				break
			}
		}

		// Pure HF-API byte sum (the number HFCacheMultiplier multiplies). nil DB
		// keeps the denominator the genuine API prediction, not a host_data
		// readback that could echo the on-disk size and force the ratio to ~1.
		unionRawHF, unionUnresolved, _ := dataloc.ResolveInputSizes(unionInputs, nil)
		estimateGB, _ := campaign.EstimateGroupDisk(group, database, nil)

		row := diskCalibRow{
			LaunchID:         lr.launchID,
			Jobs:             lr.jobIDs,
			Project:          lr.project,
			Reason:           lr.reason,
			UnionRawHFBytes:  unionRawHF,
			HFCachePostBytes: lr.hfCachePost,
			EstimateGB:       estimateGB,
		}

		// HF multiplier residual: actual instance cache / union API byte sum.
		// Require every declared ref to resolve — an incomplete denominator
		// (malformed refs, datasets mis-declared as models, gated models) makes
		// the ratio meaningless.
		switch {
		case lr.hfCachePost > bound:
			report.SkippedAnomalyHF++
		case len(unionInputs) > 0 && len(unionUnresolved) > 0:
			report.SkippedUnresolvedHF++
		case unionRawHF > 0 && lr.hfCachePost > 0:
			row.HFRatio = float64(lr.hfCachePost) / float64(unionRawHF)
			hfRatios = append(hfRatios, row.HFRatio)
		case unionRawHF == 0 && lr.hfCachePost > 0:
			report.SkippedNoUnionHF++
		}

		// Whole-estimate residual: actual peak disk / recomputed estimate.
		// Reject each peak component above the plausibility bound (statfs
		// Bsize-vs-Frsize bug) rather than the whole launch.
		peak := plausiblePeak(lr.summaryPeak, lr.diskUsed, bound)
		if peak == 0 && (lr.summaryPeak > bound || lr.diskUsed > bound) {
			report.SkippedAnomalyPeak++
		}
		row.PeakDiskBytes = peak
		if peak > 0 && estimateGB > 0 {
			row.WholeRatio = float64(peak) / (float64(estimateGB) * 1e9)
			wholeRatios = append(wholeRatios, row.WholeRatio)
			if row.WholeRatio > 1.0 {
				report.WholeUndersizedCount++
				report.WholeUndersizedJobs = append(report.WholeUndersizedJobs, lr.launchID)
			}
		}

		report.Rows = append(report.Rows, row)
	}

	report.HFMultiplier = summarizeResiduals(hfRatios)
	report.WholeEstimate = summarizeResiduals(wholeRatios)
	if s := report.HFMultiplier; s.N > 0 {
		sort.Float64s(hfRatios)
		// A trustworthy structural multiplier sits in a tight band a little
		// above 1. A distribution that dips well below 1 or runs far above it is
		// confounded by input-declaration quality (declared models diverging
		// from what was actually cached), not a real cache-overhead signal — so
		// we do not emit a recommendation from it.
		report.HFRecommendationReliable = s.Min >= 0.8 && s.Max <= 2.5
		if report.HFRecommendationReliable {
			report.RecommendedHFMult = quantile(hfRatios, diskCalibCoverage)
		}
	}

	if diskCalibJSON {
		return writeJSON(cmd.OutOrStdout(), report)
	}
	printDiskCalibration(cmd.OutOrStdout(), report)
	return nil
}

// loadDiskCalibLaunches returns every cloud launch that has disk telemetry,
// aggregated over the jobs that ran on it. Telemetry is keyed per job
// (job_phase_timings is the job's last run), so the per-launch values are the
// max across the launch's jobs — a close approximation when jobs share an
// instance's cache.
func loadDiskCalibLaunches(database *sql.DB, project string) ([]launchRow, error) {
	rows, err := database.Query(
		`SELECT l.id,
		        COALESCE(l.termination_reason, ''),
		        COALESCE(MIN(j.project), ''),
		        GROUP_CONCAT(DISTINCT ja.job_id),
		        COALESCE(MAX(jpt.cache_hf_post_bytes), 0),
		        COALESCE(MAX(jpt.disk_used_bytes), 0),
		        COALESCE(MAX(s.peak), 0)
		   FROM launches l
		   JOIN job_attempts ja ON ja.launch_id = l.id
		   JOIN jobs j ON j.id = ja.job_id
		   LEFT JOIN job_phase_timings jpt ON jpt.job_id = ja.job_id
		   LEFT JOIN (
		     SELECT job_id, MAX(peak_disk_used_bytes) AS peak
		       FROM job_timeseries_summaries
		      GROUP BY job_id
		   ) s ON s.job_id = ja.job_id
		  WHERE (? = '' OR j.project = ?)
		  GROUP BY l.id
		 HAVING MAX(COALESCE(jpt.cache_hf_post_bytes, 0)) > 0
		     OR MAX(COALESCE(jpt.disk_used_bytes, 0)) > 0
		     OR MAX(COALESCE(s.peak, 0)) > 0
		  ORDER BY l.id DESC`,
		project, project,
	)
	if err != nil {
		return nil, fmt.Errorf("query disk-calibration launches: %w", err)
	}
	defer rows.Close()

	var out []launchRow
	for rows.Next() {
		var lr launchRow
		var jobIDs string
		if err := rows.Scan(&lr.launchID, &lr.reason, &lr.project, &jobIDs, &lr.hfCachePost, &lr.diskUsed, &lr.summaryPeak); err != nil {
			return nil, err
		}
		lr.jobIDs = parseInt64CSV(jobIDs)
		out = append(out, lr)
	}
	return out, rows.Err()
}

// loadLaunchGroup loads the jobs that ran on a launch into a single
// InstanceGroup and returns the deduplicated union of their declared and
// observed input refs.
func loadLaunchGroup(database *sql.DB, jobIDs []int64) (campaign.InstanceGroup, []string) {
	group := campaign.InstanceGroup{}
	seen := map[string]bool{}
	var union []string
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			union = append(union, ref)
		}
	}
	for _, id := range jobIDs {
		job, err := db.GetJobByID(database, id)
		if err != nil || job == nil {
			continue
		}
		group.Jobs = append(group.Jobs, job)
		for _, ref := range job.Inputs {
			add(ref)
		}
		for _, ref := range job.ObservedInputs {
			add(ref)
		}
	}
	return group, union
}

// trustworthyLaunchTermination reports whether a launch ran its workload to a
// point where its disk telemetry reflects the real footprint. Excludes empty
// (non-terminal), disk_full (truncated/capped), and infrastructure failures.
func trustworthyLaunchTermination(reason string) bool {
	r := strings.TrimSpace(reason)
	if r == "" || r == db.TerminationReasonDiskFull {
		return false
	}
	return db.IsNormalInstanceTermination(r)
}

// plausiblePeak returns the larger of the two peak components that is within the
// plausibility bound, rejecting statfs-bug poisoned samples per component.
func plausiblePeak(summaryPeak, diskUsed, bound int64) int64 {
	var peak int64
	if summaryPeak > 0 && summaryPeak <= bound {
		peak = max(peak, summaryPeak)
	}
	if diskUsed > 0 && diskUsed <= bound {
		peak = max(peak, diskUsed)
	}
	return peak
}

func parseInt64CSV(s string) []int64 {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func summarizeResiduals(ratios []float64) *residualSummary {
	if len(ratios) == 0 {
		return &residualSummary{}
	}
	sorted := append([]float64{}, ratios...)
	sort.Float64s(sorted)
	var sum float64
	for _, r := range sorted {
		sum += r
	}
	return &residualSummary{
		N:    len(sorted),
		Min:  sorted[0],
		P50:  quantile(sorted, 0.50),
		P90:  quantile(sorted, 0.90),
		P95:  quantile(sorted, 0.95),
		P99:  quantile(sorted, 0.99),
		Max:  sorted[len(sorted)-1],
		Mean: sum / float64(len(sorted)),
	}
}

// quantile returns the nearest-rank upper quantile of a sorted slice. Nearest
// rank (not interpolation) is deliberate: for a coverage/safety target we want
// the smallest observed value that covers the requested fraction.
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	idx := int(math.Ceil(q*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

func printDiskCalibration(w io.Writer, r diskCalibrationReport) {
	fmt.Fprintf(w, "Disk-size calibration (per-launch, recomputed with current algorithm vs actual usage)\n")
	fmt.Fprintf(w, "Launches with telemetry: %d   coverage target: %.0f%%\n", r.LaunchesTotal, r.Coverage*100)
	fmt.Fprintf(w, "Excluded: %d untrustworthy termination, %d disk_full\n", r.ExcludedUntrusted, r.DiskFullCount)
	if r.MalformedRefLaunches > 0 {
		fmt.Fprintf(w, "Data quality: %d launches carried structurally invalid HF refs (kept out of the byte sums)\n", r.MalformedRefLaunches)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "HF cache multiplier  (actual hf_cache_post / recomputed union raw HF bytes):")
	printResidualSummary(w, r.HFMultiplier)
	fmt.Fprintf(w, "  current HFCacheMultiplier: %.2f\n", r.CurrentHFMultiplier)
	if r.HFMultiplier != nil && r.HFMultiplier.N > 0 {
		if r.HFRecommendationReliable {
			fmt.Fprintf(w, "  recommended (p%.0f):        %.2f\n", r.Coverage*100, r.RecommendedHFMult)
		} else {
			fmt.Fprintln(w, "  recommendation withheld — distribution confounded by input-declaration quality")
			fmt.Fprintln(w, "    (declared models diverge from cached content: over-declaration pulls ratios below 1,")
			fmt.Fprintln(w, "     undeclared auto-downloads such as tokenizers/base models push them above 1)")
		}
	}
	if r.SkippedUnresolvedHF > 0 {
		fmt.Fprintf(w, "  (%d launches skipped — some declared HF refs did not resolve; denominator incomplete)\n", r.SkippedUnresolvedHF)
	}
	if r.SkippedNoUnionHF > 0 {
		fmt.Fprintf(w, "  (%d launches had an HF cache but no resolvable declared HF input)\n", r.SkippedNoUnionHF)
	}
	if r.SkippedAnomalyHF > 0 {
		fmt.Fprintf(w, "  (%d launches skipped — HF cache above the 2TB plausibility bound)\n", r.SkippedAnomalyHF)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Whole estimate  (actual peak disk / recomputed full estimate):")
	printResidualSummary(w, r.WholeEstimate)
	fmt.Fprintf(w, "  current EmpiricalDiskSafetyMultiplier: %.2f\n", r.CurrentEmpiricalMult)
	if r.WholeEstimate != nil && r.WholeEstimate.N > 0 {
		fmt.Fprintf(w, "  undersized (actual > estimate):        %d of %d\n", r.WholeUndersizedCount, r.WholeEstimate.N)
	}
	if r.SkippedAnomalyPeak > 0 {
		fmt.Fprintf(w, "  (%d launches skipped — peak above the 2TB plausibility bound)\n", r.SkippedAnomalyPeak)
	}
	fmt.Fprintln(w)

	if len(r.WholeUndersizedJobs) > 0 {
		ids := make([]string, len(r.WholeUndersizedJobs))
		for i, id := range r.WholeUndersizedJobs {
			ids[i] = fmt.Sprintf("L%d", id)
		}
		fmt.Fprintf(w, "Undersized launches (actual peak exceeded estimate): %s\n\n", strings.Join(ids, ", "))
	}

	if diskCalibRows {
		printDiskCalibRows(w, r.Rows)
	}
}

func printResidualSummary(w io.Writer, s *residualSummary) {
	if s == nil || s.N == 0 {
		fmt.Fprintln(w, "  (no data)")
		return
	}
	fmt.Fprintf(w, "  n=%d  min=%.2f  p50=%.2f  p90=%.2f  p95=%.2f  p99=%.2f  max=%.2f  mean=%.2f\n",
		s.N, s.Min, s.P50, s.P90, s.P95, s.P99, s.Max, s.Mean)
}

func printDiskCalibRows(w io.Writer, rows []diskCalibRow) {
	if len(rows) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "LAUNCH\tPROJECT\tJOBS\tUNION_HF\tHF_CACHE\tHF_RATIO\tEST_GB\tPEAK\tWHOLE_RATIO\tREASON")
	for _, row := range rows {
		hfRatio := "-"
		if row.HFRatio > 0 {
			hfRatio = fmt.Sprintf("%.2f", row.HFRatio)
		}
		wholeRatio := "-"
		if row.WholeRatio > 0 {
			wholeRatio = fmt.Sprintf("%.2f", row.WholeRatio)
		}
		fmt.Fprintf(tw, "L%d\t%s\t%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			row.LaunchID, row.Project, len(row.Jobs),
			humanBytesDecimal(row.UnionRawHFBytes), humanBytesDecimal(row.HFCachePostBytes), hfRatio,
			row.EstimateGB, humanBytesDecimal(row.PeakDiskBytes), wholeRatio, row.Reason)
	}
	tw.Flush()
}
