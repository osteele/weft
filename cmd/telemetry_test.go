package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestCollectTelemetryOutputUsesLatestRun(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}

	oldTimeseries := []db.TimeseriesSample{{
		Ts:             1000,
		GPUUtilPct:     95,
		GPUMemUsedMiB:  900,
		GPUMemTotalMiB: 1000,
		GPUTempC:       88,
		GPUClockMHz:    1800,
	}}
	if err := db.InsertTimeseries(database, jobID, oldTimeseries); err != nil {
		t.Fatalf("InsertTimeseries(old): %v", err)
	}
	oldTelemetry := []db.TelemetrySample{{
		Ts:        1000,
		ElapsedS:  0,
		ProcRSSKB: 5000,
		GPUs: []db.TelemetryGPUSample{{
			GPUIndex:      "0",
			GPUName:       "A100",
			GPUMemUsedMiB: 900,
			GPUUtilPct:    float64PtrTelemetry(95),
		}},
	}}
	if err := db.InsertTelemetrySamples(database, jobID, oldTelemetry); err != nil {
		t.Fatalf("InsertTelemetrySamples(old): %v", err)
	}
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, nil, 1100); err != nil {
		t.Fatalf("CloseAttempt(old): %v", err)
	}

	newAttemptID, err := db.CreateAttempt(database, jobID, "host1", nil, db.StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning(new): %v", err)
	}

	newTimeseries := []db.TimeseriesSample{{
		Ts:             2000,
		GPUUtilPct:     40,
		GPUMemUsedMiB:  120,
		GPUMemTotalMiB: 1000,
		GPUTempC:       65,
		GPUClockMHz:    1200,
	}}
	if err := db.InsertTimeseries(database, jobID, newTimeseries); err != nil {
		t.Fatalf("InsertTimeseries(new): %v", err)
	}
	newTelemetry := []db.TelemetrySample{
		{
			Ts:        2000,
			ElapsedS:  0,
			ProcRSSKB: 1000,
			GPUs: []db.TelemetryGPUSample{{
				GPUIndex:      "0",
				GPUName:       "H100",
				GPUMemUsedMiB: 120,
				GPUUtilPct:    float64PtrTelemetry(40),
				GPUMemUtilPct: float64PtrTelemetry(12),
			}},
		},
		{
			Ts:        2001,
			ElapsedS:  1,
			ProcRSSKB: 1100,
			GPUs: []db.TelemetryGPUSample{{
				GPUIndex:      "0",
				GPUName:       "H100",
				GPUMemUsedMiB: 150,
				GPUUtilPct:    float64PtrTelemetry(60),
				GPUMemUtilPct: float64PtrTelemetry(15),
			}},
		},
	}
	if err := db.InsertTelemetrySamples(database, jobID, newTelemetry); err != nil {
		t.Fatalf("InsertTelemetrySamples(new): %v", err)
	}

	out, err := collectTelemetryOutput(database, jobID)
	if err != nil {
		t.Fatalf("collectTelemetryOutput: %v", err)
	}
	if out.AttemptID != newAttemptID {
		t.Fatalf("AttemptID = %d, want %d", out.AttemptID, newAttemptID)
	}
	if out.TimeMin != 2000 || out.TimeMax != 2001 {
		t.Fatalf("time range = %d..%d, want 2000..2001", out.TimeMin, out.TimeMax)
	}
	if out.GPU == nil {
		t.Fatal("GPU stats = nil, want latest-run stats")
	}
	if out.GPU.MemPeakMiB == nil || *out.GPU.MemPeakMiB != 120 {
		t.Fatalf("GPU.MemPeakMiB = %v, want 120", out.GPU.MemPeakMiB)
	}
	if out.GPU.TempMax == nil || *out.GPU.TempMax != 65 {
		t.Fatalf("GPU.TempMax = %v, want 65", out.GPU.TempMax)
	}
	if out.Summary == nil || len(out.Summary.GPUs) != 1 {
		t.Fatalf("Summary = %+v, want one GPU summary", out.Summary)
	}
	if out.Summary.GPUs[0].GPUPeakMemMiB != 150 {
		t.Fatalf("Summary peak mem = %d, want 150", out.Summary.GPUs[0].GPUPeakMemMiB)
	}
}

func float64PtrTelemetry(value float64) *float64 {
	return &value
}
