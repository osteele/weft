package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/daemonapi"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/localmutate"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/syncorch"
	"github.com/spf13/cobra"
)

var (
	daemonStatusJSON bool
	daemonLogsFollow bool
)

const (
	daemonInterruptiblePollInterval = 15 * time.Second
	daemonStopTimeout               = 30 * time.Second
)

var (
	daemonSyncAll          = syncorch.SyncAll
	daemonPrePassSyncGrace = 5 * time.Second
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run and control the sync + autopilot daemon",
	Long: `Run and control the background daemon that continuously syncs job state
and runs gated autopilot passes. The daemon PID file is for local process
control only; autopilot ownership and sync exclusivity stay in the database.`,
}

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the sync + autopilot daemon in the foreground",
	Args:  cobra.NoArgs,
	RunE:  runDaemonRun,
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the sync + autopilot daemon",
	Args:  cobra.NoArgs,
	RunE:  runDaemonStart,
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the sync + autopilot daemon",
	Args:  cobra.NoArgs,
	RunE:  runDaemonStop,
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the sync + autopilot daemon",
	Args:  cobra.NoArgs,
	RunE:  runDaemonRestart,
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show sync + autopilot daemon status",
	Args:  cobra.NoArgs,
	RunE:  runDaemonStatus,
}

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the sync + autopilot daemon as a launchd service",
	Args:  cobra.NoArgs,
	RunE:  runDaemonInstall,
}

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the sync + autopilot launchd service",
	Args:  cobra.NoArgs,
	RunE:  runDaemonUninstall,
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show sync + autopilot daemon logs",
	Args:  cobra.NoArgs,
	RunE:  runDaemonLogs,
}

func init() {
	rootCmd.AddCommand(daemonCmd)
	daemonCmd.AddCommand(daemonRunCmd)
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonInstallCmd)
	daemonCmd.AddCommand(daemonUninstallCmd)
	daemonCmd.AddCommand(daemonLogsCmd)
	daemonStatusCmd.Flags().BoolVar(&daemonStatusJSON, "json", false, "Emit machine-readable JSON")
	daemonLogsCmd.Flags().BoolVarP(&daemonLogsFollow, "follow", "f", false, "Follow log output")
}

func runDaemonRun(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	pid := os.Getpid()
	lock, err := daemoncontrol.AcquireLock(paths)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := daemoncontrol.EnsureSocketAvailable(paths, 100*time.Millisecond); err != nil {
		return err
	}
	if err := daemoncontrol.WritePIDFile(paths.PIDFile, pid); err != nil {
		return err
	}
	if err := daemoncontrol.WriteMetadata(paths, pid, Version); err != nil {
		return err
	}
	defer func() {
		_ = daemoncontrol.RemovePIDFileIfOwn(paths.PIDFile, pid)
		_ = daemoncontrol.RemoveMetadataFileIfOwn(paths, pid)
	}()

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "warning: load config: %v\n", cfgErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	writeExecutor := daemonapi.NewWriteExecutor(ctx, database)
	watchServer, err := daemonapi.StartServerWithOptions(ctx, database, paths.SocketFile, daemonapi.ServerOptions{
		Info:     daemonInfo(pid, Version),
		Shutdown: stop,
		Mutate:   localmutate.Handler,
		Writer:   writeExecutor,
	})
	if err != nil {
		return fmt.Errorf("start daemon watch socket: %w", err)
	}
	defer watchServer.Close()
	changeSource := openDBChangeSource("daemon")
	if changeSource != nil {
		defer changeSource.Close()
	}
	wakeSnapshot, snapshotErr := readAutopilotWakeSnapshot(database)
	if snapshotErr != nil {
		fmt.Fprintf(os.Stderr, "warning: read autopilot wake state: %v\n", snapshotErr)
	}

	fmt.Printf("[%s] daemon started pid=%d\n", time.Now().Format("15:04:05"), pid)
	pass := 0
	runAutopilot := true
	lastOutcome := outcomeIdle
	for {
		if ctx.Err() != nil {
			return nil
		}
		pass++
		wait, outcome := runDaemonPass(ctx, database, cfg, pass, runAutopilot)
		if nextSnapshot, err := readAutopilotWakeSnapshot(database); err == nil {
			wakeSnapshot = nextSnapshot
		} else {
			fmt.Fprintf(os.Stderr, "warning: read autopilot wake state: %v\n", err)
		}
		lastOutcome = outcome
		var reason autopilotWakeReason
		wakeSnapshot, reason = waitForAutopilotInvalidation(ctx, changeSource, nil, database, wait, true, wakeSnapshot)
		if reason == autopilotWakeDone {
			return nil
		}
		runAutopilot = reason != autopilotWakeTimer || autopilotTimerRunsPass(lastOutcome)
	}
}

