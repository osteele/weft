package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

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

var sessionUnprocessedProject string

func init() {
	rootCmd.AddCommand(sessionCmd)
	sessionCmd.AddCommand(sessionUnprocessedCmd)
	sessionUnprocessedCmd.Flags().StringVar(&sessionUnprocessedProject, "project", "", "Limit the inbox to one exact project")
}

func runSessionUnprocessed(cmd *cobra.Command, _ []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	query, err := loadCurrentSessionUnprocessed(database, sessionUnprocessedProject, time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(query)
}

func loadCurrentSessionUnprocessed(database *sql.DB, project string, now time.Time) (sessioninbox.Query, error) {
	return sessioninbox.Load(database, project, submitterSession(), now)
}

// writeSessionUnprocessedReminder appends the session reminder to a human
// surface. The reminder is advisory, so a failed inbox read is reported to the
// log and leaves the command's own exit status alone.
func writeSessionUnprocessedReminder(database *sql.DB, w io.Writer) {
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
