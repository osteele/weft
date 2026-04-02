package db

import (
	"encoding/json"
	"testing"
)

func TestTrainingExamplesViewSeparatesRequestedAndActualHardware(t *testing.T) {
	database := SetupTestDB(t)

	if err := SaveCachedHostInfo(database, &CachedHostInfo{
		Name:        "cool30",
		CPUCount:    32,
		CPUModel:    "AMD Ryzen",
		CPUFreq:     "3.5 GHz",
		MemTotal:    "128G",
		GPUsJSON:    `[{"Index":0,"Name":"RTX 3090","MemTotal":"24576MiB"},{"Index":1,"Name":"RTX 2080 Ti","MemTotal":"11264MiB"}]`,
		LastUpdated: 1234,
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	jobID, err := RecordQueued(database, "cool30", "/tmp/proj", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := SetJobGPU(database, jobID, "0,1"); err != nil {
		t.Fatalf("SetJobGPU: %v", err)
	}
	if err := SetJobGPUClass(database, jobID, "a100"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := SetJobCPUAllotment(database, jobID, intPtr(400)); err != nil {
		t.Fatalf("SetJobCPUAllotment: %v", err)
	}
	if err := SetJobGPUMemGB(database, jobID, intPtr(80)); err != nil {
		t.Fatalf("SetJobGPUMemGB: %v", err)
	}
	if err := SetJobTags(database, jobID, []string{"benchmark", "nightly"}); err != nil {
		t.Fatalf("SetJobTags: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	meta := &JobMetadata{
		Resource: &ResourceUsage{
			PeakRSSKB:    int64Ptr(123456),
			MaxGPUMemMiB: int64Ptr(4096),
			GPUDevices:   "1",
		},
		CPU: &JobCPUStats{Mean: float64Ptr(12.5)},
	}
	if err := SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}
	if err := RecordCompletionByID(database, jobID, 0, job.StartTime+42); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	var (
		host, requestedGPU, requestedGPUClass string
		actualGPUName, actualGPUClass         string
		gpuNamesJSON, tagsJSON                string
		cpuAllotment, gpuMemGB                int
		cpuCount                              int
		cpuModel                              string
		peakRSSKB, maxGPUMemMiB               int64
		gpuCount, gpuVRAMPerDeviceMiB         int
		gpuVRAMTotalMiB                       int
	)
	err = database.QueryRow(`
		SELECT
			host,
			requested_gpu,
			requested_gpu_class,
			cpu_allotment,
			gpu_mem_gb,
			tags,
			cpu_count,
			cpu_model,
			actual_gpu_name,
			gpu_names,
			gpu_count,
			gpu_vram_per_device_mib,
			gpu_vram_total_mib,
			actual_gpu_class,
			peak_rss_kb,
			max_gpu_mem_mib
		FROM training_examples
		WHERE job_id = ?`, jobID).Scan(
		&host,
		&requestedGPU,
		&requestedGPUClass,
		&cpuAllotment,
		&gpuMemGB,
		&tagsJSON,
		&cpuCount,
		&cpuModel,
		&actualGPUName,
		&gpuNamesJSON,
		&gpuCount,
		&gpuVRAMPerDeviceMiB,
		&gpuVRAMTotalMiB,
		&actualGPUClass,
		&peakRSSKB,
		&maxGPUMemMiB,
	)
	if err != nil {
		t.Fatalf("QueryRow(training_examples): %v", err)
	}

	if host != "cool30" {
		t.Fatalf("host = %q, want cool30", host)
	}
	if requestedGPU != "0,1" || requestedGPUClass != "a100" {
		t.Fatalf("requested placement = (%q, %q), want (0,1, a100)", requestedGPU, requestedGPUClass)
	}
	if cpuAllotment != 400 || gpuMemGB != 80 {
		t.Fatalf("requested capacity = (%d, %d), want (400, 80)", cpuAllotment, gpuMemGB)
	}
	if cpuCount != 32 || cpuModel != "AMD Ryzen" {
		t.Fatalf("cpu hardware = (%d, %q), want (32, AMD Ryzen)", cpuCount, cpuModel)
	}
	if actualGPUName != "RTX 2080 Ti" || actualGPUClass != "rtx2080ti" {
		t.Fatalf("actual gpu = (%q, %q), want (RTX 2080 Ti, rtx2080ti)", actualGPUName, actualGPUClass)
	}
	if gpuCount != 1 || gpuVRAMPerDeviceMiB != 11264 || gpuVRAMTotalMiB != 11264 {
		t.Fatalf("gpu summary = (%d, %d, %d), want (1, 11264, 11264)", gpuCount, gpuVRAMPerDeviceMiB, gpuVRAMTotalMiB)
	}
	if peakRSSKB != 123456 || maxGPUMemMiB != 4096 {
		t.Fatalf("resource summaries = (%d, %d), want (123456, 4096)", peakRSSKB, maxGPUMemMiB)
	}

	var gpuNames []string
	if err := json.Unmarshal([]byte(gpuNamesJSON), &gpuNames); err != nil {
		t.Fatalf("Unmarshal(gpu_names): %v", err)
	}
	if len(gpuNames) != 1 || gpuNames[0] != "RTX 2080 Ti" {
		t.Fatalf("gpu_names = %+v, want [RTX 2080 Ti]", gpuNames)
	}

	var tags []string
	if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
		t.Fatalf("Unmarshal(tags): %v", err)
	}
	if len(tags) != 2 || tags[0] != "benchmark" || tags[1] != "nightly" {
		t.Fatalf("tags = %+v, want [benchmark nightly]", tags)
	}

	var aliasActualGPUClass string
	if err := database.QueryRow(`SELECT actual_gpu_class FROM job_run_training_examples WHERE job_id = ?`, jobID).Scan(&aliasActualGPUClass); err != nil {
		t.Fatalf("QueryRow(job_run_training_examples): %v", err)
	}
	if aliasActualGPUClass != "rtx2080ti" {
		t.Fatalf("job_run_training_examples.actual_gpu_class = %q, want rtx2080ti", aliasActualGPUClass)
	}

	runs, err := ListTrainingJobRuns(database, 0)
	if err != nil {
		t.Fatalf("ListTrainingJobRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("run count = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.RequestedGPUClass != "a100" || run.ActualGPUClass != "rtx2080ti" {
		t.Fatalf("run gpu classes = (%q, %q), want (a100, rtx2080ti)", run.RequestedGPUClass, run.ActualGPUClass)
	}
	if len(run.GPUNames) != 1 || run.GPUNames[0] != "RTX 2080 Ti" {
		t.Fatalf("run gpu_names = %+v, want [RTX 2080 Ti]", run.GPUNames)
	}
}

func TestTrainingExamplesViewNormalizesVendorAndMemorySuffixes(t *testing.T) {
	database := SetupTestDB(t)

	if err := SaveCachedHostInfo(database, &CachedHostInfo{
		Name:        "cool100",
		CPUCount:    64,
		CPUModel:    "AMD EPYC",
		CPUFreq:     "3.2 GHz",
		MemTotal:    "256G",
		GPUsJSON:    `[{"Index":0,"Name":"NVIDIA A100 80GB PCIe","MemTotal":"81920MiB"}]`,
		LastUpdated: 5678,
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	jobID, err := RecordQueued(database, "cool100", "/tmp/proj", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	meta := &JobMetadata{
		Resource: &ResourceUsage{
			GPUDevices: "0",
		},
	}
	if err := SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}
	if err := RecordCompletionByID(database, jobID, 0, job.StartTime+60); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	var actualGPUClass string
	if err := database.QueryRow(`SELECT actual_gpu_class FROM training_examples WHERE job_id = ?`, jobID).Scan(&actualGPUClass); err != nil {
		t.Fatalf("QueryRow(training_examples): %v", err)
	}
	if actualGPUClass != "a100" {
		t.Fatalf("actual_gpu_class = %q, want a100", actualGPUClass)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

func float64Ptr(v float64) *float64 {
	return &v
}

func intPtr(v int) *int {
	return &v
}
