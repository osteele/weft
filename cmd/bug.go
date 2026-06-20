package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

var (
	bugReportTitle       string
	bugReportKind        string
	bugReportScope       string
	bugReportLikelihood  string
	bugReportSeverity    string
	bugReportFingerprint string
	bugReportJob         string
	bugReportHost        string
	bugReportSummary     string
	bugReportDetail      string
	bugReportNote        string
	bugNoteStdin         bool
	bugListAll           bool
	bugCloseReason       string
)

type bugTracker interface {
	Report(db.BugReport) error
	Note(id, note string) error
	List(all bool) error
	Show(id string) error
	Close(id, reason string) error
	Reopen(id string) error
}

var bugCmd = &cobra.Command{
	Use:   "bug",
	Short: "Report and inspect Weft bugs",
}

var bugReportCmd = &cobra.Command{
	Use:   "report [title]",
	Short: "Report a Weft bug and print its bug number",
	Args:  cobra.ArbitraryArgs,
	RunE:  runBugReport,
}

var bugNoteCmd = &cobra.Command{
	Use:   "note [--stdin] <bug-id> [text...]",
	Short: "Add context to an existing Weft bug",
	Args:  usageArgs(validateBugNoteArgs),
	RunE:  runBugNote,
}

var bugListCmd = &cobra.Command{
	Use:   "list",
	Short: "List outstanding Weft bugs",
	Args:  cobra.NoArgs,
	RunE:  runBugList,
}

var bugShowCmd = &cobra.Command{
	Use:   "show <bug-id>",
	Short: "Show a Weft bug",
	Args:  cobra.ExactArgs(1),
	RunE:  runBugShow,
}

var bugCloseCmd = &cobra.Command{
	Use:   "close <bug-id>",
	Short: "Close a Weft bug",
	Args:  cobra.ExactArgs(1),
	RunE:  runBugClose,
}

var bugReopenCmd = &cobra.Command{
	Use:   "reopen <bug-id>",
	Short: "Reopen a closed Weft bug",
	Args:  cobra.ExactArgs(1),
	RunE:  runBugReopen,
}

var bugTrackerCmd = &cobra.Command{
	Use:   "tracker [github|local]",
	Short: "Show or set the Weft bug tracker backend",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runBugTrackerConfig,
}

func init() {
	rootCmd.AddCommand(bugCmd)
	bugCmd.AddCommand(bugReportCmd)
	bugCmd.AddCommand(bugNoteCmd)
	bugCmd.AddCommand(bugListCmd)
	bugCmd.AddCommand(bugShowCmd)
	bugCmd.AddCommand(bugCloseCmd)
	bugCmd.AddCommand(bugReopenCmd)
	bugCmd.AddCommand(bugTrackerCmd)

	bugReportCmd.Flags().StringVar(&bugReportTitle, "title", "", "bug title")
	bugReportCmd.Flags().StringVar(&bugReportKind, "kind", "bug", "bug kind")
	bugReportCmd.Flags().StringVar(&bugReportScope, "scope", "infrastructure", "scope: job-specific, network, infrastructure, other")
	bugReportCmd.Flags().StringVar(&bugReportLikelihood, "likelihood", "unknown", "likelihood of recurrence")
	bugReportCmd.Flags().StringVar(&bugReportSeverity, "severity", "notice", "severity: internal, notice, warning, error")
	bugReportCmd.Flags().StringVar(&bugReportFingerprint, "fingerprint", "", "dedupe fingerprint")
	bugReportCmd.Flags().StringVar(&bugReportJob, "job", "", "related job id")
	bugReportCmd.Flags().StringVar(&bugReportHost, "host", "", "related host")
	bugReportCmd.Flags().StringVar(&bugReportSummary, "summary", "", "short user-facing summary")
	bugReportCmd.Flags().StringVar(&bugReportDetail, "detail", "", "maintainer detail")
	bugReportCmd.Flags().StringVar(&bugReportNote, "note", "", "initial note")

	bugNoteCmd.Flags().BoolVar(&bugNoteStdin, "stdin", false, "read note text from stdin")
	bugNoteCmd.Flags().SetInterspersed(false)

	bugListCmd.Flags().BoolVar(&bugListAll, "all", false, "include closed bugs")
	bugCloseCmd.Flags().StringVar(&bugCloseReason, "reason", "", "close reason")
}

