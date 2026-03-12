package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestExportTrainingDataIncludesHostSpecsAndRunTiming(t *testing.T) {
	database := db.SetupTestDB(t)

	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "cool30",
		CPUCount:    32,
		CPUModel:    "AMD Ryzen",
		CPUFreq:     "3.5 GHz",
		MemTotal:    "128G",
		GPUsJSON:    `[{"Name":"RTX 3090","MemTotal":"24576MiB"}]`,
		LastUpdated: 1234,
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	jobID, err := db.RecordQueued(database, "cool30", "/tmp/proj", "python train.py --epochs 10", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobProject(database, jobID, "proj"); err != nil {
		t.Fatalf("SetJobProject: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "3090"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	meta := &db.JobMetadata{
		Resource: &db.ResourceUsage{
			PeakRSSKB:    int64Ptr(123456),
			MaxGPUMemMiB: int64Ptr(8192),
		},
		CPU: &db.JobCPUStats{Mean: float64Ptr(12.5)},
	}
	if err := db.SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}
	if err := db.RecordCompletionByID(database, jobID, 0, job.StartTime+42); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	oldOutput := exportOutput
	oldSince := exportSince
	exportOutput = filepath.Join(t.TempDir(), "training-data.jsonl")
	exportSince = ""
	t.Cleanup(func() {
		exportOutput = oldOutput
		exportSince = oldSince
	})

	if err := runExportTrainingData(exportTrainingDataCmd, nil); err != nil {
		t.Fatalf("runExportTrainingData: %v", err)
	}

	payload, err := os.ReadFile(exportOutput)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	if len(lines) != 1 {
		t.Fatalf("line count = %d, want 1", len(lines))
	}

	var rec trainingDataRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if rec.Host != "cool30" || rec.Command != "python train.py --epochs 10" {
		t.Fatalf("record identity = %+v", rec)
	}
	if rec.WorkingDir != "/tmp/proj" || rec.Project != "proj" || rec.GPUClass != "3090" {
		t.Fatalf("record metadata = %+v", rec)
	}
	if rec.StartTime == 0 || rec.EndTime == 0 || rec.DurationS != 42 {
		t.Fatalf("record timing = %+v", rec)
	}
	if rec.PeakRSSKB != 123456 || rec.MaxGPUMiB != 8192 || rec.CPUMean != 12.5 {
		t.Fatalf("record resources = %+v", rec)
	}
	if rec.HostSpecs == nil {
		t.Fatalf("host specs missing")
	}
	if rec.HostSpecs.CPUCount != 32 || rec.HostSpecs.MemTotal != "128G" {
		t.Fatalf("host specs = %+v", rec.HostSpecs)
	}
	if len(rec.HostSpecs.GPUNames) != 1 || rec.HostSpecs.GPUNames[0] != "RTX 3090" {
		t.Fatalf("gpu names = %+v", rec.HostSpecs)
	}
	if rec.HostSpecs.GPUVRAMPerDeviceMiB != 24576 || rec.HostSpecs.GPUVRAMTotalMiB != 24576 {
		t.Fatalf("gpu vram = %+v", rec.HostSpecs)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

func float64Ptr(v float64) *float64 {
	return &v
}
