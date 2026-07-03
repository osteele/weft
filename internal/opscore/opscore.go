// Package opscore provides shared types used across ops sub-packages.
package opscore

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

// TimeoutMode controls the timeout behavior for operations.
// Use these modes instead of raw durations to centralize timeout configuration
// and enable future config file overrides.
type TimeoutMode int

const (
	// TimeoutFast is for TUI and --fast flag: quick attempt, defer if unreachable.
	TimeoutFast TimeoutMode = iota

	// TimeoutNormal is the CLI default: moderate timeout for typical operations.
	TimeoutNormal

	// TimeoutSync is for --sync flag: wait longer for operation to complete.
	TimeoutSync
)

// String returns a string representation of the timeout mode.
func (m TimeoutMode) String() string {
	switch m {
	case TimeoutFast:
		return "fast"
	case TimeoutNormal:
		return "normal"
	case TimeoutSync:
		return "sync"
	default:
		return "unknown"
	}
}

// Duration returns the timeout duration for this mode.
func (m TimeoutMode) Duration() time.Duration {
	switch m {
	case TimeoutFast:
		return 5 * time.Second
	case TimeoutNormal:
		return 30 * time.Second
	case TimeoutSync:
		return 2 * time.Minute
	default:
		return 30 * time.Second
	}
}

// ExecuteOptions configures operation execution
type ExecuteOptions struct {
	Timeout time.Duration // SSH timeout (default 30s)
	Verbose bool          // Print verbose output

	// UnplaceReason, when non-empty, marks an unplace as a re-place/drain back
	// to the on-prem pool: the operation records this reason and does NOT
	// promote the job to rental. Empty preserves the default move-to-cloud
	// behavior (promote to rental + "manually moved to unplaced queue").
	UnplaceReason string
}

// DefaultOptions returns default execution options
func DefaultOptions() ExecuteOptions {
	return ExecuteOptions{
		Timeout: TimeoutNormal.Duration(),
		Verbose: false,
	}
}

// OptionsForMode returns ExecuteOptions configured for the given timeout mode
func OptionsForMode(mode TimeoutMode) ExecuteOptions {
	return ExecuteOptions{
		Timeout: mode.Duration(),
		Verbose: false,
	}
}