func daemonInfo(pid int, version string) daemonapi.DaemonInfo {
	info := daemonapi.DaemonInfo{
		PID:       pid,
		Version:   version,
		StartedAt: time.Now().Unix(),
	}
	if exe, err := os.Executable(); err == nil {
		info.Executable = exe
		if stat, err := os.Stat(exe); err == nil {
			info.ExecutableModTime = stat.ModTime().Unix()
		}
	}
	return info
}

func daemonPrePassHostTimeout() time.Duration {
	return FastSyncHostTimeout
}

func runDaemonPass(ctx context.Context, database *sql.DB, cfg *config.Config, pass int, runAutopilot bool) (time.Duration, autopilotOutcome) {
	started := time.Now()
	var (
		result *orchestration.GroupedAutoPilotResult
		runErr error
	)
	if runAutopilot {
		paused, perr := orchestration.IsAutopilotPaused(database)
		if perr == nil && paused {
			runErr = orchestration.ErrAutopilotPaused
		} else {
			result, runErr = orchestration.RunGroupedAutoPilotPassGated(ctx, database, nil, fmt.Sprintf("daemon/%d", os.Getpid()))
		}
	}

	// Keep sync out of the critical path for claiming autopilot. Cloud/R2 sync can
	// time out or contend on the DB; new jobs should not sit in "waiting:
	// autopilot" while the daemon is refreshing auxiliary state.
	syncResult := runDaemonPrePassSync(ctx, database, cfg, syncorch.SyncOptions{
		SSHTimeout:        FastSyncTimeout,
		HostTimeout:       daemonPrePassHostTimeout(),
		CloudMode:         syncorch.CloudBounded,
		CloudTimeout:      NormalCloudSyncTimeout,
		StartQueueRunner:  true,
		EnsureQueueRunner: ensureQueueRunnerStarted,
		Verbose:           verbose,
	})
	for _, warning := range syncResult.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}

	outcome, wait := classifyAutopilotPass(result, runErr, autopilotRunPausedWait)
	if syncResult.HostsUpdated+syncResult.CloudUpdated > 0 && outcome != outcomeError {
		outcome = outcomeProgress
		wait = orchestration.AutopilotCooldownProgress
	}
	if !syncResult.AllCompleted && outcome == outcomeIdle {
		outcome = outcomeBlocked
		wait = orchestration.AutopilotCooldownBlocked
	}
	wait = capDaemonWaitForInterruptibles(database, wait)
	emitDaemonPass(pass, started, syncResult, result, outcome, runErr, wait)
	return wait, outcome
}

func runDaemonPrePassSync(ctx context.Context, database *sql.DB, cfg *config.Config, opts syncorch.SyncOptions) syncorch.SyncResult {
	budget := daemonPrePassSyncBudget(opts)
	syncCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	opts.Context = syncCtx

	result := daemonSyncAll(database, cfg, opts)
	if ctx.Err() != nil {
		return syncorch.SyncResult{
			Warnings:     []string{"daemon pre-pass sync canceled; continuing shutdown"},
			AllCompleted: false,
		}
	}
	if syncCtx.Err() != nil && !result.AllCompleted {
		result.Warnings = append(result.Warnings, fmt.Sprintf("daemon pre-pass sync exceeded %s; continuing to autopilot", budget.Truncate(time.Second)))
		result.AllCompleted = false
	}
	return result
}

func daemonPrePassSyncBudget(opts syncorch.SyncOptions) time.Duration {
	budget := opts.HostTimeout
	if budget <= 0 {
		budget = daemonPrePassHostTimeout()
	}
	if opts.CloudMode != syncorch.CloudDisabled {
		cloudTimeout := opts.CloudTimeout
		if cloudTimeout > budget {
			budget = cloudTimeout
		}
	}
	if budget <= 0 {
		budget = daemonPrePassHostTimeout()
	}
	return budget + daemonPrePassSyncGrace
}