func runBugReport(_ *cobra.Command, args []string) error {
	title := strings.TrimSpace(bugReportTitle)
	if title == "" {
		title = strings.TrimSpace(strings.Join(args, " "))
	}
	jobID, err := parseOptionalBugJobID(bugReportJob)
	if err != nil {
		return err
	}
	tracker, err := currentBugTracker()
	if err != nil {
		return err
	}
	return tracker.Report(db.BugReport{
		Title:       title,
		Kind:        bugReportKind,
		Scope:       bugReportScope,
		Likelihood:  bugReportLikelihood,
		Severity:    bugReportSeverity,
		Fingerprint: bugReportFingerprint,
		JobID:       jobID,
		Host:        bugReportHost,
		Summary:     bugReportSummary,
		Detail:      bugReportDetail,
		Note:        bugReportNote,
	})
}

func runBugNote(_ *cobra.Command, args []string) error {
	note := strings.TrimSpace(strings.Join(args[1:], " "))
	if bugNoteStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		note = strings.TrimRight(string(data), "\r\n")
	}
	tracker, err := currentBugTracker()
	if err != nil {
		return err
	}
	return tracker.Note(args[0], note)
}

func validateBugNoteArgs(_ *cobra.Command, args []string) error {
	if bugNoteStdin {
		if len(args) != 1 {
			return fmt.Errorf("--stdin expects exactly one bug id and no note text")
		}
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("bug id is required")
	}
	if len(args) == 1 {
		return fmt.Errorf("bug note text is required: pass text after the bug id, or pipe text with --stdin")
	}
	return nil
}

func runBugList(_ *cobra.Command, _ []string) error {
	tracker, err := currentBugTracker()
	if err != nil {
		return err
	}
	return tracker.List(bugListAll)
}

func runBugShow(_ *cobra.Command, args []string) error {
	tracker, err := currentBugTracker()
	if err != nil {
		return err
	}
	return tracker.Show(args[0])
}

func runBugClose(_ *cobra.Command, args []string) error {
	tracker, err := currentBugTracker()
	if err != nil {
		return err
	}
	return tracker.Close(args[0], bugCloseReason)
}

func runBugReopen(_ *cobra.Command, args []string) error {
	tracker, err := currentBugTracker()
	if err != nil {
		return err
	}
	return tracker.Reopen(args[0])
}

func runBugTrackerConfig(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		tracker, err := cfg.BugTracker()
		if err != nil {
			return err
		}
		fmt.Printf("Bug tracker: %s\n", tracker)
		if strings.TrimSpace(cfg.Bug.GitHubRepo) != "" {
			fmt.Printf("GitHub repo: %s\n", strings.TrimSpace(cfg.Bug.GitHubRepo))
		}
		return nil
	}
	if err := config.SetBugTrackerSetting(args[0]); err != nil {
		return err
	}
	fmt.Printf("Bug tracker set to %s in %s\n", strings.ToLower(strings.TrimSpace(args[0])), config.ConfigPath())
	return nil
}

func currentBugTracker() (bugTracker, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	tracker, err := cfg.BugTracker()
	if err != nil {
		return nil, err
	}
	switch tracker {
	case config.BugTrackerLocal:
		return localBugTracker{}, nil
	case config.BugTrackerGitHub:
		return newGitHubBugTracker(cfg.Bug.GitHubRepo), nil
	default:
		return nil, fmt.Errorf("unknown bug tracker %q", tracker)
	}
}

type localBugTracker struct{}

