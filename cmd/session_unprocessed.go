package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/sessioninbox"
	"github.com/spf13/cobra"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Query state attributed to the current agent session",
}

var sessionUnprocessedCmd = &cobra.Command{
	Use:   "unprocessed",
	Short: "Print the current session's unprocessed terminal-job inbox as JSON",
	Args:  cobra.NoArgs,
	RunE:  runSessionUnprocessed,
}

var (
	sessionUnprocessedProject string
	sessionUnprocessedGroupBy string
)

func init() {
	rootCmd.AddCommand(sessionCmd)
	sessionCmd.AddCommand(sessionUnprocessedCmd)
	sessionUnprocessedCmd.Flags().StringVar(&sessionUnprocessedProject, "project", "", "Limit the inbox to one exact project")
	sessionUnprocessedCmd.Flags().StringVar(&sessionUnprocessedGroupBy, "group-by", "", `Group all sessions by "project,session"`)
}

func runSessionUnprocessed(cmd *cobra.Command, _ []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if sessionUnprocessedGroupBy != "" {
		if sessionUnprocessedGroupBy != "project,session" {
			return fmt.Errorf(`unsupported --group-by %q (use "project,session")`, sessionUnprocessedGroupBy)
		}
		if sessionUnprocessedProject != "" {
			return fmt.Errorf("--project cannot be combined with --group-by project,session")
		}
		query, err := sessioninbox.LoadGroups(database)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(query)
	}

	query, err := loadCurrentSessionUnprocessed(database, sessionUnprocessedProject, time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(query)
}

func loadCurrentSessionUnprocessed(database *sql.DB, project string, now time.Time) (sessioninbox.Query, error) {
	return sessioninbox.Load(database, project, submitterSession(), now)
}

// writeSessionUnprocessedReminder appends the session reminder only to a
// demonstrably interactive destination. See QuerySessionUnprocessedInbox in
// specs/job-lifecycle.allium. A failed inbox read leaves the command status alone.
func writeSessionUnprocessedReminder(database *sql.DB, w io.Writer) {
	fdWriter, ok := w.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(fdWriter.Fd()) {
		return
	}
	query, err := loadCurrentSessionUnprocessed(database, "", time.Now())
	if err != nil {
		slog.Warn("session unprocessed inbox lookup failed", "component", "cmd", "error", err)
		return
	}
	if reminder := sessioninbox.FormatReminder(query); reminder != "" {
		fmt.Fprintln(w, reminder)
	}
}

// writeSessionUnprocessedReminderFor opens its own read handle, for surfaces
// that have already closed the one they ran with.
func writeSessionUnprocessedReminderFor(w io.Writer) {
	database, err := db.OpenForReading()
	if err != nil {
		slog.Warn("session reminder database open failed", "component", "cmd", "error", err)
		return
	}
	defer database.Close()
	writeSessionUnprocessedReminder(database, w)
}
