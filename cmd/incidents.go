package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	incidentsJSON bool
)

var incidentsCmd = &cobra.Command{
	Use:   "incidents",
	Short: "List active placement incidents (errors affecting ≥2 jobs)",
	Long: `Group currently-unplaced jobs by their upstream error fingerprint and
print one row per distinct incident. An "incident" is a classified upstream
failure (e.g. Vast.ai API 400, account credit exhaustion, R2 timeout) that
the autopilot is seeing across multiple jobs in the queue.

Use this to spot systemic outages at a glance: a single failing vastai
filter clause, an exhausted account, or a regional provider degradation
shows up as ONE row affecting N jobs instead of N look-alike per-job
failures.

Examples:
  weft incidents
  weft incidents --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		database, err := db.OpenForReading()
		if err != nil {
			return err
		}
		defer database.Close()

		incs, err := collectIncidents(database)
		if err != nil {
			return err
		}
		if incidentsJSON {
			return writeIncidentsJSON(os.Stdout, incs)
		}
		writeIncidentsTable(os.Stdout, incs)
		return nil
	},
}

// IncidentSummary captures one distinct upstream error class across multiple
// unplaced jobs. Exported for `--json` consumers and for the autopilot status
// surface that also reports active incidents.
type IncidentSummary struct {
	Fingerprint string  `json:"fingerprint"`
	Count       int     `json:"count"`
	SampleJobID int64   `json:"sample_job_id"`
	Message     string  `json:"message,omitempty"`
	JobIDs      []int64 `json:"job_ids"`
}

// collectIncidents scans the placement_blocked column on currently-unplaced
// jobs and groups them by Structured.Fingerprint. Incidents are surfaced
// only when ≥2 jobs share a fingerprint — a single blocked job is a
// job-specific failure, not a systemic incident.
//
// Cost: one SQL scan over unplaced jobs (sub-millisecond against local
// SQLite per CLAUDE.md storage-latency notes); per-job JSON parse is cheap
// because Structured is a tiny object.
func collectIncidents(database *sql.DB) ([]IncidentSummary, error) {
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}
	type bucket struct {
		jobIDs      []int64
		message     string
		sampleJobID int64
	}
	by := make(map[string]*bucket)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		s := blockreason.Parse(job.PlacementBlockedJSON)
		if s == nil {
			continue
		}
		fp := strings.TrimSpace(s.Fingerprint)
		if fp == "" {
			continue
		}
		b, ok := by[fp]
		if !ok {
			b = &bucket{}
			by[fp] = b
		}
		b.jobIDs = append(b.jobIDs, job.ID)
		msg := strings.TrimSpace(s.LaunchDetail)
		if msg == "" {
			msg = strings.TrimSpace(s.Launch)
		}
		if msg == "" {
			msg = strings.TrimSpace(s.Summary)
		}
		// Only adopt this job as the sample when it actually carries a
		// non-empty message — otherwise a lower-ID job with neither
		// LaunchDetail nor Launch nor Summary would wipe a meaningful
		// sample picked up earlier.
		if msg != "" && (b.message == "" || job.ID < b.sampleJobID) {
			b.sampleJobID = job.ID
			b.message = msg
		}
	}
	out := make([]IncidentSummary, 0, len(by))
	for fp, b := range by {
		if len(b.jobIDs) < 2 {
			continue
		}
		sort.Slice(b.jobIDs, func(i, j int) bool { return b.jobIDs[i] < b.jobIDs[j] })
		out = append(out, IncidentSummary{
			Fingerprint: fp,
			Count:       len(b.jobIDs),
			SampleJobID: b.sampleJobID,
			Message:     b.message,
			JobIDs:      b.jobIDs,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out, nil
}

func writeIncidentsJSON(w *os.File, incs []IncidentSummary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Incidents []IncidentSummary `json:"incidents"`
	}{Incidents: incs})
}

func writeIncidentsTable(w *os.File, incs []IncidentSummary) {
	if len(incs) == 0 {
		fmt.Fprintln(w, "No active placement incidents.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COUNT\tFINGERPRINT\tMESSAGE")
	for _, inc := range incs {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", inc.Count, inc.Fingerprint, truncateForTable(inc.Message, 120))
	}
	_ = tw.Flush()
	for _, inc := range incs {
		if hint := incidentHint(inc.Fingerprint); hint != "" {
			fmt.Fprintf(w, "\n  %s\n  → %s\n", inc.Fingerprint, hint)
		}
	}
}

// incidentHint returns an actionable suggestion for a known fingerprint
// class, or "" when the fingerprint carries no specific guidance.
func incidentHint(fingerprint string) string {
	switch {
	case strings.HasSuffix(fingerprint, "/account-credit-exhausted"):
		return "Top up at https://vast.ai/billing/ or switch to a different provider."
	case strings.Contains(fingerprint, "/400/bad-field:"):
		return "The vastai search filter contains a field the API no longer accepts. " +
			"Check internal/vastai/client.go buildSearchFilter against the current `vastai search offers --help`."
	case strings.HasSuffix(fingerprint, "/cli/timeout"):
		return "vastai CLI timed out. Check network/VPN, or set [vastai] cli_timeout higher in config."
	}
	return ""
}

func truncateForTable(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func init() {
	incidentsCmd.Flags().BoolVar(&incidentsJSON, "json", false, "Emit machine-readable JSON instead of a table")
	rootCmd.AddCommand(incidentsCmd)
}
