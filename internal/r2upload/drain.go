// Package r2upload runs rclone-backed uploads to R2 with a progress-watched
// gate: a stall watchdog (no bytes-progress for N seconds → abort) plus a
// size-derived hard ceiling (baseline + bytes/floor, capped at MaxDrain).
//
// The gate is intentionally proportional to the size of what the agent is
// trying to upload, so a 1 MiB log and a 1 GiB checkpoint don't share the
// same timeout, while still bounding the worst-case wait before instance
// self-destruct.
//
// On stall/ceiling/error the caller can write a structured failure marker
// (see [WriteFailureMarker]) so the coordinator and CLI can surface
// "upload truncated: stall|ceiling|error" rather than the log silently
// disappearing with the instance.
package r2upload

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Defaults. Callers may override individual fields via [Options].
const (
	DefaultStallTimeout    = 30 * time.Second
	DefaultFloorThroughput = 256 * 1024 // bytes/sec
	DefaultMaxDrain        = 15 * time.Minute
	DefaultBaseline        = 60 * time.Second
	DefaultTickInterval    = 2 * time.Second
)

// Status values returned by [Drain].
const (
	StatusOK      = "ok"
	StatusStalled = "stalled"
	StatusCeiling = "ceiling"
	StatusError   = "error"
)

// KilledBy values, embedded in failure markers.
const (
	KilledByStall    = "stall"
	KilledByCeiling  = "ceiling"
	KilledByCanceled = "canceled"
)

// Options configures one [Drain] invocation.
type Options struct {
	// Source is the local file or directory rclone will copy from.
	Source string
	// DestRemote is the full rclone destination, e.g.
	// "r2:bucket/jobs/123/runs/456/results/".
	DestRemote string
	// Command is "copy" or "copyto".
	Command string
	// Extra holds extra rclone args (e.g. ["--update"]).
	Extra []string

	// TotalBytes is the pre-measured source size in bytes. Pass 0 if unknown
	// — Drain will then use just the baseline as its ceiling.
	TotalBytes int64

	// StallTimeout: if no byte-progress in this window, abort. Zero → default.
	StallTimeout time.Duration
	// FloorThroughput: bytes/sec used to compute the ceiling. Zero → default.
	FloorThroughput int64
	// MaxDrain: absolute ceiling regardless of size. Zero → default.
	MaxDrain time.Duration
	// Baseline added to the ceiling to cover rclone startup and final flush.
	// Zero → default.
	Baseline time.Duration
	// TickInterval: how often the watchdog wakes up. Zero → default.
	TickInterval time.Duration

	// Runner is the command runner. Zero → real exec-based rclone.
	Runner Runner
}

