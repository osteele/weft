package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/runner"
)

// version is set via -ldflags "-X main.version=<agent-version>".
// The value is a deterministic local source hash in normal builds.
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

	// Handle cloud instance worker subcommands. run-campaign is kept as a
	// compatibility alias for manifests and cached agents that still use it.
	if len(os.Args) > 1 && (os.Args[1] == "run-instance" || os.Args[1] == "run-campaign") {
		runInstance(os.Args[2:])
		return
	}

	// Handle grace-wait subcommand
	if len(os.Args) > 1 && os.Args[1] == "grace-wait" {
		graceWait(os.Args[2:])
		return
	}

	// Handle heartbeat-sidecar subcommand
	if len(os.Args) > 1 && os.Args[1] == "heartbeat-sidecar" {
		runHeartbeatSidecar(os.Args[2:])
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
	parsed, err := parseRunQueueArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-queue: %v\n", err)
		os.Exit(2)
	}
	cfg := runner.DefaultConfig()
	cfg.SetupTimeout = parsed.SetupTimeout
	r := runner.New(cfg)
	if parsed.R2Bucket != "" {
		setupInventoryR2(r, parsed.R2Bucket)
		// Wire the Layer D R2-isolated source fallback: when the dispatcher
		// queues a job with SourceR2Key set, the runner asks us to fetch
		// the content-addressed tarball and extract it into a per-job dir.
		bucket := parsed.R2Bucket
		r.EnsureSourceFromR2 = func(_ int64, r2Key, perJobDir string) error {
			return fetchSourceTarballToDir(bucket, r2Key, perJobDir)
		}
	}
	if err := r.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "runner error: %v\n", err)
		os.Exit(1)
	}
}

type runQueueArgs struct {
	R2Bucket     string
	SetupTimeout time.Duration
}

func parseRunQueueArgs(args []string) (runQueueArgs, error) {
	var result runQueueArgs
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--r2-bucket="):
			result.R2Bucket = arg[len("--r2-bucket="):]
		case strings.HasPrefix(arg, "--setup-timeout="):
			d, err := time.ParseDuration(arg[len("--setup-timeout="):])
			if err != nil {
				return result, fmt.Errorf("invalid --setup-timeout: %w", err)
			}
			result.SetupTimeout = d
		case arg == opsqueue.AgentLegacyQueueArg:
			// Accept the legacy positional default queue name for compatibility.
		default:
			return result, fmt.Errorf("unsupported queue %q; only %q is supported", arg, opsqueue.AgentLegacyQueueArg)
		}
	}
	return result, nil
}
