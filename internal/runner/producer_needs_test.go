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
