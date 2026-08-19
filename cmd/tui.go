package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/monitor"
	dashboard "github.com/osteele/weft/internal/ui/dashboard"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/web"
	"github.com/spf13/cobra"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Interactive terminal UI for managing jobs",
	Long: `Launch an interactive terminal UI for viewing and managing jobs.

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
	tuiCmd.Flags().BoolVar(&tuiMouse, "mouse", true, "Enable mouse support (disables terminal text selection)")
}

var tuiMouse bool

func runTUI(cmd *cobra.Command, args []string) error {
	ensureDaemonForWork(os.Stderr)

	// Load config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Build TUI options from config
	opts := dashboard.DefaultModelOptions()
	if cfg.SyncActiveInterval > 0 {
		opts.SyncActiveInterval = time.Duration(cfg.SyncActiveInterval) * time.Second
	} else if cfg.SyncInterval > 0 {
		opts.SyncActiveInterval = time.Duration(cfg.SyncInterval) * time.Second
	}
	if cfg.SyncIdleInterval > 0 {
		opts.SyncIdleInterval = time.Duration(cfg.SyncIdleInterval) * time.Second
	}
	if cfg.LogRefreshInterval > 0 {
		opts.LogRefreshInterval = time.Duration(cfg.LogRefreshInterval) * time.Second
	}
	if cfg.HostRefreshInterval > 0 {
		opts.HostRefreshInterval = time.Duration(cfg.HostRefreshInterval) * time.Second
	}

	monCfg := monitor.DefaultConfig()
	monCfg.SyncActiveInterval = opts.SyncActiveInterval
	monCfg.SyncIdleInterval = opts.SyncIdleInterval
	monCfg.HostRefreshInterval = opts.HostRefreshInterval

	mon := monitor.New(database, monCfg)

	// Enable embedded remediation so failed jobs get diagnosed
	// without requiring the deprecated coordinator daemon.
	mon.EnableRemediationWithLogger(cfg, logging.Discard())

	// Suppress log output and capture stdout/stderr while the TUI runs to
	// avoid corrupting the alternate screen.
	stdio := terminal.InstallTUIStdioCapture()
	defer stdio.Restore()
	opts.Monitor = mon
	defer mon.Stop()
	snapshot := dashboard.LoadInitialSnapshot(database, opts.HostCacheDuration)
	opts.InitialSnapshot = &snapshot

	model := dashboard.NewModelWithOptions(database, opts)

	// Default to mouse enabled; flag can override
	useMouse := true
	if cmd.Flags().Changed("mouse") {
		useMouse = tuiMouse
	}

	programOpts := []tea.ProgramOption{stdio.Option, tea.WithAltScreen(), tea.WithReportFocus()}
	if useMouse {
		programOpts = append(programOpts, tea.WithMouseCellMotion())
	}

	p := tea.NewProgram(model, programOpts...)

	if cfg.WebEnabled {
		server, err := web.NewServer(mon, web.Config{Port: cfg.WebPort})
		if err != nil {
			return fmt.Errorf("start web server: %w", err)
		}
		if _, err := server.Start(); err != nil {
			return fmt.Errorf("start web server: %w", err)
		}
		defer server.Stop(context.Background())
	}

	_, err = p.Run()
	if err != nil {
		return fmt.Errorf("run TUI: %w", err)
	}

	return nil
}
