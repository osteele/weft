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
		queueName, jobIDs := parseBatchStatusArgs(os.Args[2:])
		batchStatus(queueName, jobIDs)
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
	var r2Bucket string
	queueName := ops.DefaultQueueName
	for _, arg := range args {
		if strings.HasPrefix(arg, "--r2-bucket=") {
			r2Bucket = arg[len("--r2-bucket="):]
		} else {
			queueName = arg
		}
	}
	cfg := runner.DefaultConfig(queueName)
	r := runner.New(cfg)
	if r2Bucket != "" {
		setupInventoryR2(r, r2Bucket)
	}
	if err := r.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "runner error: %v\n", err)
		os.Exit(1)
	}
}
