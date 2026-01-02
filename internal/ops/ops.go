// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"time"
)

// Result represents the outcome of an operation
type Result struct {
	Success  bool   // Operation completed successfully
	Deferred bool   // Operation was queued for later (host unreachable)
	JobID    int64  // Job ID (for create/restart operations)
	Message  string // Human-readable result message
}

// ExecuteOptions configures operation execution
type ExecuteOptions struct {
	Timeout time.Duration // SSH timeout (default 30s)
	Verbose bool          // Print verbose output
}

// DefaultOptions returns default execution options
func DefaultOptions() ExecuteOptions {
	return ExecuteOptions{
		Timeout: 30 * time.Second,
		Verbose: false,
	}
}
