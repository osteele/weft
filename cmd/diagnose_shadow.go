package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

var (
	diagnoseShadowSince string
	diagnoseShadowJSON  bool
)

var diagnoseShadowCmd = &cobra.Command{
	Use:   "shadow",
	Short: "List recorded TypeSafe Jev second opinions on failure diagnoses",
	Long: `List the diagnosis shadow's recorded judgments: for each judged failed job,
weft's display, remediation and runtime-fatal regex patterns next to TypeSafe
Jev's category, its confidence, and whether Jev read the log as supporting the
regex diagnoses (P(yes); "-" when there was no pattern to ask about).

The shadow is a measurement pilot enabled by [diagnosis_shadow] in
config.toml; nothing acts on these rows.

Examples:
  weft diagnose shadow
  weft diagnose shadow --since 30d --json`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runDiagnoseShadow,
}

func init() {
	diagnoseCmd.AddCommand(diagnoseShadowCmd)
	diagnoseShadowCmd.Flags().StringVar(&diagnoseShadowSince, "since", "7d", "Show judgments made since cutoff (YYYY-MM-DD, RFC3339, or duration like \"24h\"/\"7d\")")
	diagnoseShadowCmd.Flags().BoolVar(&diagnoseShadowJSON, "json", false, "Emit machine-readable JSON")
}

func runDiagnoseShadow(cmd *cobra.Command, args []string) error {
	cutoff, err := parseSinceCutoff(diagnoseShadowSince, time.Now())
	if err != nil {
		return usageErrorf("%s", err)
	}
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	rows, err := db.ListRecentDiagnosisShadows(database, cutoff)
	if err != nil {
		return err
	}
	if diagnoseShadowJSON {
		if rows == nil {
			rows = []db.DiagnosisShadow{}
		}
		return writeJSON(cmd.OutOrStdout(), rows)
	}
	return printDiagnosisShadows(cmd.OutOrStdout(), rows)
}

func printDiagnosisShadows(w io.Writer, rows []db.DiagnosisShadow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No diagnosis shadow judgments in this window.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tJUDGED\tDISPLAY\tREMEDIATION\tFATAL\tJEV\tCONF\tREGEX_OK\tFATAL_OK\tMODEL")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%s\t%s\t%s\n",
			ids.FormatJobID(row.JobID),
			util.FormatCLITime(time.Unix(row.JudgedAt, 0), "2006-01-02 15:04"),
			dashIfEmpty(row.DisplayPattern),
			dashIfEmpty(row.RemediationPattern),
			dashIfEmpty(row.FatalPattern),
			dashIfEmpty(row.JevCategory),
			row.JevConfidence,
			formatOptionalNoul(row.RegexSupported),
			formatOptionalNoul(row.FatalSupported),
			dashIfEmpty(row.Model))
	}
	return tw.Flush()
}
