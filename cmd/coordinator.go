package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

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

The coordinator watches for intent files, scores hosts, and dispatches
jobs to remote queue runners. It handles host connectivity changes by
queuing intents for offline hosts and retrying when they come back online.

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

func init() {
	rootCmd.AddCommand(coordinatorCmd)
	coordinatorCmd.AddCommand(coordinatorStartCmd)
	coordinatorCmd.AddCommand(coordinatorStopCmd)
	coordinatorCmd.AddCommand(coordinatorStatusCmd)
}

func runCoordinatorStart(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	config := coordinator.DefaultConfig()
	c := coordinator.New(database, config)

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
	fmt.Printf("Intent dir: %s\n", config.IntentDir)
	fmt.Printf("Log: %s\n", config.LogPath)
	return nil
}
