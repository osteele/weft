package cmd

import (
	"database/sql"
	"fmt"
	"io"

	"github.com/osteele/weft/internal/campaign"
)

// telemetryAnomaly is the per-row shape surfaced by `weft job anomalies` for
// historical disk telemetry samples that exceeded the plausibility bound.
// The fields are deliberately a superset of campaign.DiskTelemetryAnomaly so
// the same record can be lifted into a future telemetry_anomalies table with
// only a SQL change (no schema migration in the consumers).
type telemetryAnomaly struct {
	JobID         int64  `json:"job_id"`
	JobExtID      string `json:"job"`
	Project       string `json:"project,omitempty"`
	Command       string `json:"command,omitempty"`
	Source        string `json:"source"`
	ObservedBytes int64  `json:"observed_bytes"`
	BoundBytes    int64  `json:"bound_bytes"`
}

// scanDiskTelemetryAnomalies walks the same source tables the disk estimator
// reads (job_phase_timings.disk_used_bytes and job_timeseries.peak_used_bytes,
// where peak = MAX(disk_total - disk_free)) and returns every row whose
// reported value exceeds DiskTelemetryPlausibilityBytes.
//
// This intentionally re-derives anomalies on the fly rather than reading from
// a materialized table, so:
//   - older bogus telemetry (written before the estimator detection landed)
//     is still surfaced;
//   - if the plausibility bound is tuned later, results recompute without a
//     migration;
//   - fixing the bug at the source (probe, agent) makes anomalies disappear
//     naturally as old jobs roll off.
func scanDiskTelemetryAnomalies(database *sql.DB) ([]telemetryAnomaly, error) {
	if database == nil {
		return nil, nil
	}
	bound := campaign.DiskTelemetryPlausibilityBytes
	var out []telemetryAnomaly

	phaseRows, err := database.Query(
		`SELECT j.id, COALESCE(j.project, ''), COALESCE(j.command, ''), jpt.disk_used_bytes
		   FROM job_phase_timings jpt
		   JOIN jobs j ON j.id = jpt.job_id
		  WHERE jpt.disk_used_bytes > ?
		  ORDER BY jpt.disk_used_bytes DESC`,
		bound,
	)
	if err != nil {
		return nil, fmt.Errorf("scan phase_timings disk anomalies: %w", err)
	}
	for phaseRows.Next() {
		var a telemetryAnomaly
		if err := phaseRows.Scan(&a.JobID, &a.Project, &a.Command, &a.ObservedBytes); err != nil {
			phaseRows.Close()
			return nil, err
		}
		a.JobExtID = fmt.Sprintf("wj%d", a.JobID)
		a.Source = "phase_timings"
		a.BoundBytes = bound
		out = append(out, a)
	}
	phaseRows.Close()
	if err := phaseRows.Err(); err != nil {
		return nil, err
	}

	tsRows, err := database.Query(
		`SELECT j.id, COALESCE(j.project, ''), COALESCE(j.command, ''),
		        MAX(ts.peak_used_bytes) AS peak_used_bytes
		   FROM (
		     SELECT job_id, peak_disk_used_bytes AS peak_used_bytes
		       FROM job_timeseries_summaries
		     UNION ALL
		     SELECT job_id,
		            CASE
		              WHEN disk_total_bytes > 0
		               AND disk_free_bytes >= 0
		               AND disk_total_bytes >= disk_free_bytes
		              THEN disk_total_bytes - disk_free_bytes
		              ELSE 0
		            END AS peak_used_bytes
		       FROM job_timeseries
		      WHERE job_id NOT IN (SELECT job_id FROM job_timeseries_summaries)
		   ) ts
		   JOIN jobs j ON j.id = ts.job_id
		  GROUP BY j.id
		 HAVING peak_used_bytes > ?
		  ORDER BY peak_used_bytes DESC`,
		bound,
	)
	if err != nil {
		return nil, fmt.Errorf("scan timeseries disk anomalies: %w", err)
	}
	for tsRows.Next() {
		var a telemetryAnomaly
		if err := tsRows.Scan(&a.JobID, &a.Project, &a.Command, &a.ObservedBytes); err != nil {
			tsRows.Close()
			return nil, err
		}
		a.JobExtID = fmt.Sprintf("wj%d", a.JobID)
		a.Source = "timeseries"
		a.BoundBytes = bound
		out = append(out, a)
	}
	tsRows.Close()
	if err := tsRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func printTelemetryAnomalies(w io.Writer, anomalies []telemetryAnomaly) {
	if len(anomalies) == 0 {
		return
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Disk telemetry anomalies (samples exceeding plausibility bound):")
	fmt.Fprintln(w, "  Likely cause: statfs Bsize-vs-Frsize on overlay/fuse filesystems.")
	fmt.Fprintln(w, "  Source tables: job_phase_timings.disk_used_bytes, job_timeseries.peak_used_bytes.")
	for _, a := range anomalies {
		fmt.Fprintf(w, "  %s  source=%s  observed=%s  bound=%s",
			a.JobExtID, a.Source, humanBytesDecimal(a.ObservedBytes), humanBytesDecimal(a.BoundBytes))
		if a.Project != "" {
			fmt.Fprintf(w, "  project=%s", a.Project)
		}
		fmt.Fprintln(w)
		if a.Command != "" {
			fmt.Fprintf(w, "    command: %s\n", a.Command)
		}
	}
}

// humanBytesDecimal mirrors campaign.humanBytes so the CLI output style stays
// consistent with placement_reasons. Decimal (1000-based) units intentionally.
func humanBytesDecimal(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n) / 1000.0
	idx := 0
	for v >= 1000 && idx < len(units)-1 {
		v /= 1000.0
		idx++
	}
	return fmt.Sprintf("%.1f%s", v, units[idx])
}
