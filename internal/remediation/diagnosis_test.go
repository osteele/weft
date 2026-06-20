package remediation

import (
	"strings"
	"testing"
)

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
	if d.Pattern != "module_not_found" {
		t.Errorf("expected module_not_found, got %s", d.Pattern)
	}
}

func TestDiagnoseFromLog_CUDALibSymbolMismatch(t *testing.T) {
	// Regression: a CUDA toolkit / nvidia-* wheel coherency bug surfaces
	// as a Python ImportError, but should NOT be classified as a missing
	// module. Real-world failure from wi3026 (job 2025) — image-provided
	// torch 2.7.0+cu128 vs lockfile-installed nvidia-cusparse-cu12.
	log := `Traceback (most recent call last):
  File "/workspace/adaptive-escalation/scripts/run_prefix_framing.py", line 27, in <module>
    import torch
  File "/opt/conda/lib/python3.11/site-packages/torch/__init__.py", line 409, in <module>
    from torch._C import *  # noqa: F403
    ^^^^^^^^^^^^^^^^^^^^^^
ImportError: /opt/conda/lib/python3.11/site-packages/torch/lib/../../nvidia/cusparse/lib/libcusparse.so.12: undefined symbol: __nvJitLinkCreate_12_8, version libnvJitLink.so.12`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Pattern != "cuda_lib_symbol_mismatch" {
		t.Errorf("expected cuda_lib_symbol_mismatch, got %s", d.Pattern)
	}
	if d.Category != "environment" {
		t.Errorf("expected category environment, got %s", d.Category)
	}
	if sym, _ := d.StructuredDetails["undefined_symbol"].(string); sym != "__nvJitLinkCreate_12_8" {
		t.Errorf("expected undefined_symbol __nvJitLinkCreate_12_8, got %q", sym)
	}
	if lib, _ := d.StructuredDetails["expected_in_library"].(string); lib != "libnvJitLink.so.12" {
		t.Errorf("expected expected_in_library libnvJitLink.so.12, got %q", lib)
	}
}

func TestDiagnoseFromLog_FirstPartyImportPathBeatsVLLMSetup(t *testing.T) {
	log := `=== START Thu Jun 11 13:48:59 UTC 2026 ===
cmd: uv run --with "vllm>=0.8.5" --with "transformers>=4.45" python experiments/exp_037_vllm_capacity_cliff.py --phase 1
===
Installed 154 packages in 2.71s
Traceback (most recent call last):
  File "/workspace/llm-performance-models/experiments/exp_037_vllm_capacity_cliff.py", line 36, in <module>
    from experiments._experiment_utils import write_artifact_manifest
ModuleNotFoundError: No module named 'experiments'`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Pattern != "python_first_party_import_path" {
		t.Fatalf("pattern = %q, want python_first_party_import_path", d.Pattern)
	}
	if d.Message != "First-party Python package is not on sys.path" {
		t.Fatalf("message = %q", d.Message)
	}
	if got := d.StructuredDetails["missing_module"]; got != "experiments" {
		t.Fatalf("missing_module = %v, want experiments", got)
	}
}

func TestDiagnoseFromLog_FirstPartyImportPathForPlainModuleNotFound(t *testing.T) {
	log := `=== START Thu Jun 11 21:43:40 CST 2026 ===
cmd: uv run python experiments/exp_n06_mooncake_reported_comparison.py --policies LRUCache
===
Traceback (most recent call last):
  File "/mnt/hbnas/home/oliver_30/code/research/llm-performance-models/experiments/exp_n06_mooncake_reported_comparison.py", line 26, in <module>
    from experiments._experiment_utils import write_artifact_manifest
ModuleNotFoundError: No module named 'experiments'`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Pattern != "python_first_party_import_path" {
		t.Fatalf("pattern = %q, want python_first_party_import_path", d.Pattern)
	}
	if !strings.Contains(d.Solution, "uv run experiments/script.py") {
		t.Fatalf("solution should mention direct uv run form, got %q", d.Solution)
	}
}

func TestDiagnoseFromLog_VLLMSetupRequiresFrameworkImportFailure(t *testing.T) {
	log := `Traceback (most recent call last):
  File "serve.py", line 1, in <module>
    import vllm
ModuleNotFoundError: No module named 'vllm'`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected a diagnosis")
	}
	if d.Pattern != "vllm_setup" {
		t.Fatalf("pattern = %q, want vllm_setup", d.Pattern)
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
		StructuredDetails: map[string]any{
			"missing_module": "transformers",
		},
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
	if d2.StructuredDetails["missing_module"] != "transformers" {
		t.Errorf("structured details mismatch: %v", d2.StructuredDetails)
	}
}

