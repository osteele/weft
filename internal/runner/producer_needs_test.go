package runner

import (
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/opsqueue"
)

// A producer need the controller resolved into a stageable ArtifactNeed is
// fetched from R2 by this runner before the job starts. Keying the stageable
// bypass to named assets stranded producer needs: they waited on a marker that
// only the producer's own runner writes, on a different host, so the job never
// started. wb137.
func TestCheckDependencies_ProducerNeedIsStageable(t *testing.T) {
	logDir := t.TempDir()
	spec := "outputs/result.tar:8144"
	stageable := []opsqueue.ArtifactNeed{{Spec: spec, Path: "outputs/result.tar", R2Key: "jobs/8144/x"}}

	got := checkDependencies("", []string{spec}, stageable, logDir)
	if got.Result != DepOK {
		t.Fatalf("stageable producer need: got %v, want DepOK — the runner is waiting on a marker for an artifact it will fetch itself", got)
	}

	// Without the controller resolving it, the marker path still governs.
	if got := checkDependencies("", []string{spec}, nil, logDir); got.Result != DepWaiting {
		t.Fatalf("unstageable producer need: got %v, want DepWaiting", got)
	}
}

// The marker writer refused any non-asset spec, which meant a producer need
// that did reach staging failed at the point of recording its success.
func TestWriteArtifactNeedSatisfiedMarkers_ProducerNeed(t *testing.T) {
	logDir := t.TempDir()
	needs := []opsqueue.ArtifactNeed{
		{Spec: "outputs/result.tar:8144", Path: "outputs/result.tar"},
		{Spec: "asset:weights", Path: "weights.bin"},
	}
	if err := writeArtifactNeedSatisfiedMarkers(logDir, needs); err != nil {
		t.Fatalf("write markers: %v", err)
	}
	for _, want := range []string{
		ArtifactSatisfiedFile(logDir, "outputs/result.tar", 8144),
		NamedAssetSatisfiedFile(logDir, "weights"),
	} {
		if code, found := ReadStatusFile(want); !found || code != 0 {
			t.Errorf("marker %s: found=%v code=%d", filepath.Base(want), found, code)
		}
	}
}

// R2-pull reconciliation can publish the same add more than once before the
// controller observes runner state. The attempt identity must remain one queue
// item so staging retries cannot execute the consumer twice.
func TestProducerNeedDuplicateDispatchQueuesOneAttempt(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	job := &opsqueue.CommandJob{
		ID: 9001, RunID: 7001, Dir: "/tmp/consumer", Cmd: "cat outputs/result.tar",
		Needs: []string{"outputs/result.tar:8144"},
		ArtifactNeeds: []opsqueue.ArtifactNeed{{
			Spec: "outputs/result.tar:8144", Path: "outputs/result.tar",
			R2Key: "jobs/8144/runs/7000/outputs/outputs/result.tar",
		}},
	}
	for _, timestamp := range []string{"2026-09-16T00:00:00Z", "2026-09-16T00:00:01Z"} {
		appendCmd(t, cmdFile, opsqueue.QueueCommand{Timestamp: timestamp, Op: opsqueue.OpAdd, Job: job})
	}

	state := NewState()
	cp := NewCommandProcessor(cmdFile, dir, dir)
	if _, err := cp.ProcessCommands(state); err != nil {
		t.Fatalf("ProcessCommands: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0] != job.ID {
		t.Fatalf("duplicate attempt queued as %v, want one pending job %d", state.Pending, job.ID)
	}
}
