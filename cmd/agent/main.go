package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/osteele/weft/internal/oplog"
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
