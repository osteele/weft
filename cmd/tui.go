package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/monitor"
	dashboard "github.com/osteele/weft/internal/ui/dashboard"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/web"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Interactive terminal UI for managing jobs",
	Long: `Launch an interactive terminal UI for viewing and managing jobs.

The TUI shows a split-screen view with:
  - Top panel: Job list with status indicators
  - Bottom panel: Log output for selected job

Use --session, --project, and --read-only together to open a non-mutating view
restricted to jobs attributed to one exact agent session and project root.

Keyboard shortcuts:
  Up/Down    Navigate job list
  Enter      Select job / view logs
  Escape     Clear selection
  R          Restart highlighted job
  k/Delete   Kill highlighted job
  P          Prune completed/dead jobs
  Ctrl-C/q   Quit
  Ctrl-Z     Suspend (resume with 'fg')`,
	RunE: runTUI,
}

func init() {
	rootCmd.AddCommand(tuiCmd)
	tuiCmd.Flags().BoolVar(&tuiMouse, "mouse", true, "Enable mouse support (disables terminal text selection)")
	tuiCmd.Flags().StringVar(&tuiSession, "session", "", "Show only jobs submitted by this exact agent session ID (requires --project and --read-only)")
	tuiCmd.Flags().StringVar(&tuiProject, "project", "", "Show only jobs from this absolute project root (requires --session and --read-only)")
	tuiCmd.Flags().BoolVar(&tuiReadOnly, "read-only", false, "Disable job, host, sync, cloud, and metadata mutations (requires --session and --project)")
}

var (
	tuiMouse    bool
	tuiSession  string
	tuiProject  string
	tuiReadOnly bool
)

func resolveTUIJobScope(session, project string, readOnly bool) (*dashboard.JobScope, error) {
	session = strings.TrimSpace(session)
	project = strings.TrimSpace(project)
	if session == "" && project == "" && !readOnly {
		return nil, nil
	}
	if session == "" || project == "" || !readOnly {
		return nil, usageErrorf("--session, --project, and --read-only must be used together")
	}
	if !filepath.IsAbs(project) {
		return nil, usageErrorf("--project must be an absolute path")
	}
	canonicalProject, err := workdir.CanonicalPath(project)
	if err != nil {
		canonicalProject = filepath.Clean(project)
	}
	return &dashboard.JobScope{
		SubmitterSession: session,
		ProjectRoot:      canonicalProject,
	}, nil
}

func runTUI(cmd *cobra.Command, args []string) error {
	jobScope, err := resolveTUIJobScope(tuiSession, tuiProject, tuiReadOnly)
	if err != nil {
		return err
	}
	if !tuiReadOnly {
		ensureDaemonForWork(os.Stderr)
	}
	// Load config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var database *sql.DB
	if tuiReadOnly {
		database, err = db.OpenReadOnlyForReading()
	} else {
		database, err = db.OpenForReading()
	}
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

	var mon *monitor.Monitor
	if !tuiReadOnly {
		monCfg := monitor.DefaultConfig()
		monCfg.SyncActiveInterval = opts.SyncActiveInterval
		monCfg.SyncIdleInterval = opts.SyncIdleInterval
		monCfg.HostRefreshInterval = opts.HostRefreshInterval

		mon = monitor.New(database, monCfg)
		mon.EnableRemediationWithLogger(cfg, logging.Discard())
		opts.Monitor = mon
		defer mon.Stop()
	}

	// Suppress log output and capture stdout/stderr while the TUI runs to
	// avoid corrupting the alternate screen.
	stdio := terminal.InstallTUIStdioCapture()
	defer stdio.Restore()
	var snapshot dashboard.InitialSnapshot
	if jobScope != nil {
		snapshot, err = dashboard.LoadInitialSnapshotForScope(database, opts.HostCacheDuration, jobScope)
		if err != nil {
			return fmt.Errorf("load scoped TUI snapshot: %w", err)
		}
	} else {
		snapshot = dashboard.LoadInitialSnapshot(database, opts.HostCacheDuration)
	}
	opts.InitialSnapshot = &snapshot
	opts.JobScope = jobScope
	opts.ReadOnly = tuiReadOnly

	model := dashboard.NewModelWithOptions(database, opts)

	// Mouse reporting defaults to the config value; the flag only overrides
	// it when set explicitly.
	useMouse := cfg.EnableMouse
	if cmd.Flags().Changed("mouse") {
		useMouse = tuiMouse
	}

	programOpts := []tea.ProgramOption{stdio.Option, tea.WithAltScreen(), tea.WithReportFocus()}
	if useMouse {
		programOpts = append(programOpts, tea.WithMouseCellMotion())
	}

	p := tea.NewProgram(model, programOpts...)

	if cfg.WebEnabled && !tuiReadOnly {
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
