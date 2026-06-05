package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

var coordinatorCmd = &cobra.Command{
	Use:   "coordinator",
	Short: "Manage the deprecated coordinator daemon",
	Long:  `Deprecated: the coordinator daemon is no longer part of normal weft operation. Use local CLI/TUI placement, sync, autopilot, and remote agents instead.`,
}

var coordinatorStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Deprecated: coordinator start is disabled",
	Long:  `Deprecated: coordinator start is disabled. Use local CLI/TUI placement, sync, autopilot, and remote agents instead.`,
	RunE:  runCoordinatorStart,
}

var coordinatorStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a legacy coordinator daemon",
	Long:  `Deprecated cleanup command: stop a legacy coordinator daemon if one is still running.`,
	RunE:  runCoordinatorStop,
}

var coordinatorStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show legacy coordinator daemon status",
	Long:  `Deprecated cleanup command: report whether a legacy coordinator daemon is still running.`,
	RunE:  runCoordinatorStatus,
}

var coordinatorInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Deprecated: coordinator install is disabled",
	Long:  `Deprecated: coordinator install is disabled. Use local CLI/TUI placement, sync, autopilot, and remote agents instead.`,
	RunE:  runCoordinatorInstall,
}

var coordinatorUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove legacy coordinator launchd service",
	Long:  `Deprecated cleanup command: remove a legacy coordinator launchd service.`,
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
	return fmt.Errorf("coordinator start is deprecated and disabled; use local placement, sync, autopilot, and remote agents instead")
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

	if !util.IsProcessAlive(pid) {
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
	return fmt.Errorf("coordinator install is deprecated and disabled; use local placement, sync, autopilot, and remote agents instead")
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
