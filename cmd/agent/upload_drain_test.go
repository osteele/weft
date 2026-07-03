package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2upload"
)

// recordedPut captures one PUT against agentMarkerWriter so tests can
// assert what the agent tried to upload after a drain failure.
type recordedPut struct {
	key  string
	body []byte
}

type capturingMarkerWriter struct {
	mu    sync.Mutex
	calls []recordedPut
}

func (c *capturingMarkerWriter) Put(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.calls = append(c.calls, recordedPut{key: key, body: data})
	c.mu.Unlock()
	return nil
}

func TestApplyDrainSettingsOverrides(t *testing.T) {
	origStall, origFloor, origMax, origBase, origMarker :=
		drainStallTimeout, drainFloorThroughput, drainMaxDrain, drainBaseline, drainMarkerTimeout
	defer func() {
		drainStallTimeout = origStall
		drainFloorThroughput = origFloor
		drainMaxDrain = origMax
		drainBaseline = origBase
		drainMarkerTimeout = origMarker
	}()
	applyDrainSettings(cloud.DrainSettings{
		StallTimeoutSeconds:        45,
		FloorThroughputBytesPerSec: 1 << 20,
		MaxDrainSeconds:            1200,
		BaselineSeconds:            90,
		MarkerTimeoutSeconds:       15,
	})
	if got := drainStallTimeout.Seconds(); got != 45 {
		t.Errorf("drainStallTimeout = %v, want 45s", got)
	}
	if drainFloorThroughput != 1<<20 {
		t.Errorf("drainFloorThroughput = %d, want 1MiB", drainFloorThroughput)
	}
	if drainMaxDrain.Seconds() != 1200 {
		t.Errorf("drainMaxDrain = %v, want 1200s", drainMaxDrain)
	}
	if drainBaseline.Seconds() != 90 {
		t.Errorf("drainBaseline = %v, want 90s", drainBaseline)
	}
	if drainMarkerTimeout.Seconds() != 15 {
		t.Errorf("drainMarkerTimeout = %v, want 15s", drainMarkerTimeout)
	}
}

func TestApplyDrainSettingsLeavesDefaultsForZeros(t *testing.T) {
	origStall := drainStallTimeout
	applyDrainSettings(cloud.DrainSettings{})
	if drainStallTimeout != origStall {
		t.Errorf("zero settings clobbered drainStallTimeout: was %v, now %v", origStall, drainStallTimeout)
	}
}

func TestMeasureUploadTreeReportsRootWalkFailure(t *testing.T) {
	files, bytes, ok := measureUploadTree(filepath.Join(t.TempDir(), "missing"))
	if ok {
		t.Fatal("missing root should report measurement failure")
	}
	if files != 0 || bytes != 0 {
		t.Fatalf("files/bytes = %d/%d, want 0/0", files, bytes)
	}
}

func TestMeasureUploadTreeCountsFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("de"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	files, bytes, ok := measureUploadTree(dir)
	if !ok {
		t.Fatal("expected measurement success")
	}
	if files != 2 || bytes != 5 {
		t.Fatalf("files/bytes = %d/%d, want 2/5", files, bytes)
	}
}

func TestBytesForMaxDrain(t *testing.T) {
	opts := r2upload.Options{
		Baseline:        time.Minute,
		MaxDrain:        15 * time.Minute,
		FloorThroughput: 256 * 1024,
	}
	got := bytesForMaxDrain(opts)
	want := int64((14 * time.Minute) / time.Second * 256 * 1024)
	if got != want {
		t.Fatalf("bytesForMaxDrain = %d, want %d", got, want)
	}
}

func TestFailureMarkerKey(t *testing.T) {
	jobKey := failureMarkerKey(drainTarget{JobID: 42, RunID: 7})
	if want := r2keys.JobAttemptUploadFailure(42, 7); jobKey != want {
		t.Errorf("job marker key = %q, want %q", jobKey, want)
	}
	instKey := failureMarkerKey(drainTarget{InstanceID: 99})
	if want := r2keys.InstanceUploadFailure(99); instKey != want {
		t.Errorf("instance marker key = %q, want %q", instKey, want)
	}
}

