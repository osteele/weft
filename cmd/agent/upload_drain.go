package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2upload"
	"github.com/osteele/weft/internal/retry"
)

// drainTarget identifies what is being uploaded and where the failure
// marker (if any) should be written. Exactly one of {JobID,RunID} pair or
// InstanceOnly should be set.
type drainTarget struct {
	JobID      int64
	RunID      int64
	InstanceID int64 // for the instance-level failure marker
	Label      string
}

// drainAndMark wraps r2upload.Drain so every upload site shares the same
// stall-watchdog + ceiling policy and the same best-effort failure marker.
// One-shot — no retries. Use [drainAndMarkWithRetry] for the post-job
// background path where transient R2 errors can be retried with backoff.
//
// On any non-ok status the function PUTs an upload-failure.json marker to
// R2 using a separate, short context. The marker write has no Drain
// semantics behind it — that's the invariant the user asked for: the
// marker writer never recurses into another drain.
func drainAndMark(ctx context.Context, bucket string, target drainTarget, opts r2upload.Options) r2upload.Result {
	if opts.Source == "" || opts.DestRemote == "" {
		return r2upload.Result{Status: r2upload.StatusError, Reason: "drainAndMark: empty source or dest"}
	}
	result := r2upload.Drain(ctx, opts)
	recordDrainOutcome(bucket, target, opts, result)
	return result
}

// drainAndMarkWithRetry calls Drain inside the agent's standard retry
// envelope (3 attempts, 5s then 10s backoff). The failure marker reflects
// the final attempt's outcome, not the per-attempt status — agents that
// recover on retry leave no marker behind.
func drainAndMarkWithRetry(ctx context.Context, bucket string, target drainTarget, opts r2upload.Options) r2upload.Result {
	if opts.Source == "" || opts.DestRemote == "" {
		return r2upload.Result{Status: r2upload.StatusError, Reason: "drainAndMarkWithRetry: empty source or dest"}
	}
	var result r2upload.Result
	_ = retry.Do(ctx, retry.ExplicitDelays(5*time.Second, 10*time.Second), func() error {
		result = r2upload.Drain(ctx, opts)
		if result.Status != r2upload.StatusOK {
			return fmt.Errorf("drain %s: %s", result.Status, result.Reason)
		}
		return nil
	}, retry.WithOnRetry(func(attempt int, err error, delay time.Duration) {
		fmt.Fprintf(os.Stderr, "upload %s (attempt %d/3): %v; retrying in %s\n",
			target.Label, attempt, err, delay)
	}))
	recordDrainOutcome(bucket, target, opts, result)
	return result
}

// recordDrainOutcome logs and (on failure) writes the upload-failure marker.
// Shared between the one-shot and retry variants.
func recordDrainOutcome(bucket string, target drainTarget, opts r2upload.Options, result r2upload.Result) {
	if result.Status == r2upload.StatusOK {
		return
	}
	slog.Warn("upload drain failed",
		"label", target.Label,
		"status", result.Status,
		"killed_by", result.KilledBy,
		"reason", result.Reason,
		"bytes_uploaded", result.BytesUploaded,
		"bytes_total", result.BytesTotal,
		"elapsed_seconds", result.ElapsedSeconds,
	)
	oplog.Log(oplog.OpR2Copy,
		oplog.WithJobID(target.JobID),
		oplog.WithDetailf("drain %s status=%s killed_by=%s bytes=%d/%d",
			target.Label, result.Status, result.KilledBy,
			result.BytesUploaded, result.BytesTotal),
	)

	marker := r2upload.NewFailureMarker(result)
	marker.JobID = target.JobID
	marker.RunID = target.RunID
	marker.InstanceID = target.InstanceID
	marker.SourcePath = opts.Source
	marker.DestRemote = opts.DestRemote

	key := failureMarkerKey(target)
	markerCtx, cancel := context.WithTimeout(context.Background(), drainMarkerTimeout)
	defer cancel()
	if err := r2upload.WriteFailureMarker(markerCtx, agentMarkerWriter{bucket: bucket}, key, marker, drainMarkerTimeout); err != nil {
		// Best effort. We've already logged the underlying drain failure;
		// surface only that the marker itself didn't land.
		fmt.Fprintf(os.Stderr, "write upload-failure marker (%s): %v\n", key, err)
	}
}

func failureMarkerKey(t drainTarget) string {
	if t.JobID > 0 {
		return r2keys.JobAttemptUploadFailure(t.JobID, t.RunID)
	}
	return r2keys.InstanceUploadFailure(t.InstanceID)
}

// agentMarkerWriter adapts r2Put (rclone rcat-based) into the
// r2upload.MarkerWriter interface.
type agentMarkerWriter struct {
	bucket string
}

func (w agentMarkerWriter) Put(ctx context.Context, key string, body io.Reader, _ string) error {
	// r2Put has its own bounded timeout via r2Timeout. The ctx passed in
	// here is honored only for early cancellation: if it's already done
	// when we start, bail out.
	if err := ctx.Err(); err != nil {
		return err
	}
	return r2PutReader(w.bucket, key, body)
}

// drainOptionsFromConfig fills [r2upload.Options] timing fields from the
// cloud drain config. Source/Dest/Command/Extra/TotalBytes remain caller's
// responsibility.
func drainOptionsFromConfig() r2upload.Options {
	return r2upload.Options{
		StallTimeout:    drainStallTimeout,
		FloorThroughput: drainFloorThroughput,
		MaxDrain:        drainMaxDrain,
		Baseline:        drainBaseline,
	}
}

// drain tunables populated at agent startup from the manifest's DrainSettings
// (zero fields fall back to the r2upload package defaults). Mutated only
// from applyDrainSettings before the job loop starts; goroutine-safe by
// virtue of being immutable after that.
var (
	drainStallTimeout    = r2upload.DefaultStallTimeout
	drainFloorThroughput = int64(r2upload.DefaultFloorThroughput)
	drainMaxDrain        = r2upload.DefaultMaxDrain
	drainBaseline        = r2upload.DefaultBaseline
	drainMarkerTimeout   = r2upload.DefaultMarkerTimeout
)

// applyDrainSettings overrides the package-level drain tunables from a
// manifest. Each zero/unset field leaves its current value (the default)
// in place.
func applyDrainSettings(s cloud.DrainSettings) {
	if s.StallTimeoutSeconds > 0 {
		drainStallTimeout = time.Duration(s.StallTimeoutSeconds) * time.Second
	}
	if s.FloorThroughputBytesPerSec > 0 {
		drainFloorThroughput = s.FloorThroughputBytesPerSec
	}
	if s.MaxDrainSeconds > 0 {
		drainMaxDrain = time.Duration(s.MaxDrainSeconds) * time.Second
	}
	if s.BaselineSeconds > 0 {
		drainBaseline = time.Duration(s.BaselineSeconds) * time.Second
	}
	if s.MarkerTimeoutSeconds > 0 {
		drainMarkerTimeout = time.Duration(s.MarkerTimeoutSeconds) * time.Second
	}
}
