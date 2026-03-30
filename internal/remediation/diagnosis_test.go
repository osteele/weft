package remediation

import "testing"

func TestDiagnoseFromLog_DataPriority(t *testing.T) {
	// Log contains both a data error and a code error; data should win
	log := `ModuleNotFoundError: No module named 'transformers'
FileNotFoundError: /home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B/snapshots/abc/model.safetensors`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Category != "data" {
		t.Errorf("expected data category to take priority, got %s", d.Category)
	}
	if d.Pattern != "missing_hf_model" {
		t.Errorf("expected missing_hf_model, got %s", d.Pattern)
	}
}

func TestDiagnoseFromLog_CodeOnly(t *testing.T) {
	log := `Traceback (most recent call last):
  File "train.py", line 1, in <module>
ModuleNotFoundError: No module named 'transformers'`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Pattern != "missing_import" {
		t.Errorf("expected missing_import, got %s", d.Pattern)
	}
}

func TestDiagnoseFromLog_NoMatch(t *testing.T) {
	log := `epoch 1/10: loss=2.34
epoch 2/10: loss=1.89
Training complete!`

	d := DiagnoseFromLog(log)
	if d != nil {
		t.Errorf("expected nil diagnosis for clean log, got %+v", d)
	}
}

func TestDiagnoseFromLog_EnvironmentError(t *testing.T) {
	log := `torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Pattern != "gpu_oom" {
		t.Errorf("expected gpu_oom, got %s", d.Pattern)
	}
	if d.Remediable {
		t.Error("GPU OOM should not be remediable")
	}
}

func TestMarshalUnmarshalDiagnosis(t *testing.T) {
	d := &ErrorDiagnosis{
		Pattern:       "missing_hf_model",
		Category:      "data",
		Message:       "Missing HuggingFace model",
		MissingAssets: []string{"hf:meta-llama/Llama-3-8B"},
		Remediable:    true,
		Details:       "FileNotFoundError: ...",
	}

	s, err := MarshalDiagnosis(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	d2, err := UnmarshalDiagnosis(s)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if d2.Pattern != d.Pattern {
		t.Errorf("pattern mismatch: %s vs %s", d2.Pattern, d.Pattern)
	}
	if len(d2.MissingAssets) != 1 || d2.MissingAssets[0] != "hf:meta-llama/Llama-3-8B" {
		t.Errorf("missing assets mismatch: %v", d2.MissingAssets)
	}
}

func TestUnmarshalDiagnosis_Empty(t *testing.T) {
	d, err := UnmarshalDiagnosis("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Errorf("expected nil for empty string, got %+v", d)
	}
}

func TestCheckFatalAtRuntime_CUDAUnknownError(t *testing.T) {
	log := `Loading data...
RuntimeError: CUDA unknown error - this may be due to a hardware failure
Process finished with exit code 1`

	d := CheckFatalAtRuntime(log)
	if d == nil {
		t.Fatal("expected fatal diagnosis for CUDA unknown error")
	}
	if d.Pattern != "cuda_fatal" {
		t.Errorf("expected cuda_fatal, got %s", d.Pattern)
	}
}

func TestCheckFatalAtRuntime_CUDARuntimeError(t *testing.T) {
	log := `RuntimeError: CUDA error: unspecified launch failure
CUDA kernel errors might be asynchronously reported`

	d := CheckFatalAtRuntime(log)
	if d == nil {
		t.Fatal("expected fatal diagnosis for CUDA runtime error")
	}
	if d.Pattern != "cuda_error" {
		t.Errorf("expected cuda_error, got %s", d.Pattern)
	}
}

func TestCheckFatalAtRuntime_CannotReinitialize(t *testing.T) {
	log := `UserWarning: CUDA initialization: cannot re-initialize CUDA without restarting the process`

	d := CheckFatalAtRuntime(log)
	if d == nil {
		t.Fatal("expected fatal diagnosis for CUDA re-init failure")
	}
	if d.Pattern != "cuda_fatal" {
		t.Errorf("expected cuda_fatal, got %s", d.Pattern)
	}
}

func TestCheckFatalAtRuntime_NormalOutput(t *testing.T) {
	log := `epoch 1/10: loss=2.34
GPU memory: 12.5GB / 24.0GB
epoch 2/10: loss=1.89
Training complete!`

	d := CheckFatalAtRuntime(log)
	if d != nil {
		t.Errorf("expected nil for normal training output, got %+v", d)
	}
}

func TestCheckFatalAtRuntime_GPUOOMNotFatal(t *testing.T) {
	// GPU OOM is not fatal-at-runtime (the process usually exits on its own)
	log := `torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB`

	d := CheckFatalAtRuntime(log)
	if d != nil {
		t.Errorf("GPU OOM should not be fatal-at-runtime, got %+v", d)
	}
}
