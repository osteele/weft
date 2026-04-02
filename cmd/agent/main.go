package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/runner"
)

// version is set via -ldflags "-X main.version=<commit-hash>"
var version = "dev"

// agentLogPath returns the path for the agent's operations log.
func agentLogPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/tmp"
	}
	return home + "/.cache/weft/agent-operations.log"
}

func main() {
	// Handle --version without initializing logging
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("weft-agent %s\n", version)
		os.Exit(0)
	}

	// Handle run-queue subcommand
	if len(os.Args) > 1 && os.Args[1] == "run-queue" {
		runQueue(os.Args[2:])
		return
	}

	// Handle run-job subcommand
	if len(os.Args) > 1 && os.Args[1] == "run-job" {
		runJob(os.Args[2:])
		return
	}

	// Handle run-campaign subcommand
	if len(os.Args) > 1 && os.Args[1] == "run-campaign" {
		runCampaign(os.Args[2:])
		return
	}

	// Handle grace-wait subcommand
	if len(os.Args) > 1 && os.Args[1] == "grace-wait" {
		graceWait(os.Args[2:])
		return
	}

	// Handle batch-status subcommand
	if len(os.Args) > 1 && os.Args[1] == "batch-status" {
		jobIDs, err := parseBatchStatusArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "batch-status: %v\n", err)
			os.Exit(2)
		}
		batchStatus(jobIDs)
		return
	}

	// Initialize structured logging (JSON for agent, debug-level for remote diagnostics)
	logging.Setup(os.Stderr, "json")
	logging.SetLevel(slog.LevelDebug)

	// Initialize ops logging
	if err := oplog.Init(agentLogPath(), 0); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to init ops log: %v\n", err)
	} else {
		defer oplog.Close()
	}

	oplog.Log(oplog.OpAgentStart, oplog.WithDetail(version))

	// Set up graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf("weft-agent %s\n", version)
	fmt.Println("Agent started. Waiting for signal to stop.")

	sig := <-sigCh
	oplog.Log(oplog.OpAgentStop, oplog.WithDetailf("signal: %s", sig))
	fmt.Printf("\nReceived %s, shutting down.\n", sig)
}

func runQueue(args []string) {
	r2Bucket, err := parseRunQueueArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-queue: %v\n", err)
		os.Exit(2)
	}
	cfg := runner.DefaultConfig()
	r := runner.New(cfg)
	if r2Bucket != "" {
		setupInventoryR2(r, r2Bucket)
	}
	if err := r.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "runner error: %v\n", err)
		os.Exit(1)
	}
}

func parseRunQueueArgs(args []string) (string, error) {
	var r2Bucket string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--r2-bucket="):
			r2Bucket = arg[len("--r2-bucket="):]
		case arg == ops.DefaultQueueName:
			// Accept the legacy positional default queue name for compatibility.
		default:
			return "", fmt.Errorf("unsupported queue %q; only %q is supported", arg, ops.DefaultQueueName)
		}
	}
	return r2Bucket, nil
}
