package ops

import (
	"testing"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
)

func writeCu128Project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dataloc.WriteTestTorchPin(t, dir, "2.9.1", "cu128")
	return dir
}

// Regression (wb18): refreshing a CPU-only job in a torch-pinned project
// must not persist (and must clear) the inert torch-derived arch cap.
func TestRefreshProjectDerivedMetadata_CapOnlyForGPUJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	dir := writeCu128Project(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", dir, "uv run train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	// Simulate a stale cap persisted before the job became CPU-only.
	if err := db.SetJobMaxComputeCap(database, jobID, "12.0"); err != nil {
		t.Fatalf("SetJobMaxComputeCap: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.RequestsGPU() {
		t.Fatal("fixture job unexpectedly requests a GPU")
	}

	if err := RefreshProjectDerivedMetadata(database, job); err != nil {
		t.Fatalf("RefreshProjectDerivedMetadata: %v", err)
	}
	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if refreshed.MaxComputeCap != "" {
		t.Errorf("MaxComputeCap = %q after CPU-job refresh, want cleared", refreshed.MaxComputeCap)
	}

	// A GPU job in the same project still gets the cap.
	gpuJobID, err := db.RecordQueuedWithGPU(database, "", dir, "uv run train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobGPUClass(database, gpuJobID, "nvidia"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	gpuJob, err := db.GetJobByID(database, gpuJobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if err := RefreshProjectDerivedMetadata(database, gpuJob); err != nil {
		t.Fatalf("RefreshProjectDerivedMetadata: %v", err)
	}
	gpuRefreshed, err := db.GetJobByID(database, gpuJobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if gpuRefreshed.MaxComputeCap != "12.0" {
		t.Errorf("MaxComputeCap = %q for GPU job, want %q", gpuRefreshed.MaxComputeCap, "12.0")
	}
}
