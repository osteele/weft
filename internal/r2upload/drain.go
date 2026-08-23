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
// (see [WriteFailureMarker]) so the controller and CLI can surface
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
	DefaultStallTimeout = 30 * time.Second
	// DefaultInitialStallTimeout applies before the first byte is observed.
	// Larger than the steady-state stall timeout because rclone's pre-transfer
	// phase (HEAD requests, multipart upload init, chunk hashing) can run for
	// minutes against a multi-GB source on a high-latency provider before
	// the first chunk PUT lands and the progress counter ticks above zero.
	DefaultInitialStallTimeout = 5 * time.Minute
	// DefaultHeartbeatTimeout is how long rclone may be silent on stderr
	// before we consider it hung. Independent of byte progress — rclone
	// emits stats lines every 2s under --stats=2s even at zero throughput,
	// so any longer gap means the process is wedged, not just slow.
	DefaultHeartbeatTimeout = 60 * time.Second
	DefaultFloorThroughput  = 256 * 1024 // bytes/sec
	DefaultMaxDrain         = 15 * time.Minute
	DefaultBaseline         = 60 * time.Second
	DefaultTickInterval     = 2 * time.Second
	// DefaultPaceCheckAfter is how long the drain runs before the slow-pace
	// gate activates. Lets connections ramp up (TCP slow-start, rclone
	// chunk pipelining) before we judge sustained throughput.
	DefaultPaceCheckAfter = 2 * time.Minute
	// DefaultMinThroughputFraction is the fraction of FloorThroughput a
	// drain must sustain (in average bytes/sec since start) to avoid being
	// killed for uneconomic pace. 0.25 = 25% means at the default 256 KiB/s
	// floor, the drain must sustain at least 64 KiB/s avg or it aborts.
	DefaultMinThroughputFraction = 0.25
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
	// KilledBySlowPace: sustained throughput below MinThroughputFraction *
	// FloorThroughput. Distinct from KilledByStall so the failure marker
	// surfaces "this connection is uneconomically slow" rather than "wedged".
	KilledBySlowPace = "slow_pace"
)

// StallKind discriminates between the reasons a stall watchdog fires.
// Embedded in [Result.StallKind] when Status == StatusStalled.
type StallKind string

