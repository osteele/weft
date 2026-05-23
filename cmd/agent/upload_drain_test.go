package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

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
}
