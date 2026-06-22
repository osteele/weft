package skypilot

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestParseJobsQueueJSONFlexibleShapes(t *testing.T) {
	jobs, err := ParseJobsQueueJSON([]byte(`{
		"jobs": [
			{
				"job_id": 42,
				"task_id": "train",
				"name": "weft-wj7",
				"status": "RUNNING",
				"cluster": "sky-cluster",
				"run": "python train.py"
			}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseJobsQueueJSON: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.ID != "42" || job.TaskID != "train" || job.Name != "weft-wj7" || job.ClusterName != "sky-cluster" {
		t.Fatalf("parsed job = %+v", job)
	}
	if job.Command != "python train.py" {
		t.Fatalf("Command = %q", job.Command)
	}
}

func TestNormalizeStatus(t *testing.T) {
	tests := map[string]string{
		"submitted":    db.StatusQueued,
		"PROVISIONING": db.StatusQueued,
		"running":      db.StatusRunning,
		"succeeded":    db.StatusCompleted,
		"failed":       db.StatusFailed,
		"cancelled":    db.StatusCanceled,
		"stopped":      db.StatusKilled,
	}
	for raw, want := range tests {
		if got := NormalizeStatus(raw); got != want {
			t.Fatalf("NormalizeStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestBuildTaskYAML(t *testing.T) {
	mem := 80
	task, err := BuildTaskYAML(SubmitOptions{
		Name:     "weft-wj12",
		Command:  "python train.py",
		WorkDir:  "/tmp/project",
		GPUClass: "a100",
		GPUCount: 2,
		GPUMemGB: &mem,
		EnvVars:  []string{"WANDB_MODE=offline"},
	})
	if err != nil {
		t.Fatalf("BuildTaskYAML: %v", err)
	}
	for _, want := range []string{
		`name: "weft-wj12"`,
		`workdir: "/tmp/project"`,
		`accelerators: A100:2`,
		`gpu_memory: 80`,
		`WANDB_MODE: "offline"`,
		`run: "python train.py"`,
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("task YAML missing %q:\n%s", want, task)
		}
	}
}