func TestDiagnoseFailedAttemptFromLog_PatternRegistry(t *testing.T) {
	tests := []struct {
		name    string
		log     string
		pattern string
		detail  string
	}{
		{
			name:    "preempted",
			log:     "runpod spot interruption: preempted with 30 seconds notice",
			pattern: "preempted",
			detail:  "provider",
		},
		{
			name:    "gpu oom",
			log:     "torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB on GPU 0",
			pattern: "gpu_oom",
			detail:  "requested_mib",
		},
		{
			name:    "cuda error",
			log:     "RuntimeError: CUDA error: 700 CUDA_ERROR_ILLEGAL_ADDRESS kernel name: matmul_fp16",
			pattern: "cuda_error",
			detail:  "cuda_error_code",
		},
		{
			name:    "glibcxx version not found",
			log:     "ImportError: /home/user/.julia/juliaup/julia-1.12/lib/libjulia-internal.so.1.12: version `GLIBCXX_3.4.30' not found",
			pattern: "glibcxx_version_not_found",
			detail:  "required_glibcxx",
		},
		{
			name:    "glibc version not found",
			log:     "ImportError: /opt/tool/libnative.so: version `GLIBC_2.34' not found",
			pattern: "glibc_version_not_found",
			detail:  "required_glibc",
		},
		{
			name:    "disk full",
			log:     "OSError: [Errno 28] No space left on device",
			pattern: "disk_full",
		},
		{
			name:    "ssh disconnect",
			log:     "ssh: Connection reset by peer; Broken pipe",
			pattern: "ssh_disconnect",
		},
		{
			name:    "http connection reset is not ssh disconnect",
			log:     "requests.exceptions.ConnectionError: ConnectionResetError(54, 'Connection reset by peer') while requesting https://huggingface.co/model/config.json",
			pattern: "unknown",
		},
		{
			name:    "timeout",
			log:     "agent timed out: budget 3600 seconds elapsed 3610 seconds",
			pattern: "timeout",
			detail:  "budget_seconds",
		},
		{
			name:    "module not found",
			log:     "ModuleNotFoundError: No module named 'transformers'",
			pattern: "module_not_found",
			detail:  "missing_module",
		},
		{
			name:    "assert failure",
			log:     "Traceback (most recent call last):\n  File \"train.py\", line 1\nAssertionError: bad batch",
			pattern: "assert_failure",
		},
		{
			name:    "subprocess failure",
			log:     "subprocess.CalledProcessError: Command 'python prep.py' returned non-zero exit status 2",
			pattern: "subprocess_failure",
		},
		{
			name:    "unknown",
			log:     "job failed without a recognizable signature",
			pattern: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DiagnoseFailedAttemptFromLog(tt.log, "backfill")
			if d == nil {
				t.Fatal("expected diagnosis")
			}
			if d.Pattern != tt.pattern {
				t.Fatalf("pattern = %q, want %q", d.Pattern, tt.pattern)
			}
			if d.DetectedBy != "backfill" {
				t.Fatalf("detected_by = %q, want backfill", d.DetectedBy)
			}
			if d.MatchedText == "" {
				t.Fatal("matched_text should be populated")
			}
			if tt.detail != "" {
				if _, ok := d.StructuredDetails[tt.detail]; !ok {
					t.Fatalf("details missing %q: %v", tt.detail, d.StructuredDetails)
				}
			}
		})
	}
}

func TestUnmarshalDiagnosis_LegacyDetailsString(t *testing.T) {
	d, err := UnmarshalDiagnosis(`{"pattern":"gpu_oom","details":"CUDA out of memory","gpu_capacity_gb":24}`)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Details != "CUDA out of memory" {
		t.Fatalf("details = %q", d.Details)
	}
	if d.GPUCapacityGB != 24 {
		t.Fatalf("gpu_capacity_gb = %d", d.GPUCapacityGB)
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

func TestGPUOOMDetailLines(t *testing.T) {
	tests := []struct {
		name string
		d    *ErrorDiagnosis
		want []string
	}{
		{
			name: "nil receiver",
			d:    nil,
			want: nil,
		},
		{
			name: "non-gpu_oom pattern returns nil",
			d: &ErrorDiagnosis{
				Pattern:         "module_not_found",
				GPUOOMProcesses: []GPUOOMProcess{{PID: 1234, MemoryGiB: 12.5}},
			},
			want: nil,
		},
		{
			name: "gpu_oom with empty process list returns nil",
			d: &ErrorDiagnosis{
				Pattern: "gpu_oom",
			},
			want: nil,
		},
		{
			name: "main only — falls back to first process PID when MainPID unset",
			d: &ErrorDiagnosis{
				Pattern:         "gpu_oom",
				GPUOOMProcesses: []GPUOOMProcess{{PID: 4321, MemoryGiB: 22.10}},
			},
			want: []string{"main GPU process pid=4321 using 22.10 GiB"},
		},
		{
			name: "main with explicit MainPID",
			d: &ErrorDiagnosis{
				Pattern:         "gpu_oom",
				GPUOOMMainPID:   12345,
				GPUOOMProcesses: []GPUOOMProcess{{PID: 12345, MemoryGiB: 78.42}},
			},
			want: []string{"main GPU process pid=12345 using 78.42 GiB"},
		},
		{
			name: "main + additional + notes + hint, in order",
			d: &ErrorDiagnosis{
				Pattern:           "gpu_oom",
				GPUOOMMainPID:     12345,
				GPUOOMProcesses:   []GPUOOMProcess{{PID: 12345, MemoryGiB: 78.42}},
				GPUOOMExtraPID:    67890,
				GPUOOMExtraGiB:    2.10,
				GPUOOMNotes:       "shared workspace VRAM contention",
				GPUOOMHintDeltaGB: 8,
			},
			want: []string{
				"main GPU process pid=12345 using 78.42 GiB",
				"additional GPU process pid=67890 using 2.10 GiB",
				"shared workspace VRAM contention",
				"hint: increase --gpu-mem by ~8GB on retry",
			},
		},
		{
			name: "additional process omitted when only PID is set",
			d: &ErrorDiagnosis{
				Pattern:         "gpu_oom",
				GPUOOMProcesses: []GPUOOMProcess{{PID: 1, MemoryGiB: 1.0}},
				GPUOOMExtraPID:  2,
				// GPUOOMExtraGiB == 0
			},
			want: []string{"main GPU process pid=1 using 1.00 GiB"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.d.GPUOOMDetailLines()
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
