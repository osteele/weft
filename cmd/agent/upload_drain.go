package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
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
		uploadHealth.recordSuccess()
		return
	}
	if result.KilledBy == r2upload.KilledByStall {
		if decision := uploadHealth.recordStall(); decision.shouldSelfDestruct {
			triggerUploadStallSelfDestruct(bucket, target, decision, result)
		}
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

// Upload-stall self-destruct: when R2 uploads stall repeatedly with no
// successful traffic in between, this instance has lost effective R2
// connectivity. The wrapper writes failure markers (best-effort, also via
// R2), but those markers may never reach the coordinator. Rather than wait
// for the heartbeat-staleness reconciler to notice (which can take hours),
// the agent triggers its own teardown so the coordinator can place jobs on
// a fresh instance with working network.
//
// Trigger policy (both must be true):
//   - At least uploadStallMinStalls drain calls in a row have ended in
//     stall (KilledByStall), with no successful upload between them.
//   - Time since the last successful upload (or since this tracker was
//     constructed) exceeds uploadStallMinSinceSuccess.
//
// Both gates exist because either alone would mis-fire: a single stall
// burst from a transient blip shouldn't tear down the instance, and a long
// idle period with no uploads (e.g. a job that hasn't produced output yet)
// shouldn't either.
const (
	uploadStallMinStalls       = 5
	uploadStallMinSinceSuccess = 30 * time.Minute
)

type uploadHealthDecision struct {
	consecutiveStalls  int
	sinceLastSuccess   time.Duration
	shouldSelfDestruct bool
}

type uploadHealthTracker struct {
	mu                sync.Mutex
	consecutiveStalls int
	lastSuccessAt     time.Time
	selfDestructFired atomic.Bool

	// Test seams.
	minStalls       int
	minSinceSuccess time.Duration
}

func newUploadHealthTracker() *uploadHealthTracker {
	return &uploadHealthTracker{
		lastSuccessAt:   time.Now(),
		minStalls:       uploadStallMinStalls,
		minSinceSuccess: uploadStallMinSinceSuccess,
	}
}

func (t *uploadHealthTracker) recordSuccess() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consecutiveStalls = 0
	t.lastSuccessAt = time.Now()
}

func (t *uploadHealthTracker) recordStall() uploadHealthDecision {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consecutiveStalls++
	since := time.Since(t.lastSuccessAt)
	d := uploadHealthDecision{
		consecutiveStalls: t.consecutiveStalls,
		sinceLastSuccess:  since,
	}
	if t.consecutiveStalls >= t.minStalls && since >= t.minSinceSuccess {
		// CompareAndSwap ensures only the first qualifying caller triggers
		// teardown; later calls in a flurry of stalls are no-ops.
		if t.selfDestructFired.CompareAndSwap(false, true) {
			d.shouldSelfDestruct = true
		}
	}
	return d
}

// uploadHealth is the package-level tracker shared by all upload sites.
// Reset for tests via resetUploadHealth.
var uploadHealth = newUploadHealthTracker()

func resetUploadHealth() { uploadHealth = newUploadHealthTracker() }

// uploadStallSelfDestructContext is set once at agent startup so the
// drain layer can call into the self-destruct path without threading
// instance state through every upload call site.
var (
	uploadStallCtxMu       sync.RWMutex
	uploadStallInstanceID  int64
	uploadStallSelfDestCmd string
	uploadStallPhaseGetter func() string
)

// setUploadStallSelfDestructContext wires the agent's instance identity and
// self-destruct command into the upload-drain layer. Called once near the
// top of runInstance, before any uploads can run.
func setUploadStallSelfDestructContext(instanceID int64, selfDestructCmd string, phase func() string) {
	uploadStallCtxMu.Lock()
	defer uploadStallCtxMu.Unlock()
	uploadStallInstanceID = instanceID
	uploadStallSelfDestCmd = selfDestructCmd
	uploadStallPhaseGetter = phase
}

func uploadStallContext() (int64, string, func() string) {
	uploadStallCtxMu.RLock()
	defer uploadStallCtxMu.RUnlock()
	return uploadStallInstanceID, uploadStallSelfDestCmd, uploadStallPhaseGetter
}

// uploadStallSelfDestructHook lets tests intercept the terminate call.
// Production code leaves this nil and the real terminate path runs.
var uploadStallSelfDestructHook func(bucket string, instanceID int64, selfDestructCmd, phase string, jobID int64, lastError string)

func triggerUploadStallSelfDestruct(bucket string, target drainTarget, decision uploadHealthDecision, result r2upload.Result) {
	instanceID, selfDestructCmd, phaseGetter := uploadStallContext()
	if instanceID == 0 {
		instanceID = target.InstanceID
	}
	phase := "upload_stall"
	if phaseGetter != nil {
		if p := phaseGetter(); p != "" {
			phase = p
		}
	}
	lastError := fmt.Sprintf("persistent R2 upload stalls: %d consecutive, no successful upload for %s (last: %s %s, bytes=%d/%d)",
		decision.consecutiveStalls,
		decision.sinceLastSuccess.Round(time.Second),
		result.Status, result.KilledBy,
		result.BytesUploaded, result.BytesTotal)
	slog.Warn("upload stall threshold exceeded; triggering instance self-destruct",
		"consecutive_stalls", decision.consecutiveStalls,
		"since_last_success", decision.sinceLastSuccess.Round(time.Second),
		"instance_id", instanceID,
		"phase", phase,
		"job_id", target.JobID,
	)
	oplog.Log(oplog.OpPhaseTransition,
		oplog.WithJobID(target.JobID),
		oplog.WithDetailf("upload_stall_self_destruct stalls=%d since_success=%s",
			decision.consecutiveStalls, decision.sinceLastSuccess.Round(time.Second)),
	)
	if hook := uploadStallSelfDestructHook; hook != nil {
		hook(bucket, instanceID, selfDestructCmd, phase, target.JobID, lastError)
		return
	}
	if selfDestructCmd == "" {
		// Not configured (non-cloud agent, tests, etc.). The marker has
		// already been written by recordDrainOutcome's caller; nothing more
		// to do here.
		slog.Warn("upload stall self-destruct skipped: no self-destruct command configured",
			"instance_id", instanceID)
		return
	}
	terminateInstanceWithReason(bucket, instanceID, selfDestructCmd, phase, target.JobID, db.TerminationReasonUploadStall, lastError)
}

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
