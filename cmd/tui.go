package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/tui"
	"github.com/spf13/cobra"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Interactive terminal UI for managing jobs",
	Long: `Launch an interactive terminal UI for viewing and managing remote jobs.

The TUI shows a split-screen view with:
  - Top panel: Job list with status indicators
  - Bottom panel: Log output for selected job

Keyboard shortcuts:
  Up/Down    Navigate job list
  Enter      Select job / view logs
  Escape     Clear selection
  r          Restart highlighted job
  k/Delete   Kill highlighted job
  p          Prune completed/dead jobs
  Ctrl-C/q   Quit
  Ctrl-Z     Suspend (resume with 'fg')`,
	RunE: runTUI,
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}

func runTUI(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	model := tui.NewModel(database)

	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
	)

	_, err = p.Run()
	if err != nil {
		return fmt.Errorf("run TUI: %w", err)
	}

	return nil
}