func capDaemonWaitForInterruptibles(database *sql.DB, wait time.Duration) time.Duration {
	if database == nil || wait <= daemonInterruptiblePollInterval {
		return wait
	}
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*)
		  FROM launches
		 WHERE instance_type = ?
		   AND status IN (?, ?, ?, ?)`,
		cloud.InstanceTypeInterruptible,
		db.LaunchStatusLaunching,
		db.LaunchStatusRunning,
		db.LaunchStatusPaused,
		db.LaunchStatusGrace).Scan(&count)
	if err != nil || count == 0 {
		return wait
	}
	return daemonInterruptiblePollInterval
}

func emitDaemonPass(pass int, started time.Time, syncResult syncorch.SyncResult, result *orchestration.GroupedAutoPilotResult, outcome autopilotOutcome, runErr error, wait time.Duration) {
	placed, launched, rebalanced, blocked := 0, 0, 0, 0
	if result != nil {
		placed = result.Placed
		launched = result.Launched
		rebalanced = result.Rebalanced
		blocked = orchestration.AutoPilotBlockedReasonCount(result.BlockedReasons)
	}
	parts := []string{
		fmt.Sprintf("pass %d", pass),
		string(outcome),
		fmt.Sprintf("sync=%d", syncResult.HostsUpdated+syncResult.CloudUpdated),
		fmt.Sprintf("hosts=%d", syncResult.HostsReached),
		fmt.Sprintf("placed=%d", placed),
		fmt.Sprintf("launched=%d", launched),
		fmt.Sprintf("rebalanced=%d", rebalanced),
		fmt.Sprintf("blocked=%d", blocked),
		fmt.Sprintf("dur=%s", time.Since(started).Truncate(time.Millisecond)),
	}
	if len(syncResult.HostsUnreachable) > 0 {
		parts = append(parts, fmt.Sprintf("offline=%s", strings.Join(syncResult.HostsUnreachable, ",")))
	}
	if len(syncResult.HostsSlow) > 0 {
		parts = append(parts, fmt.Sprintf("slow=%s", strings.Join(syncResult.HostsSlow, ",")))
	}
	if runErr != nil && !errors.Is(runErr, orchestration.ErrAutopilotPaused) && !errors.Is(runErr, orchestration.ErrAutopilotBusy) {
		parts = append(parts, fmt.Sprintf("err=%q", runErr.Error()))
	}
	fmt.Printf("[%s] %s (next in %s)\n", time.Now().Format("15:04:05"), strings.Join(parts, " "), wait.Truncate(time.Second))
}

func runDaemonStart(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	status, action, err := ensureDaemonStarted(paths, 2*time.Second)
	if err != nil {
		return err
	}
	if action == daemoncontrol.EnsureStarted && status.Installed {
		fmt.Printf("Started daemon via launchd (%s)\n", daemoncontrol.Label)
	} else if action == daemoncontrol.EnsureStarted {
		fmt.Printf("Started daemon (PID %d)\n", status.PID)
	} else if action == daemoncontrol.EnsureRestarted {
		fmt.Printf("Restarted daemon (PID %d)\n", status.PID)
	} else {
		fmt.Printf("Daemon already running (PID %d)\n", status.PID)
	}
	fmt.Printf("stdout: %s\n", paths.StdoutLog)
	fmt.Printf("stderr: %s\n", paths.StderrLog)
	return nil
}

func runDaemonStop(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	before, _ := daemoncontrol.CurrentStatus(paths)
	if daemoncontrol.IsInstalled(paths) {
		if err := daemoncontrol.Unload(paths); err != nil {
			return err
		}
	}
	pid, hadProcess, err := daemoncontrol.StopPID(paths, daemonStopTimeout)
	if err != nil {
		return err
	}
	if pid == 0 && before.PID > 0 && !daemoncontrol.ProcessLive(before.PID) {
		pid = before.PID
		hadProcess = before.Live
	}
	if pid > 0 {
		if err := releaseDaemonAutopilotClaim(pid, "daemon stopped"); err != nil {
			return err
		}
	}
	switch {
	case pid == 0:
		fmt.Println("Daemon is not running (no PID file)")
	case hadProcess:
		fmt.Printf("Stopped daemon (PID %d)\n", pid)
	default:
		fmt.Printf("Removed stale daemon PID file (PID %d)\n", pid)
	}
	return nil
}

func runDaemonRestart(cmd *cobra.Command, args []string) error {
	if err := runDaemonStop(cmd, args); err != nil {
		return err
	}
	return runDaemonStart(cmd, args)
}

type daemonStatusView struct {
	State             string             `json:"state"`
	PID               int                `json:"pid,omitempty"`
	PIDFile           string             `json:"pid_file"`
	StdoutLog         string             `json:"stdout_log"`
	StderrLog         string             `json:"stderr_log"`
	Installed         bool               `json:"installed"`
	LaunchdLabel      string             `json:"launchd_label"`
	ActiveBinaryStale bool               `json:"active_binary_stale,omitempty"`
	DaemonVersion     string             `json:"daemon_version,omitempty"`
	DaemonExecutable  string             `json:"daemon_executable,omitempty"`
	Autopilot         autopilotStateView `json:"autopilot"`
}

func runDaemonStatus(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	status, err := daemoncontrol.CurrentStatus(paths)
	if err != nil {
		return err
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	apState, err := db.LoadAutopilotState(database)
	if err != nil {
		return fmt.Errorf("load autopilot state: %w", err)
	}
	view := daemonStatusView{
		State:             daemonProcessState(status),
		PID:               status.PID,
		PIDFile:           paths.PIDFile,
		StdoutLog:         paths.StdoutLog,
		StderrLog:         paths.StderrLog,
		Installed:         status.Installed,
		LaunchdLabel:      daemoncontrol.Label,
		ActiveBinaryStale: status.ActiveBinaryStale,
		Autopilot:         buildAutopilotStateView(apState),
	}
	if status.Metadata != nil {
		view.DaemonVersion = status.Metadata.Version
		view.DaemonExecutable = status.Metadata.Executable
	}
	if daemonStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	fmt.Printf("daemon: %s", view.State)
	if status.HasPID {
		fmt.Printf(" (pid %d)", status.PID)
	}
	fmt.Println()
	if status.ActiveBinaryStale {
		fmt.Println("warning: daemon binary is older than the current weft executable; run `weft daemon restart`")
	}
	fmt.Printf("pidfile: %s\n", paths.PIDFile)
	fmt.Printf("stdout: %s\n", paths.StdoutLog)
	fmt.Printf("stderr: %s\n", paths.StderrLog)
	if status.Installed {
		fmt.Printf("launchd: installed (%s)\n", daemoncontrol.Label)
	} else {
		fmt.Println("launchd: not installed")
	}
	fmt.Println(formatAutopilotStatusText(view.Autopilot))
	return nil
}

func daemonProcessState(status daemoncontrol.Status) string {
	switch {
	case status.Live && status.ActiveBinaryStale:
		return "stale"
	case status.Live:
		return "running"
	case status.Stale:
		return "stale"
	default:
		return "stopped"
	}
}

func runDaemonInstall(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	installed := daemoncontrol.IsInstalled(paths)
	transition, err := daemoncontrol.InstallTransition(paths, daemonStopTimeout)
	if err != nil {
		return err
	}
	if transition.OldPID > 0 {
		if err := releaseDaemonAutopilotClaim(transition.OldPID, "daemon service updated"); err != nil {
			return err
		}
	}
	if installed {
		fmt.Println("Updated daemon launchd service")
	} else {
		fmt.Println("Installed daemon launchd service")
	}
	fmt.Printf("plist: %s\n", paths.PlistFile)
	return nil
}

func releaseDaemonAutopilotClaim(pid int, summary string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	if err := db.ReleaseAutopilotPass(database, pid, 0, summary, context.Canceled); err != nil {
		return fmt.Errorf("release daemon autopilot claim: %w", err)
	}
	return nil
}

func runDaemonUninstall(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	if !daemoncontrol.IsInstalled(paths) {
		fmt.Println("Daemon launchd service is not installed")
		return nil
	}
	if err := daemoncontrol.Uninstall(paths); err != nil {
		return err
	}
	fmt.Println("Uninstalled daemon launchd service")
	return nil
}

func runDaemonLogs(cmd *cobra.Command, args []string) error {
	paths := daemoncontrol.DefaultPaths()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return printDaemonLogs(ctx, paths, daemonLogsFollow)
}

func printDaemonLogs(ctx context.Context, paths daemoncontrol.Paths, follow bool) error {
	offsets := map[string]int64{}
	for _, path := range []string{paths.StdoutLog, paths.StderrLog} {
		n, err := printLogFile(os.Stdout, path, 0)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		offsets[path] = n
	}
	if !follow {
		return nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for _, path := range []string{paths.StdoutLog, paths.StderrLog} {
				n, err := printLogFile(os.Stdout, path, offsets[path])
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				offsets[path] = n
			}
		}
	}
}

func printLogFile(out io.Writer, path string, offset int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return offset, err
	}
	size := stat.Size()
	if offset > size {
		offset = 0
	}
	if offset == size {
		return size, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	if offset == 0 {
		fmt.Fprintf(out, "==> %s <==\n", path)
	}
	if _, err := io.Copy(out, f); err != nil {
		return offset, err
	}
	return size, nil
}
