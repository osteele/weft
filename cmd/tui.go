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
  r          Restart selected job
  k/Delete   Kill selected job
  p          Prune completed/dead jobs
  q          Quit`,
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

	model := tui.NewModel(database)

	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
	)

	finalModel, err := p.Run()
	if err != nil {
		database.Close()
		return fmt.Errorf("run TUI: %w", err)
	}

	// Close database after TUI exits
	if m, ok := finalModel.(tui.Model); ok {
		m.Close()
	} else {
		database.Close()
	}

	return nil
}
