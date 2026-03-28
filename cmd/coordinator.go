package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	appconfig "github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var coordinatorCmd = &cobra.Command{
	Use:   "coordinator",
	Short: "Manage the coordinator daemon",
	Long:  `The coordinator daemon centralizes job placement decisions, running on an always-on host (e.g., studio).`,
}

var coordinatorStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the coordinator daemon",
	Long: `Start the coordinator daemon in the foreground.

The coordinator manages cloud instance sweeps (Vast.ai), host state
monitoring, and job remediation.

Use Ctrl+C to stop.`,
	RunE: runCoordinatorStart,
}

var coordinatorStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the coordinator daemon",
	RunE:  runCoordinatorStop,
}

var coordinatorStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show coordinator daemon status",
	RunE:  runCoordinatorStatus,
}

var coordinatorInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install coordinator as a launchd service (macOS)",
	Long:  `Creates a launchd plist so the coordinator starts automatically on login and restarts if it crashes.`,
	RunE:  runCoordinatorInstall,
}

var coordinatorUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove coordinator launchd service",
	RunE:  runCoordinatorUninstall,
}

func init() {
	rootCmd.AddCommand(coordinatorCmd)
	coordinatorCmd.AddCommand(coordinatorStartCmd)
	coordinatorCmd.AddCommand(coordinatorStopCmd)
	coordinatorCmd.AddCommand(coordinatorStatusCmd)
	coordinatorCmd.AddCommand(coordinatorInstallCmd)
	coordinatorCmd.AddCommand(coordinatorUninstallCmd)
}

func runCoordinatorStart(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	config := coordinator.DefaultConfig()
	c := coordinator.New(database, config)

	cfg, cfgErr := appconfig.Load()
	if cfgErr != nil {
		slog.Warn("failed to load config", "component", "coordinator", "error", cfgErr)
	} else if clients, err := buildCloudClients(cfg); err != nil {
		slog.Warn("cloud clients unavailable", "component", "coordinator", "error", err)
	} else {
		c.CloudClients = clients
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return c.Run(ctx)
}

func runCoordinatorStop(cmd *cobra.Command, args []string) error {
	config := coordinator.DefaultConfig()

	data, err := os.ReadFile(config.PIDFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Coordinator is not running (no PID file)")
			return nil
		}
		return fmt.Errorf("read PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid PID file: %w", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send signal: %w", err)
	}

	fmt.Printf("Sent SIGTERM to coordinator (PID %d)\n", pid)
	return nil
}

func runCoordinatorStatus(cmd *cobra.Command, args []string) error {
	config := coordinator.DefaultConfig()

	data, err := os.ReadFile(config.PIDFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Status: not running")
			return nil
		}
		return fmt.Errorf("read PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid PID file: %w", err)
	}

	// Check if process is actually running
	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("Status: stale PID file (PID %d)\n", pid)
		return nil
	}

	// On Unix, FindProcess always succeeds. Send signal 0 to check.
	if err := process.Signal(syscall.Signal(0)); err != nil {
		fmt.Printf("Status: stale PID file (PID %d, process not running)\n", pid)
		return nil
	}

	fmt.Printf("Status: running (PID %d)\n", pid)
	fmt.Printf("Log: %s\n", config.LogPath)
	if coordinator.IsInstalled() {
		fmt.Printf("Launchd: installed (%s)\n", coordinator.PlistPath())
	}
	return nil
}

func runCoordinatorInstall(cmd *cobra.Command, args []string) error {
	if coordinator.IsInstalled() {
		fmt.Printf("Already installed at %s\n", coordinator.PlistPath())
		return nil
	}
	if err := coordinator.Install(); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Printf("Installed coordinator launchd service\n")
	fmt.Printf("  Plist: %s\n", coordinator.PlistPath())
	fmt.Println("The coordinator will start automatically on login.")
	return nil
}

func runCoordinatorUninstall(cmd *cobra.Command, args []string) error {
	if !coordinator.IsInstalled() {
		fmt.Println("Not installed")
		return nil
	}
	if err := coordinator.Uninstall(); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Println("Uninstalled coordinator launchd service")
	return nil
}