func TestRecordDrainOutcomeSerializesFailureMarker(t *testing.T) {
	// Round-trip: serialize and decode to confirm the wire shape.
	r := r2upload.Result{
		Status:         r2upload.StatusStalled,
		KilledBy:       r2upload.KilledByStall,
		StallKind:      r2upload.StallKindMidTransfer,
		Reason:         "no progress for 30s",
		BytesUploaded:  123456,
		BytesTotal:     1000000,
		ElapsedSeconds: 31.5,
		StderrTail:     "rclone: tail",
	}
	m := r2upload.NewFailureMarker(r)
	m.JobID = 11
	m.RunID = 22
	m.SourcePath = "/tmp/logs"
	m.DestRemote = "r2:bucket/jobs/11/runs/22/results/"

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got r2upload.FailureMarker
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != r2upload.StatusStalled {
		t.Errorf("Status = %q, want %q", got.Status, r2upload.StatusStalled)
	}
	if got.KilledBy != r2upload.KilledByStall {
		t.Errorf("KilledBy = %q, want %q", got.KilledBy, r2upload.KilledByStall)
	}
	if got.JobID != 11 || got.RunID != 22 {
		t.Errorf("JobID/RunID = %d/%d, want 11/22", got.JobID, got.RunID)
	}
	if !strings.Contains(got.Reason, "no progress") {
		t.Errorf("Reason = %q, want 'no progress' phrase", got.Reason)
	}
	if got.Kind != r2upload.StallKindMidTransfer {
		t.Errorf("Kind = %q, want %q (StallKind must survive the marker round-trip)",
			got.Kind, r2upload.StallKindMidTransfer)
	}
}

// orderRecordingWriter appends "marker" to the shared event log on every PUT
// so tests can assert ordering against the self-destruct hook.
type orderRecordingWriter struct {
	events *[]string
}

func (w orderRecordingWriter) Put(_ context.Context, _ string, body io.Reader, _ string) error {
	if _, err := io.ReadAll(body); err != nil {
		return err
	}
	*w.events = append(*w.events, "marker")
	return nil
}

// Regression: the failure marker must be written BEFORE self-destruct is
// initiated — if teardown wins the race, the marker is lost and the CLI can
// never explain why the instance died.
func TestRecordDrainOutcomeWritesMarkerBeforeSelfDestruct(t *testing.T) {
	origTracker := uploadHealth
	origHook := uploadStallSelfDestructHook
	origWriter := newDrainMarkerWriter
	t.Cleanup(func() {
		uploadHealth = origTracker
		uploadStallSelfDestructHook = origHook
		newDrainMarkerWriter = origWriter
	})

	var events []string
	newDrainMarkerWriter = func(string) r2upload.MarkerWriter {
		return orderRecordingWriter{events: &events}
	}
	uploadStallSelfDestructHook = func(string, int64, string, string, int64, string) {
		events = append(events, "self-destruct")
	}
	uploadHealth = newTestTracker(1, 0) // fire on the first stall

	stall := r2upload.Result{
		Status: r2upload.StatusStalled, KilledBy: r2upload.KilledByStall,
		StallKind: r2upload.StallKindNeverStarted, Reason: "never started",
	}
	recordDrainOutcome("bucket", drainTarget{JobID: 7, RunID: 1, Label: "order"},
		r2upload.Options{Source: "/tmp/z/", DestRemote: "r2:bucket/z/"}, stall)

	want := []string{"marker", "self-destruct"}
	if len(events) != 2 || events[0] != want[0] || events[1] != want[1] {
		t.Fatalf("event order = %v, want %v", events, want)
	}
}

// newTestTracker returns an uploadHealthTracker tuned for fast tests:
// fire on 3 stalls and 0 elapsed (gate only on count, not wall-clock).
func newTestTracker(stalls int, sinceSuccess time.Duration) *uploadHealthTracker {
	return &uploadHealthTracker{
		lastSuccessAt:   time.Now(),
		minStalls:       stalls,
		minSinceSuccess: sinceSuccess,
	}
}

func TestUploadHealthTrackerDoesNotFireBelowStallThreshold(t *testing.T) {
	tr := newTestTracker(5, 0)
	for i := 1; i < 5; i++ {
		d := tr.recordStall()
		if d.shouldSelfDestruct {
			t.Fatalf("stall #%d fired self-destruct prematurely (consecutive=%d)", i, d.consecutiveStalls)
		}
	}
}

func TestUploadHealthTrackerDoesNotFireWithinMinSinceSuccess(t *testing.T) {
	// Require 3 stalls AND 1 hour since last success. Tracker just started,
	// so even after 100 stalls it shouldn't fire.
	tr := newTestTracker(3, time.Hour)
	for i := 0; i < 100; i++ {
		d := tr.recordStall()
		if d.shouldSelfDestruct {
			t.Fatalf("fired despite sinceLastSuccess=%v < threshold %v",
				d.sinceLastSuccess, time.Hour)
		}
	}
}

