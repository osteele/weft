package db

import "testing"

func TestClassifyFailureMode(t *testing.T) {
	tests := []struct {
		name          string
		failureReason string
		diagnosis     string
		message       string
		exitCode      int
		want          string
	}{
		{name: "diagnosis pattern wins", diagnosis: `{"pattern":"gpu_oom"}`, want: "gpu_oom"},
		{name: "module text", message: "ModuleNotFoundError: No module named transformers", want: "module_not_found"},
		{name: "cuda fault text", message: "CUDA error: an illegal memory access was encountered", want: "cuda_fault"},
		{name: "cuda hardware text", message: "torch.AcceleratorError: CUDA error: Invalid access of peer GPU memory over nvlink or a hardware error", want: "cuda_hardware_fault"},
		{name: "cuda hardware reason", failureReason: FailureReasonInfraCUDAHardwareFault, want: "cuda_hardware_fault"},
		{name: "exit 137", exitCode: 137, want: "oom"},
		{name: "success empty", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyFailureMode(tt.failureReason, tt.diagnosis, tt.message, tt.exitCode)
			if got != tt.want {
				t.Fatalf("ClassifyFailureMode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPredictFailureModesUsesSpecificScope(t *testing.T) {
	database := setupTestDB(t)

	for i := 0; i < 3; i++ {
		jobID, err := RecordQueuedWithGPU(database, "host-a", "/tmp/test", "python train.py", "test", "a100")
		if err != nil {
			t.Fatalf("insert failed job: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE job_attempts
			 SET error_diagnosis = ?, exit_code = 1, end_time = 2000
			 WHERE job_id = ? AND end_time IS NULL`,
			`{"pattern":"module_not_found"}`,
			jobID,
		); err != nil {
			t.Fatalf("update failed job: %v", err)
		}
	}
	jobID, err := RecordQueuedWithGPU(database, "host-a", "/tmp/test", "python train.py", "test", "a100")
	if err != nil {
		t.Fatalf("insert successful job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET exit_code = 0, end_time = 2000 WHERE job_id = ? AND end_time IS NULL`,
		jobID,
	); err != nil {
		t.Fatalf("update successful job: %v", err)
	}

	preds, err := PredictFailureModes(database, "python train.py", "host-a", "a100")
	if err != nil {
		t.Fatalf("PredictFailureModes: %v", err)
	}
	if len(preds) == 0 {
		t.Fatal("expected a failure mode prediction")
	}
	if preds[0].Mode != "module_not_found" {
		t.Fatalf("top mode = %q, want module_not_found", preds[0].Mode)
	}
	if preds[0].Scope != "command+host" {
		t.Fatalf("scope = %q, want command+host", preds[0].Scope)
	}
	if preds[0].Failures != 3 || preds[0].Total != 4 {
		t.Fatalf("counts = %d/%d, want 3/4", preds[0].Failures, preds[0].Total)
	}
}
