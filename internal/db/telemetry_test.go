package db

import "testing"

func TestInsertAndSummarizeTelemetry(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	meta := &JobMetadata{
		Resource: &ResourceUsage{GPUDevices: "0,1"},
	}
	if err := SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if err := RecordCompletionByID(database, jobID, 0, job.StartTime+2); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	samples := []TelemetrySample{
		{
			Ts:           1000,
			ElapsedS:     0,
			ProcCPUUserS: 0.4,
			ProcCPUSysS:  0.1,
			ProcRSSKB:    1000,
			GPUs: []TelemetryGPUSample{
				{
					GPUIndex:       "0",
					GPUName:        "A100",
					GPUMemUsedMiB:  100,
					GPUUtilPct:     telemetryFloat64Ptr(50),
					GPUMemUtilPct:  telemetryFloat64Ptr(25),
					GPUSMClockMHz:  telemetryUint32Ptr(1200),
					GPUMemClockMHz: telemetryUint32Ptr(1500),
				},
			},
		},
		{
			Ts:           1001,
			ElapsedS:     1,
			ProcCPUUserS: 0.8,
			ProcCPUSysS:  0.2,
			ProcRSSKB:    2000,
			GPUs: []TelemetryGPUSample{
				{
					GPUIndex:       "0",
					GPUName:        "A100",
					GPUMemUsedMiB:  120,
					GPUUtilPct:     telemetryFloat64Ptr(100),
					GPUMemUtilPct:  telemetryFloat64Ptr(50),
					GPUSMClockMHz:  telemetryUint32Ptr(1400),
					GPUMemClockMHz: telemetryUint32Ptr(1600),
				},
				{
					GPUIndex:      "1",
					GPUName:       "A100",
					GPUMemUsedMiB: 80,
					GPUSMClockMHz: telemetryUint32Ptr(1000),
				},
			},
		},
	}
	if err := InsertTelemetrySamples(database, jobID, samples); err != nil {
		t.Fatalf("InsertTelemetrySamples: %v", err)
	}
	if err := RefreshJobTelemetrySummary(database, jobID); err != nil {
		t.Fatalf("RefreshJobTelemetrySummary: %v", err)
	}

	updated, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID updated: %v", err)
	}
	if updated.Metadata == nil || updated.Metadata.Telemetry == nil {
		t.Fatalf("expected telemetry summary in metadata, got %+v", updated.Metadata)
	}
	summary := updated.Metadata.Telemetry
	if summary.CPUCoreSeconds != 1.0 {
		t.Fatalf("cpu_core_seconds = %v, want 1.0", summary.CPUCoreSeconds)
	}
	if summary.MeanCPUCores != 0.5 {
		t.Fatalf("mean_cpu_cores = %v, want 0.5", summary.MeanCPUCores)
	}
	if summary.MaxRSSKB != 2000 {
		t.Fatalf("max_rss_kb = %d, want 2000", summary.MaxRSSKB)
	}
	if len(summary.AssignedGPUIndices) != 2 || summary.AssignedGPUIndices[0] != "0" || summary.AssignedGPUIndices[1] != "1" {
		t.Fatalf("assigned_gpu_indices = %v", summary.AssignedGPUIndices)
	}
	if len(summary.GPUs) != 2 {
		t.Fatalf("gpu summary count = %d, want 2", len(summary.GPUs))
	}
	if got := summary.GPUs[0]; got.GPUSMClockMinMHz == nil || *got.GPUSMClockMinMHz != 1200 ||
		got.GPUSMClockMaxMHz == nil || *got.GPUSMClockMaxMHz != 1400 ||
		got.GPUSMClockMeanMHz == nil || *got.GPUSMClockMeanMHz != 1300 {
		t.Fatalf("GPU 0 SM clock summary = %+v, want 1200/1400/1300", got)
	}
	if got := summary.GPUs[0]; got.GPUMemClockMinMHz == nil || *got.GPUMemClockMinMHz != 1500 ||
		got.GPUMemClockMaxMHz == nil || *got.GPUMemClockMaxMHz != 1600 ||
		got.GPUMemClockMeanMHz == nil || *got.GPUMemClockMeanMHz != 1550 {
		t.Fatalf("GPU 0 memory clock summary = %+v, want 1500/1600/1550", got)
	}
	if got := summary.GPUs[1]; got.GPUSMClockMinMHz == nil || *got.GPUSMClockMinMHz != 1000 || got.GPUMemClockMaxMHz != nil {
		t.Fatalf("GPU 1 partial clock summary = %+v, want SM=1000 and absent memory clock", got)
	}

	got, err := GetTelemetryByRun(database, *updated.LatestRunID)
	if err != nil {
		t.Fatalf("GetTelemetryByRun: %v", err)
	}
	if len(got) != 2 || len(got[1].GPUs) != 2 {
		t.Fatalf("telemetry rows = %+v", got)
	}
}

func telemetryFloat64Ptr(value float64) *float64 {
	return &value
}

// Every sample's GPU rows must survive the join, not just the last one's.
// The GPU rows are attached to the sample slice after it is fully built, so a
// join that holds pointers taken during the build attaches rows to backing
// arrays that append has already discarded, and the earlier samples come back
// with no GPUs at all.
func TestGetTelemetryByRunAttachesGPUsToEverySample(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}

	// Enough samples that the slice reallocates several times while growing.
	const sampleCount = 8
	var samples []TelemetrySample
	for i := range sampleCount {
		samples = append(samples, TelemetrySample{
			Ts:       int64(1000 + i),
			ElapsedS: float64(i),
			GPUs: []TelemetryGPUSample{{
				GPUIndex:      "0",
				GPUName:       "A100",
				GPUMemUsedMiB: 100 + i,
				GPUUtilPct:    telemetryFloat64Ptr(float64(i)),
				GPUSMClockMHz: telemetryUint32Ptr(uint32(1200 + i)),
			}},
		})
	}
	if err := InsertTelemetrySamples(database, jobID, samples); err != nil {
		t.Fatalf("InsertTelemetrySamples: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LatestRunID == nil {
		t.Fatal("LatestRunID = nil, want a run")
	}
	got, err := GetTelemetryByRun(database, *job.LatestRunID)
	if err != nil {
		t.Fatalf("GetTelemetryByRun: %v", err)
	}
	if len(got) != sampleCount {
		t.Fatalf("samples = %d, want %d", len(got), sampleCount)
	}
	for i, s := range got {
		if len(s.GPUs) != 1 {
			t.Errorf("sample %d (ts=%d): GPUs = %d, want 1", i, s.Ts, len(s.GPUs))
			continue
		}
		if s.GPUs[0].GPUSMClockMHz == nil {
			t.Errorf("sample %d (ts=%d): clock absent", i, s.Ts)
		}
	}
}

func telemetryUint32Ptr(value uint32) *uint32 {
	return &value
}
