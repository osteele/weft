package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2keys"
)

func withKillRequestObjects(t *testing.T, objects map[string]string) {
	t.Helper()
	previousGet, previousDelete := killR2Get, killR2Delete
	killR2Get = func(_ string, key string) (string, error) { return objects[key], nil }
	killR2Delete = func(_ string, key string) error {
		delete(objects, key)
		return nil
	}
	t.Cleanup(func() { killR2Get, killR2Delete = previousGet, previousDelete })
}

func TestConsumeJobKillRequestKeepsConcurrentJobsDistinct(t *testing.T) {
	objects := map[string]string{
		r2keys.InstanceKillJobRequest(42, 10): "10",
		r2keys.InstanceKillJobRequest(42, 11): "11",
	}
	withKillRequestObjects(t, objects)

	for _, jobID := range []int64{10, 11} {
		requested, err := consumeJobKillRequest("bucket", 42, jobID)
		if err != nil {
			t.Fatalf("consume job %d kill request: %v", jobID, err)
		}
		if !requested {
			t.Fatalf("job %d kill request was lost", jobID)
		}
	}
}

func TestConsumeJobKillRequestSupportsLegacyMailbox(t *testing.T) {
	objects := map[string]string{r2keys.InstanceKillJob(42): "10"}
	withKillRequestObjects(t, objects)

	requested, err := consumeJobKillRequest("bucket", 42, 10)
	if err != nil || !requested {
		t.Fatalf("legacy kill request = (%v, %v), want (true, nil)", requested, err)
	}
}

func TestRunJobSequenceSkipsKillRequestedBeforeStart(t *testing.T) {
	objects := map[string]string{r2keys.InstanceKillJobRequest(42, 10): "10"}
	withKillRequestObjects(t, objects)
	marker := filepath.Join(t.TempDir(), "command-ran")

	result := runJobSequence([]cloud.AgentJob{{
		ID: 10, RunID: 100, Dir: t.TempDir(), Command: "touch " + marker,
	}}, jobSequenceConfig{
		R2Bucket: "bucket", InstanceID: 42, LogDir: t.TempDir(),
		StartTime: time.Now(), DisableJobPolling: true, SkipWorkdirDeletion: true,
	})

	if !result.AnyCanceled || result.StartedJobCount != 0 {
		t.Fatalf("sequence result = %+v, want canceled without a start", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("killed job command ran before agent observed its request: stat error %v", err)
	}
}