// Result describes a [Drain] outcome.
type Result struct {
	// Status is one of [StatusOK], [StatusStalled], [StatusCeiling], [StatusError].
	Status string `json:"status"`
	// Reason is a human-readable one-line explanation.
	Reason string `json:"reason,omitempty"`
	// KilledBy is "stall", "ceiling", or "canceled" when we killed rclone;
	// "" when rclone exited on its own (success or its own error).
	KilledBy string `json:"killed_by,omitempty"`

	BytesUploaded  int64   `json:"bytes_uploaded"`
	BytesTotal     int64   `json:"bytes_total"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	StartedAtUnix  int64   `json:"started_at_unix"`
	EndedAtUnix    int64   `json:"ended_at_unix"`

	// CeilingSeconds is the computed ceiling (size-derived + baseline,
	// clamped at MaxDrain). Useful for explaining "why now".
	CeilingSeconds float64 `json:"ceiling_seconds"`
	// StallTimeoutSeconds is the stall watchdog window in effect.
	StallTimeoutSeconds float64 `json:"stall_timeout_seconds"`

	// StderrTail is the last few stderr lines from rclone — useful for
	// post-mortem diagnosis when status != ok.
	StderrTail string `json:"stderr_tail,omitempty"`

	// Err is the underlying error, if any. Not serialized — callers use
	// Status/Reason for the wire form.
	Err error `json:"-"`
}

// Drain runs rclone with a stall watchdog and a size-derived ceiling.
//
// Drain does not write the upload-failure marker itself; the caller is
// responsible for that via [WriteFailureMarker] with a separate, bounded
// context. This keeps the upload path from recursing into another drain.
func Drain(ctx context.Context, opts Options) Result {
	cfg := applyDefaults(opts)

	ceiling := computeCeiling(cfg)
	args := buildArgs(cfg)

	started := time.Now()
	result := Result{
		BytesTotal:          cfg.TotalBytes,
		StartedAtUnix:       started.Unix(),
		CeilingSeconds:      ceiling.Seconds(),
		StallTimeoutSeconds: cfg.StallTimeout.Seconds(),
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	proc, startErr := cfg.Runner.Start(runCtx, args)
	if startErr != nil {
		result.Status = StatusError
		result.Reason = "rclone start failed: " + startErr.Error()
		result.Err = startErr
		result.EndedAtUnix = time.Now().Unix()
		result.ElapsedSeconds = time.Since(started).Seconds()
		return result
	}

	var (
		mu             sync.Mutex
		bytesUploaded  int64
		lastProgressAt = started
		stderrTail     []string
	)

	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		scanner := bufio.NewScanner(proc.Stderr())
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			stderrTail = append(stderrTail, line)
			if len(stderrTail) > 20 {
				stderrTail = stderrTail[len(stderrTail)-20:]
			}
			if n, ok := extractBytes(line); ok && n > bytesUploaded {
				bytesUploaded = n
				lastProgressAt = time.Now()
			}
			mu.Unlock()
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- proc.Wait() }()

	ticker := time.NewTicker(cfg.TickInterval)
	defer ticker.Stop()
	deadline := started.Add(ceiling)

	finish := func(status, killedBy, reason string, err error) Result {
		// Drain proc + parser
		if killedBy != "" {
			proc.Kill()
		}
		<-waitErr
		<-parseDone

		mu.Lock()
		result.BytesUploaded = bytesUploaded
		result.StderrTail = strings.Join(stderrTail, "\n")
		mu.Unlock()

		result.Status = status
		result.KilledBy = killedBy
		result.Reason = reason
		result.Err = err
		ended := time.Now()
		result.EndedAtUnix = ended.Unix()
		result.ElapsedSeconds = ended.Sub(started).Seconds()
		return result
	}

	for {
		select {
		case err := <-waitErr:
			// rclone exited on its own. Drain parser, then resolve status.
			<-parseDone
			mu.Lock()
			result.BytesUploaded = bytesUploaded
			result.StderrTail = strings.Join(stderrTail, "\n")
			mu.Unlock()
			ended := time.Now()
			result.EndedAtUnix = ended.Unix()
			result.ElapsedSeconds = ended.Sub(started).Seconds()
			if err != nil {
				result.Status = StatusError
				result.Reason = "rclone exit: " + err.Error()
				result.Err = err
				return result
			}
			result.Status = StatusOK
			return result

		case now := <-ticker.C:
			mu.Lock()
			stallElapsed := now.Sub(lastProgressAt)
			seen := bytesUploaded
			mu.Unlock()

			if stallElapsed > cfg.StallTimeout {
				reason := fmt.Sprintf(
					"no progress for %s (stall timeout %s); uploaded %d/%d bytes",
					stallElapsed.Truncate(time.Second), cfg.StallTimeout,
					seen, cfg.TotalBytes,
				)
				return finish(StatusStalled, KilledByStall, reason, nil)
			}
			if now.After(deadline) {
				reason := fmt.Sprintf(
					"ceiling %s reached; uploaded %d/%d bytes",
					ceiling.Truncate(time.Second), seen, cfg.TotalBytes,
				)
				return finish(StatusCeiling, KilledByCeiling, reason, nil)
			}

		case <-ctx.Done():
			return finish(StatusError, KilledByCanceled,
				"context canceled: "+ctx.Err().Error(), ctx.Err())
		}
	}
}

// applyDefaults returns a copy of opts with zero-valued fields filled in.
func applyDefaults(opts Options) Options {
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = DefaultStallTimeout
	}
	if opts.FloorThroughput <= 0 {
		opts.FloorThroughput = DefaultFloorThroughput
	}
	if opts.MaxDrain <= 0 {
		opts.MaxDrain = DefaultMaxDrain
	}
	if opts.Baseline <= 0 {
		opts.Baseline = DefaultBaseline
	}
	if opts.TickInterval <= 0 {
		opts.TickInterval = DefaultTickInterval
	}
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	if opts.Command == "" {
		opts.Command = "copy"
	}
	return opts
}

func computeCeiling(cfg Options) time.Duration {
	ceiling := cfg.Baseline
	if cfg.TotalBytes > 0 {
		ceiling += time.Duration(cfg.TotalBytes/cfg.FloorThroughput) * time.Second
	}
	if ceiling > cfg.MaxDrain {
		ceiling = cfg.MaxDrain
	}
	return ceiling
}

func buildArgs(cfg Options) []string {
	args := make([]string, 0, 8+len(cfg.Extra))
	args = append(args, cfg.Command)
	args = append(args, cfg.Extra...)
	// Progress reporting: emit a single-line stats summary every 2s, in JSON
	// log form so each line is a complete record we can parse.
	args = append(args,
		"--use-json-log",
		"--stats", "2s",
		"--stats-one-line",
		"--stats-log-level", "NOTICE",
	)
	args = append(args, cfg.Source, cfg.DestRemote)
	return args
}

// Runner abstracts the rclone command for testability. The real
// implementation is [ExecRunner].
type Runner interface {
	Start(ctx context.Context, args []string) (Process, error)
}

// Process is one running rclone invocation.
type Process interface {
	// Stderr returns a stream the caller scans for progress lines. It is
	// closed when the process exits.
	Stderr() io.Reader
	// Wait blocks until the process exits and returns its exit error
	// (nil on success).
	Wait() error
	// Kill best-effort terminates the process. Subsequent Wait must still
	// return (with a non-nil error).
	Kill()
}
