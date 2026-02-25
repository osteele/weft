package ops

import (
	"encoding/json"
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