const (
	// StallKindNeverStarted: rclone emitted stats but the byte counter never
	// rose above zero. Typically auth/DNS/connectivity failure where the
	// HEAD/PUT-init handshake hangs.
	StallKindNeverStarted StallKind = "never_started"
	// StallKindMidTransfer: bytes were flowing, then stopped advancing for
	// longer than StallTimeout. Typically a transient network blip or a
	// provider rate-limiting the connection.
	StallKindMidTransfer StallKind = "mid_transfer"
	// StallKindHeartbeat: rclone stopped emitting stderr lines entirely.
	// The process is hung (kernel wedge, OOM, etc.), not merely slow.
	StallKindHeartbeat StallKind = "heartbeat"
	// StallKindSlowPace: sustained throughput far below the floor. Bytes
	// are flowing but at a rate that would burn substantial cloud-instance
	// $/hr before the ceiling fires. Abort so the autopilot can re-place on
	// a faster instance.
	StallKindSlowPace StallKind = "slow_pace"
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

	// StallTimeout: once bytes have started flowing, if no byte-progress in
	// this window, abort. Zero → default.
	StallTimeout time.Duration
	// InitialStallTimeout: before the first byte is observed, allow this
	// much time for rclone to complete its pre-transfer setup before we
	// declare a never-started stall. Zero → default.
	InitialStallTimeout time.Duration
	// HeartbeatTimeout: if rclone produces no stderr output at all for this
	// long, declare the process hung independent of byte progress. Zero →
	// default.
	HeartbeatTimeout time.Duration
	// FloorThroughput: bytes/sec used to compute the ceiling. Zero → default.
	FloorThroughput int64
	// MaxDrain: absolute ceiling regardless of size. Zero → default.
	MaxDrain time.Duration
	// Baseline added to the ceiling to cover rclone startup and final flush.
	// Zero → default.
	Baseline time.Duration
	// TickInterval: how often the watchdog wakes up. Zero → default.
	TickInterval time.Duration
	// PaceCheckAfter: do not enforce the slow-pace gate until the drain has
	// been running this long. Zero → default. Negative → disable pace check.
	PaceCheckAfter time.Duration
	// MinThroughputFraction: once PaceCheckAfter has elapsed, the drain must
	// sustain at least MinThroughputFraction * FloorThroughput bytes/sec on
	// average since start, or it aborts with [StallKindSlowPace]. Zero →
	// default. Negative → disable pace check.
	MinThroughputFraction float64

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
	// StallTimeoutSeconds is the steady-state stall watchdog window in effect.
	StallTimeoutSeconds float64 `json:"stall_timeout_seconds"`
	// InitialStallTimeoutSeconds is the pre-first-byte stall window in effect.
	InitialStallTimeoutSeconds float64 `json:"initial_stall_timeout_seconds"`
	// HeartbeatTimeoutSeconds is the rclone-silence threshold in effect.
	HeartbeatTimeoutSeconds float64 `json:"heartbeat_timeout_seconds"`
	// StallKind is one of [StallKindNeverStarted], [StallKindMidTransfer],
	// [StallKindHeartbeat], [StallKindSlowPace] when Status == StatusStalled.
	StallKind StallKind `json:"stall_kind,omitempty"`
	// ThroughputBytesPerSec is the average byte rate over the run. Populated
	// for completed and aborted runs alike — useful for diagnosing slow-pace
	// kills and benchmarking provider connectivity.
	ThroughputBytesPerSec float64 `json:"throughput_bytes_per_sec,omitempty"`

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
		BytesTotal:                 cfg.TotalBytes,
		StartedAtUnix:              started.Unix(),
		CeilingSeconds:             ceiling.Seconds(),
		StallTimeoutSeconds:        cfg.StallTimeout.Seconds(),
		InitialStallTimeoutSeconds: cfg.InitialStallTimeout.Seconds(),
		HeartbeatTimeoutSeconds:    cfg.HeartbeatTimeout.Seconds(),
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
		mu              sync.Mutex
		bytesUploaded   int64     // monotonic; bytesUploaded > 0 means bytes have ever flowed
		lastProgressAt  = started // last byte-count increase
		lastHeartbeatAt = started // last stderr line of any kind
		stderrTail      []string
	)

	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		scanner := bufio.NewScanner(proc.Stderr())
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			now := time.Now()
			mu.Lock()
			// Every line counts as a heartbeat — including 0-byte stats
			// lines during rclone's pre-transfer setup. Without this, a
			// 30s mid-transfer stall watchdog mis-fires on multi-GB uploads
			// where bytes haven't started flowing yet but rclone is alive.
			lastHeartbeatAt = now
			stderrTail = append(stderrTail, line)
			if len(stderrTail) > 20 {
				stderrTail = stderrTail[len(stderrTail)-20:]
			}
			if n, ok := extractBytes(line); ok && n > bytesUploaded {
				bytesUploaded = n
				lastProgressAt = now
			}
			mu.Unlock()
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- proc.Wait() }()

	ticker := time.NewTicker(cfg.TickInterval)
	defer ticker.Stop()
	deadline := started.Add(ceiling)

	finalize := func(status, killedBy string, stallKind StallKind, reason string, err error) Result {
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
		result.StallKind = stallKind
		result.Reason = reason
		result.Err = err
		ended := time.Now()
		result.EndedAtUnix = ended.Unix()
		elapsed := ended.Sub(started)
		result.ElapsedSeconds = elapsed.Seconds()
		if elapsed > 0 {
			result.ThroughputBytesPerSec = float64(result.BytesUploaded) / elapsed.Seconds()
		}
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
			elapsed := ended.Sub(started)
			result.ElapsedSeconds = elapsed.Seconds()
			if elapsed > 0 {
				result.ThroughputBytesPerSec = float64(result.BytesUploaded) / elapsed.Seconds()
			}
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
			heartbeatElapsed := now.Sub(lastHeartbeatAt)
			seen := bytesUploaded
			mu.Unlock()
			everFlowed := seen > 0

			if heartbeatElapsed > cfg.HeartbeatTimeout {
				reason := fmt.Sprintf(
					"rclone silent for %s (heartbeat timeout %s); uploaded %d/%d bytes",
					heartbeatElapsed.Truncate(time.Second), cfg.HeartbeatTimeout,
					seen, cfg.TotalBytes,
				)
				return finalize(StatusStalled, KilledByStall, StallKindHeartbeat, reason, nil)
			}

			if everFlowed && stallElapsed > cfg.StallTimeout {
				reason := fmt.Sprintf(
					"no byte progress for %s (mid-transfer stall timeout %s); uploaded %d/%d bytes",
					stallElapsed.Truncate(time.Second), cfg.StallTimeout,
					seen, cfg.TotalBytes,
				)
				return finalize(StatusStalled, KilledByStall, StallKindMidTransfer, reason, nil)
			}
			if !everFlowed && stallElapsed > cfg.InitialStallTimeout {
				reason := fmt.Sprintf(
					"no bytes uploaded after %s (initial stall timeout %s); 0/%d bytes",
					stallElapsed.Truncate(time.Second), cfg.InitialStallTimeout,
					cfg.TotalBytes,
				)
				return finalize(StatusStalled, KilledByStall, StallKindNeverStarted, reason, nil)
			}

			elapsed := now.Sub(started)
			if everFlowed && cfg.PaceCheckAfter > 0 && cfg.MinThroughputFraction > 0 && elapsed > cfg.PaceCheckAfter {
				avgBps := float64(seen) / elapsed.Seconds()
				threshold := float64(cfg.FloorThroughput) * cfg.MinThroughputFraction
				if avgBps < threshold {
					reason := fmt.Sprintf(
						"sustained slow throughput: %.0f B/s avg over %s (need %.0f B/s = %.0f%% of floor); uploaded %d/%d bytes",
						avgBps, elapsed.Truncate(time.Second),
						threshold, cfg.MinThroughputFraction*100,
						seen, cfg.TotalBytes,
					)
					return finalize(StatusStalled, KilledBySlowPace, StallKindSlowPace, reason, nil)
				}
			}

			if now.After(deadline) {
				reason := fmt.Sprintf(
					"ceiling %s reached; uploaded %d/%d bytes",
					ceiling.Truncate(time.Second), seen, cfg.TotalBytes,
				)
				return finalize(StatusCeiling, KilledByCeiling, "", reason, nil)
			}

		case <-ctx.Done():
			return finalize(StatusError, KilledByCanceled, "",
				"context canceled: "+ctx.Err().Error(), ctx.Err())
		}
	}
}

// applyDefaults returns a copy of opts with zero-valued fields filled in.
// Negative values for pace-check fields are preserved and disable the gate.
func applyDefaults(opts Options) Options {
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = DefaultStallTimeout
	}
	if opts.InitialStallTimeout <= 0 {
		opts.InitialStallTimeout = DefaultInitialStallTimeout
	}
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = DefaultHeartbeatTimeout
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
	if opts.PaceCheckAfter == 0 {
		opts.PaceCheckAfter = DefaultPaceCheckAfter
	}
	if opts.MinThroughputFraction == 0 {
		opts.MinThroughputFraction = DefaultMinThroughputFraction
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
