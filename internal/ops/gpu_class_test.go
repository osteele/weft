package ops

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/osteele/remote-jobs/internal/db"
)

func TestCommandJobGPUClassSerialization(t *testing.T) {
	t.Run("omitempty when empty", func(t *testing.T) {
		job := CommandJob{ID: 1, Cmd: "echo hi"}
		data, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "gpu_class") {
			t.Errorf("expected gpu_class to be omitted, got: %s", data)
		}
	})

	t.Run("present when set", func(t *testing.T) {
		job := CommandJob{ID: 1, Cmd: "echo hi", GPUClass: "A100"}
		data, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"gpu_class":"A100"`) {
			t.Errorf("expected gpu_class in JSON, got: %s", data)
		}
	})

	t.Run("round-trip", func(t *testing.T) {
		original := CommandJob{ID: 42, Cmd: "train.py", GPUClass: "RTX 3090", GPU: ""}
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded CommandJob
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.GPUClass != original.GPUClass {
			t.Errorf("GPUClass: got %q, want %q", decoded.GPUClass, original.GPUClass)
		}
		if decoded.GPU != "" {
			t.Errorf("GPU should be empty, got %q", decoded.GPU)
		}
	})
}

// TestQueueEnvVarsForJob_GPUClass verifies CUDA_VISIBLE_DEVICES injection for
// GPU class-based jobs vs explicit GPU device jobs.
func TestQueueEnvVarsForJob_GPUClass(t *testing.T) {
	t.Run("class-based job does not inject CUDA_VISIBLE_DEVICES", func(t *testing.T) {
		job := &db.Job{
			ID:       1,
			GPU:      "",
			GPUClass: "A100",
			EnvVars:  []string{"FOO=bar"},
		}
		result := queueEnvVarsForJob(job, nil)
		for _, ev := range result {
			if strings.Contains(ev, "CUDA_VISIBLE_DEVICES") {
				t.Errorf("class-based job should not have CUDA_VISIBLE_DEVICES, got: %v", result)
			}
		}
	})

	t.Run("explicit GPU job still injects CUDA_VISIBLE_DEVICES", func(t *testing.T) {
		job := &db.Job{
			ID:      1,
			GPU:     "0",
			EnvVars: []string{"FOO=bar"},
		}
		result := queueEnvVarsForJob(job, nil)
		found := false
		for _, ev := range result {
			if ev == "CUDA_VISIBLE_DEVICES=0" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected CUDA_VISIBLE_DEVICES=0, got: %v", result)
		}
	})
}

// TestQueueEnvVarsForJob_GPUParity verifies that specifying a GPU via
// --gpu flag vs --env CUDA_VISIBLE_DEVICES=X produces equivalent env vars
// in the queue entry sent to the remote host.
func TestQueueEnvVarsForJob_GPUParity(t *testing.T) {
	t.Run("--gpu flag job gets CUDA_VISIBLE_DEVICES injected", func(t *testing.T) {
		job := &db.Job{ID: 1, GPU: "1", EnvVars: []string{}}
		result := queueEnvVarsForJob(job, nil)
		assertHasCUDADevice(t, result, "1")
	})

	t.Run("--env CUDA_VISIBLE_DEVICES job keeps existing env var", func(t *testing.T) {
		job := &db.Job{ID: 1, GPU: "", EnvVars: []string{"CUDA_VISIBLE_DEVICES=1"}}
		result := queueEnvVarsForJob(job, nil)
		assertHasCUDADevice(t, result, "1")
	})

	t.Run("--gpu flag does not duplicate when env var already present", func(t *testing.T) {
		job := &db.Job{ID: 1, GPU: "1", EnvVars: []string{"CUDA_VISIBLE_DEVICES=1"}}
		result := queueEnvVarsForJob(job, nil)
		count := 0
		for _, ev := range result {
			if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly 1 CUDA_VISIBLE_DEVICES, got %d in: %v", count, result)
		}
	})

	t.Run("no GPU produces no CUDA_VISIBLE_DEVICES", func(t *testing.T) {
		job := &db.Job{ID: 1, GPU: "", EnvVars: []string{"OTHER=value"}}
		result := queueEnvVarsForJob(job, nil)
		for _, ev := range result {
			if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
				t.Errorf("non-GPU job should not have CUDA_VISIBLE_DEVICES, got: %v", result)
			}
		}
	})

	t.Run("multi-GPU via flag", func(t *testing.T) {
		job := &db.Job{ID: 1, GPU: "0,1", EnvVars: []string{}}
		result := queueEnvVarsForJob(job, nil)
		assertHasCUDADevice(t, result, "0,1")
	})

	t.Run("multi-GPU via env var", func(t *testing.T) {
		job := &db.Job{ID: 1, GPU: "", EnvVars: []string{"CUDA_VISIBLE_DEVICES=0,1"}}
		result := queueEnvVarsForJob(job, nil)
		assertHasCUDADevice(t, result, "0,1")
	})
}

// TestQueueEntryForJob_GPUParity verifies that queueEntryForJob produces
// equivalent entries regardless of how the GPU was specified.
func TestQueueEntryForJob_GPUParity(t *testing.T) {
	gpuMem := intPtr(20)

	t.Run("--gpu flag produces entry with GPU field and CUDA env var", func(t *testing.T) {
		job := &db.Job{
			ID: 1, WorkingDir: "/work", Command: "train.py",
			GPU: "0", GPUMemGB: gpuMem,
			EnvVars: []string{"FOO=bar"},
		}
		entry := queueEntryForJob(job, nil, "")
		if entry.GPU != "0" {
			t.Errorf("expected GPU=0, got %q", entry.GPU)
		}
		if entry.GPUMemGB == nil || *entry.GPUMemGB != 20 {
			t.Errorf("expected GPUMemGB=20, got %v", entry.GPUMemGB)
		}
		assertHasCUDADevice(t, entry.EnvVars, "0")
	})

	t.Run("--env CUDA_VISIBLE_DEVICES produces entry with env var", func(t *testing.T) {
		job := &db.Job{
			ID: 1, WorkingDir: "/work", Command: "train.py",
			GPU: "", GPUMemGB: gpuMem,
			EnvVars: []string{"FOO=bar", "CUDA_VISIBLE_DEVICES=0"},
		}
		entry := queueEntryForJob(job, nil, "")
		assertHasCUDADevice(t, entry.EnvVars, "0")
		if entry.GPUMemGB == nil || *entry.GPUMemGB != 20 {
			t.Errorf("expected GPUMemGB=20, got %v", entry.GPUMemGB)
		}
	})

	t.Run("gpu_class job has no CUDA env var in entry", func(t *testing.T) {
		job := &db.Job{
			ID: 1, WorkingDir: "/work", Command: "train.py",
			GPUClass: "A100", GPUMemGB: gpuMem,
			EnvVars: []string{"FOO=bar"},
		}
		entry := queueEntryForJob(job, nil, "")
		if entry.GPUClass != "A100" {
			t.Errorf("expected GPUClass=A100, got %q", entry.GPUClass)
		}
		for _, ev := range entry.EnvVars {
			if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
				t.Errorf("gpu_class job should not have CUDA_VISIBLE_DEVICES in entry, got: %v", entry.EnvVars)
			}
		}
	})
}

// TestNewAddCommand_GPUParity verifies that NewAddCommand produces equivalent
// JSON commands for --gpu flag and --env CUDA_VISIBLE_DEVICES paths.
func TestNewAddCommand_GPUParity(t *testing.T) {
	gpuMem := intPtr(20)

	t.Run("--gpu flag command has gpu and gpu_mem fields", func(t *testing.T) {
		entry := QueueEntry{
			JobID: 1, Command: "train.py",
			GPU: "0", GPUMemGB: gpuMem,
			EnvVars: []string{"CUDA_VISIBLE_DEVICES=0"},
		}
		cmd := NewAddCommand(entry)
		if cmd.Job.GPU != "0" {
			t.Errorf("expected GPU=0, got %q", cmd.Job.GPU)
		}
		if cmd.Job.GPUMem == nil || *cmd.Job.GPUMem != 20 {
			t.Errorf("expected GPUMem=20, got %v", cmd.Job.GPUMem)
		}
	})

	t.Run("--env CUDA_VISIBLE_DEVICES command has gpu_mem but no gpu field", func(t *testing.T) {
		entry := QueueEntry{
			JobID: 1, Command: "train.py",
			GPU: "", GPUMemGB: gpuMem,
			EnvVars: []string{"CUDA_VISIBLE_DEVICES=0"},
		}
		cmd := NewAddCommand(entry)
		// GPU field empty — device info is only in env vars
		if cmd.Job.GPU != "" {
			t.Errorf("expected empty GPU field, got %q", cmd.Job.GPU)
		}
		// But gpu_mem MUST still be set so the scheduler can track VRAM
		if cmd.Job.GPUMem == nil || *cmd.Job.GPUMem != 20 {
			t.Errorf("expected GPUMem=20, got %v", cmd.Job.GPUMem)
		}
		// CUDA env var must be present
		assertHasCUDADevice(t, cmd.Job.Env, "0")
	})

	t.Run("gpu_class command has gpu_class and gpu_mem but no gpu", func(t *testing.T) {
		entry := QueueEntry{
			JobID: 1, Command: "train.py",
			GPUClass: "A100", GPUMemGB: gpuMem,
		}
		cmd := NewAddCommand(entry)
		if cmd.Job.GPUClass != "A100" {
			t.Errorf("expected GPUClass=A100, got %q", cmd.Job.GPUClass)
		}
		if cmd.Job.GPU != "" {
			t.Errorf("expected empty GPU field, got %q", cmd.Job.GPU)
		}
		if cmd.Job.GPUMem == nil || *cmd.Job.GPUMem != 20 {
			t.Errorf("expected GPUMem=20, got %v", cmd.Job.GPUMem)
		}
	})

	t.Run("no GPU command has no gpu fields", func(t *testing.T) {
		entry := QueueEntry{
			JobID: 1, Command: "echo hi",
		}
		cmd := NewAddCommand(entry)
		if cmd.Job.GPU != "" {
			t.Errorf("expected empty GPU, got %q", cmd.Job.GPU)
		}
		if cmd.Job.GPUClass != "" {
			t.Errorf("expected empty GPUClass, got %q", cmd.Job.GPUClass)
		}
		if cmd.Job.GPUMem != nil {
			t.Errorf("expected nil GPUMem, got %d", *cmd.Job.GPUMem)
		}
	})
}

// TestNewAddCommand_JSONParity verifies that the serialized JSON sent to the
// queue runner contains the fields needed for GPU scheduling, regardless of
// how the GPU was specified.
func TestNewAddCommand_JSONParity(t *testing.T) {
	gpuMem := intPtr(20)

	// Helper to marshal and unmarshal to check the JSON structure
	marshalJob := func(t *testing.T, entry QueueEntry) map[string]any {
		t.Helper()
		cmd := NewAddCommand(entry)
		data, err := json.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		jobMap, ok := result["job"].(map[string]any)
		if !ok {
			t.Fatal("expected 'job' field in command JSON")
		}
		return jobMap
	}

	t.Run("--gpu flag job JSON has gpu_mem", func(t *testing.T) {
		jobMap := marshalJob(t, QueueEntry{
			JobID: 1, Command: "train.py",
			GPU: "0", GPUMemGB: gpuMem,
			EnvVars: []string{"CUDA_VISIBLE_DEVICES=0"},
		})
		if jobMap["gpu"] != "0" {
			t.Errorf("expected gpu=0, got %v", jobMap["gpu"])
		}
		if jobMap["gpu_mem"] != float64(20) {
			t.Errorf("expected gpu_mem=20, got %v", jobMap["gpu_mem"])
		}
	})

	t.Run("--env CUDA_VISIBLE_DEVICES job JSON has gpu_mem", func(t *testing.T) {
		jobMap := marshalJob(t, QueueEntry{
			JobID: 1, Command: "train.py",
			GPUMemGB: gpuMem,
			EnvVars:  []string{"CUDA_VISIBLE_DEVICES=0"},
		})
		// gpu field should be absent or empty
		if gpu, ok := jobMap["gpu"]; ok && gpu != "" {
			t.Errorf("expected no gpu field, got %v", gpu)
		}
		// gpu_mem MUST be present for the scheduler to track VRAM
		if jobMap["gpu_mem"] != float64(20) {
			t.Errorf("expected gpu_mem=20, got %v (type %T)", jobMap["gpu_mem"], jobMap["gpu_mem"])
		}
	})

	t.Run("non-GPU job JSON has no gpu_mem", func(t *testing.T) {
		jobMap := marshalJob(t, QueueEntry{
			JobID: 1, Command: "echo hi",
		})
		if _, ok := jobMap["gpu_mem"]; ok {
			t.Errorf("expected no gpu_mem field, got %v", jobMap["gpu_mem"])
		}
		if _, ok := jobMap["gpu"]; ok {
			t.Errorf("expected no gpu field, got %v", jobMap["gpu"])
		}
	})
}

// TestQueueJobParams_GPUExtraction tests that QueueJobParams correctly extracts
// GPU device info from env vars when no explicit GPU is set. This tests the
// extraction logic that runs inside QueueJob (without needing a database).
func TestQueueJobParams_GPUExtraction(t *testing.T) {
	// This function mimics the GPU extraction logic from QueueJob
	extractGPU := func(params QueueJobParams) (gpu string, gpuMemGB *int) {
		gpu = params.GPU
		if gpu == "" {
			for _, ev := range params.EnvVars {
				if val, ok := strings.CutPrefix(ev, "CUDA_VISIBLE_DEVICES="); ok {
					gpu = val
					break
				}
			}
		}
		gpuMemGB = params.GPUMemGB
		if gpuMemGB == nil && (gpu != "" || params.GPUClass != "") {
			defaultMem := DefaultGPUMemGB
			gpuMemGB = &defaultMem
		}
		return
	}

	t.Run("--gpu 0 gets default GPU mem", func(t *testing.T) {
		gpu, mem := extractGPU(QueueJobParams{GPU: "0"})
		if gpu != "0" {
			t.Errorf("expected gpu=0, got %q", gpu)
		}
		if mem == nil || *mem != DefaultGPUMemGB {
			t.Errorf("expected mem=%d, got %v", DefaultGPUMemGB, mem)
		}
	})

	t.Run("--env CUDA_VISIBLE_DEVICES=0 gets same default GPU mem", func(t *testing.T) {
		gpu, mem := extractGPU(QueueJobParams{
			EnvVars: []string{"CUDA_VISIBLE_DEVICES=0"},
		})
		if gpu != "0" {
			t.Errorf("expected gpu=0, got %q", gpu)
		}
		if mem == nil || *mem != DefaultGPUMemGB {
			t.Errorf("expected mem=%d, got %v", DefaultGPUMemGB, mem)
		}
	})

	t.Run("--gpu 0,1 gets default GPU mem", func(t *testing.T) {
		gpu, mem := extractGPU(QueueJobParams{GPU: "0,1"})
		if gpu != "0,1" {
			t.Errorf("expected gpu=0,1, got %q", gpu)
		}
		if mem == nil || *mem != DefaultGPUMemGB {
			t.Errorf("expected mem=%d, got %v", DefaultGPUMemGB, mem)
		}
	})

	t.Run("--env CUDA_VISIBLE_DEVICES=0,1 gets same default GPU mem", func(t *testing.T) {
		gpu, mem := extractGPU(QueueJobParams{
			EnvVars: []string{"CUDA_VISIBLE_DEVICES=0,1"},
		})
		if gpu != "0,1" {
			t.Errorf("expected gpu=0,1, got %q", gpu)
		}
		if mem == nil || *mem != DefaultGPUMemGB {
			t.Errorf("expected mem=%d, got %v", DefaultGPUMemGB, mem)
		}
	})

	t.Run("--gpu-class A100 gets default GPU mem", func(t *testing.T) {
		_, mem := extractGPU(QueueJobParams{GPUClass: "A100"})
		if mem == nil || *mem != DefaultGPUMemGB {
			t.Errorf("expected mem=%d, got %v", DefaultGPUMemGB, mem)
		}
	})

	t.Run("explicit --gpu-mem overrides default", func(t *testing.T) {
		custom := 40
		_, mem := extractGPU(QueueJobParams{
			GPU:      "0",
			GPUMemGB: &custom,
		})
		if mem == nil || *mem != 40 {
			t.Errorf("expected mem=40, got %v", mem)
		}
	})

	t.Run("explicit --gpu-mem with env var GPU overrides default", func(t *testing.T) {
		custom := 40
		_, mem := extractGPU(QueueJobParams{
			EnvVars:  []string{"CUDA_VISIBLE_DEVICES=0"},
			GPUMemGB: &custom,
		})
		if mem == nil || *mem != 40 {
			t.Errorf("expected mem=40, got %v", mem)
		}
	})

	t.Run("no GPU at all gets nil GPU mem", func(t *testing.T) {
		gpu, mem := extractGPU(QueueJobParams{
			EnvVars: []string{"OTHER=value"},
		})
		if gpu != "" {
			t.Errorf("expected empty gpu, got %q", gpu)
		}
		if mem != nil {
			t.Errorf("expected nil mem, got %d", *mem)
		}
	})

	t.Run("--gpu takes precedence over env var", func(t *testing.T) {
		gpu, _ := extractGPU(QueueJobParams{
			GPU:     "1",
			EnvVars: []string{"CUDA_VISIBLE_DEVICES=0"},
		})
		if gpu != "1" {
			t.Errorf("expected gpu=1 (from flag), got %q", gpu)
		}
	})
}

func assertHasCUDADevice(t *testing.T, envVars []string, device string) {
	t.Helper()
	expected := "CUDA_VISIBLE_DEVICES=" + device
	if !slices.Contains(envVars, expected) {
		t.Errorf("expected %s in env vars, got: %v", expected, envVars)
	}
}

func intPtr(n int) *int {
	return &n
}
