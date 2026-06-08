package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
	bugListAll           bool
	bugCloseReason       string
)

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
	Use:   "note <bug-id> <text>",
	Short: "Add context to an existing Weft bug",
	Args:  cobra.MinimumNArgs(2),
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

func init() {
	rootCmd.AddCommand(bugCmd)
	bugCmd.AddCommand(bugReportCmd)
	bugCmd.AddCommand(bugNoteCmd)
	bugCmd.AddCommand(bugListCmd)
	bugCmd.AddCommand(bugShowCmd)
	bugCmd.AddCommand(bugCloseCmd)
	bugCmd.AddCommand(bugReopenCmd)

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

	bugListCmd.Flags().BoolVar(&bugListAll, "all", false, "include closed bugs")
	bugCloseCmd.Flags().StringVar(&bugCloseReason, "reason", "", "close reason")
}

func runBugReport(_ *cobra.Command, args []string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()

	title := strings.TrimSpace(bugReportTitle)
	if title == "" {
		title = strings.TrimSpace(strings.Join(args, " "))
	}
	jobID, err := parseOptionalBugJobID(bugReportJob)
	if err != nil {
		return err
	}
	bug, created, err := db.ReportBug(database, db.BugReport{
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

func runBugNote(_ *cobra.Command, args []string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(args[0])
	if err != nil {
		return err
	}
	note := strings.TrimSpace(strings.Join(args[1:], " "))
	if err := db.AddBugNote(database, id, note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("bug %s not found", db.FormatBugID(id))
		}
		return err
	}
	fmt.Printf("Added note to %s\n", db.FormatBugID(id))
	return nil
}

func runBugList(_ *cobra.Command, _ []string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	bugs, err := db.ListBugs(database, bugListAll)
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

func runBugShow(_ *cobra.Command, args []string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(args[0])
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

func runBugClose(_ *cobra.Command, args []string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(args[0])
	if err != nil {
		return err
	}
	if err := db.CloseBug(database, id, bugCloseReason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("open bug %s not found", db.FormatBugID(id))
		}
		return err
	}
	fmt.Printf("Closed %s\n", db.FormatBugID(id))
	return nil
}

func runBugReopen(_ *cobra.Command, args []string) error {
	database, err := db.OpenBugDB()
	if err != nil {
		return err
	}
	defer database.Close()
	id, err := db.ParseBugID(args[0])
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