func TestUploadHealthTrackerFiresWhenBothThresholdsMet(t *testing.T) {
	tr := newTestTracker(3, 0)
	// First 2 stalls: no fire.
	for i := 1; i < 3; i++ {
		if d := tr.recordStall(); d.shouldSelfDestruct {
			t.Fatalf("stall #%d fired prematurely", i)
		}
	}
	// Third stall: fires.
	d := tr.recordStall()
	if !d.shouldSelfDestruct {
		t.Fatalf("third stall did not fire: %+v", d)
	}
	// Fourth and later: do not re-fire (single-shot).
	d2 := tr.recordStall()
	if d2.shouldSelfDestruct {
		t.Fatal("self-destruct fired more than once")
	}
}

func TestUploadHealthTrackerSuccessResetsConsecutiveStalls(t *testing.T) {
	tr := newTestTracker(3, 0)
	tr.recordStall()
	tr.recordStall()
	tr.recordSuccess()
	d := tr.recordStall()
	if d.consecutiveStalls != 1 {
		t.Errorf("consecutiveStalls = %d, want 1 (success should reset)", d.consecutiveStalls)
	}
	if d.shouldSelfDestruct {
		t.Error("self-destruct fired after success reset")
	}
}

func TestRecordDrainOutcomeStallTriggersSelfDestruct(t *testing.T) {
	// Save and restore package-level state.
	origTracker := uploadHealth
	origHook := uploadStallSelfDestructHook
	origMarkerTimeout := drainMarkerTimeout
	t.Cleanup(func() {
		uploadHealth = origTracker
		uploadStallSelfDestructHook = origHook
		drainMarkerTimeout = origMarkerTimeout
		setUploadStallSelfDestructContext(0, "", nil)
	})

	// 1ms marker timeout so the marker-write side effect (which shells out
	// to rclone) bails out immediately instead of blocking the test.
	drainMarkerTimeout = time.Millisecond
	uploadHealth = newTestTracker(2, 0)
	setUploadStallSelfDestructContext(4242, "echo stub", func() string { return "running:99" })

	var hookCalls []string
	uploadStallSelfDestructHook = func(bucket string, instanceID int64, selfDestructCmd, phase string, jobID int64, lastError string) {
		hookCalls = append(hookCalls, lastError)
		if instanceID != 4242 {
			t.Errorf("instanceID = %d, want 4242", instanceID)
		}
		if phase != "running:99" {
			t.Errorf("phase = %q, want running:99", phase)
		}
	}

	stall := r2upload.Result{
		Status: r2upload.StatusStalled, KilledBy: r2upload.KilledByStall,
		Reason: "no progress for 30s", BytesUploaded: 0, BytesTotal: 1000000,
	}
	target := drainTarget{JobID: 99, RunID: 1, Label: "test"}
	opts := r2upload.Options{Source: "/tmp/x/", DestRemote: "r2:bucket/x/"}

	// First stall: no fire.
	recordDrainOutcome("bucket", target, opts, stall)
	if len(hookCalls) != 0 {
		t.Fatalf("first stall fired hook prematurely: %v", hookCalls)
	}
	// Second stall: fires.
	recordDrainOutcome("bucket", target, opts, stall)
	if len(hookCalls) != 1 {
		t.Fatalf("second stall did not fire hook (got %d calls)", len(hookCalls))
	}
	if !strings.Contains(hookCalls[0], "2 consecutive") {
		t.Errorf("lastError missing consecutive count: %q", hookCalls[0])
	}
	// Third stall: does not re-fire.
	recordDrainOutcome("bucket", target, opts, stall)
	if len(hookCalls) != 1 {
		t.Fatalf("third stall re-fired hook (got %d calls, want 1)", len(hookCalls))
	}
}

func TestRecordDrainOutcomeOnlyStallsCount(t *testing.T) {
	// A non-stall failure (error, ceiling, etc.) should NOT advance the
	// upload-stall counter — only stalls do.
	origTracker := uploadHealth
	origHook := uploadStallSelfDestructHook
	origMarkerTimeout := drainMarkerTimeout
	t.Cleanup(func() {
		uploadHealth = origTracker
		uploadStallSelfDestructHook = origHook
		drainMarkerTimeout = origMarkerTimeout
	})

	drainMarkerTimeout = time.Millisecond
	uploadHealth = newTestTracker(1, 0)
	fired := false
	uploadStallSelfDestructHook = func(string, int64, string, string, int64, string) {
		fired = true
	}

	errResult := r2upload.Result{
		Status: r2upload.StatusError, KilledBy: "", Reason: "rclone error",
	}
	recordDrainOutcome("bucket", drainTarget{JobID: 1}, r2upload.Options{
		Source: "/tmp/y/", DestRemote: "r2:bucket/y/",
	}, errResult)
	if fired {
		t.Error("non-stall error fired upload-stall self-destruct")
	}
}