func (localBugTracker) Report(report db.BugReport) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()

	bug, created, err := db.ReportBug(database, report)
	if err != nil {
		return err
	}
	verb := "Reported"
	if !created {
		verb = "Updated"
	}
	fmt.Printf("%s %s: %s\n", verb, db.FormatBugID(bug.ID), bug.Title)
	return nil
}

func (localBugTracker) Note(idText, note string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(idText)
	if err != nil {
		return err
	}
	if err := db.AddBugNote(database, id, note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("bug %s not found", db.FormatBugID(id))
		}
		return err
	}
	fmt.Printf("Added note to %s\n", db.FormatBugID(id))
	return nil
}

func (localBugTracker) List(all bool) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	bugs, err := db.ListBugs(database, all)
	if err != nil {
		return err
	}
	if len(bugs) == 0 {
		fmt.Println("No bugs.")
		return nil
	}
	for _, bug := range bugs {
		fmt.Printf("%-6s %-7s %-14s %-10s %s\n", db.FormatBugID(bug.ID), bug.Status, bug.Scope, ageString(bug.UpdatedAt), bug.Title)
	}
	return nil
}

func (localBugTracker) Show(idText string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(idText)
	if err != nil {
		return err
	}
	bug, err := db.GetBug(database, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("bug %s not found", db.FormatBugID(id))
		}
		return err
	}
	printBug(bug)
	notes, err := db.ListBugNotes(database, id)
	if err != nil {
		return err
	}
	if len(notes) > 0 {
		fmt.Println()
		fmt.Println("Notes:")
		for _, note := range notes {
			fmt.Printf("- %s %s\n", formatBugUnixTime(note.CreatedAt), note.Body)
		}
	}
	return nil
}

func (localBugTracker) Close(idText, reason string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(idText)
	if err != nil {
		return err
	}
	if err := db.CloseBug(database, id, reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("open bug %s not found", db.FormatBugID(id))
		}
		return err
	}
	fmt.Printf("Closed %s\n", db.FormatBugID(id))
	return nil
}

func (localBugTracker) Reopen(idText string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(idText)
	if err != nil {
		return err
	}
	if err := db.ReopenBug(database, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("closed bug %s not found", db.FormatBugID(id))
		}
		return err
	}
	fmt.Printf("Reopened %s\n", db.FormatBugID(id))
	return nil
}

func parseOptionalBugJobID(raw string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := ids.ParseJobID(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func printBug(bug *db.Bug) {
	fmt.Printf("Bug ID:      %s\n", db.FormatBugID(bug.ID))
	fmt.Printf("Status:      %s\n", bug.Status)
	fmt.Printf("Title:       %s\n", bug.Title)
	fmt.Printf("Kind:        %s\n", bug.Kind)
	fmt.Printf("Scope:       %s\n", bug.Scope)
	fmt.Printf("Likelihood:  %s\n", bug.Likelihood)
	fmt.Printf("Severity:    %s\n", bug.Severity)
	fmt.Printf("Fingerprint: %s\n", bug.Fingerprint)
	if bug.JobID != nil {
		fmt.Printf("Job:         %s\n", ids.FormatJobID(*bug.JobID))
	}
	if bug.Host != "" {
		fmt.Printf("Host:        %s\n", bug.Host)
	}
	fmt.Printf("Occurrences: %d\n", bug.Occurrences)
	fmt.Printf("Created:     %s\n", formatBugUnixTime(bug.CreatedAt))
	fmt.Printf("Updated:     %s\n", formatBugUnixTime(bug.UpdatedAt))
	if bug.ClosedAt != nil {
		fmt.Printf("Closed:      %s\n", formatBugUnixTime(*bug.ClosedAt))
	}
	if bug.CloseReason != "" {
		fmt.Printf("Close reason: %s\n", bug.CloseReason)
	}
	if bug.Summary != "" {
		fmt.Printf("Summary:     %s\n", bug.Summary)
	}
	if bug.Detail != "" {
		fmt.Println("Detail:")
		fmt.Println(bug.Detail)
	}
}

func formatBugUnixTime(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func ageString(ts int64) string {
	if ts <= 0 {
		return ""
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
