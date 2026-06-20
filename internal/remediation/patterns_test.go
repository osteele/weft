package remediation

import "testing"

// patternByID is a position-independent lookup helper for tests. Pattern
// order in dataPatterns matters for runtime priority (first match wins),
// so tests should refer to patterns by ID rather than index.
func patternByID(ps []*pattern, id string) *pattern {
	for _, p := range ps {
		if p.patternID == id {
			return p
		}
	}
	return nil
}

func TestDataPatterns_HFGatedRepo_DiagnosesAuth(t *testing.T) {
	// Real wj2321 failure shape: huggingface_hub raises OSError /
	// GatedRepoError + 401 Client Error against the proxy URL. Before the
	// hf_gated_repo pattern existed, the matcher fell through to the
	// generic `timeout` rule (which matches loose "timed out" tokens that
	// can appear in nested urllib3 tracebacks) — surfacing the failure as
	// "Execution timed out" even though the job died in 3-4 minutes well
	// inside its time budget.
	log := `Traceback (most recent call last):
  ...
  File ".../huggingface_hub/utils/_errors.py", line 333, in hf_raise_for_status
    raise GatedRepoError(message, response) from e
huggingface_hub.errors.GatedRepoError: 401 Client Error. (Request ID: Root=1-abc)

Cannot access gated repo for url http://localhost:8080/meta-llama/Meta-Llama-3-8B/resolve/main/config.json.
Access to model meta-llama/Meta-Llama-3-8B is restricted. You must have access to it and be authenticated to access it. Please log in.

The above exception was the direct cause of the following exception:

  File "scripts/exp_xxx.py", line 42, in <module>
    config = AutoConfig.from_pretrained("meta-llama/Meta-Llama-3-8B")
OSError: You are trying to access a gated repo.`
	d := patternByID(dataPatterns, "hf_gated_repo").Match(log)
	if d == nil {
		t.Fatal("expected gated-repo diagnosis to match")
	}
	if d.Pattern != "hf_gated_repo" {
		t.Errorf("pattern = %q, want hf_gated_repo", d.Pattern)
	}
	if d.Category != "data" {
		t.Errorf("category = %q, want data", d.Category)
	}
	if d.Remediable {
		t.Error("gated-repo isn't remediable by weft — user must request access on huggingface.co")
	}
	// And: the higher-level DiagnoseFromLog should also pick this up,
	// not fall through to the timeout rule.
	if got := DiagnoseFromLog(log); got == nil || got.Pattern != "hf_gated_repo" {
		t.Fatalf("DiagnoseFromLog should classify gated-repo, got %+v", got)
	}
}

func TestDataPatterns_HFGatedRepo_DoesNotMatchBenignTimeout(t *testing.T) {
	// A plain timeout (no gated/401 mention) should still fall through to
	// the `timeout` pattern in failurePatternRules, not get mis-attributed
	// to gated-repo.
	log := `socket.timeout: The read operation timed out
ConnectionError: HTTPSConnectionPool(host='huggingface.co', port=443): Read timed out.`
	d := patternByID(dataPatterns, "hf_gated_repo").Match(log)
	if d != nil {
		t.Fatalf("gated-repo pattern should not match a plain timeout, got %+v", d)
	}
	if got := DiagnoseFromLog(log); got == nil || got.Pattern != "timeout" {
		t.Fatalf("plain timeout should classify as `timeout`, got %+v", got)
	}
}

func TestDataPatterns_HFNetworkCacheMissBeatsSSHDisconnect(t *testing.T) {
	log := `'(ProtocolError('Connection aborted.', ConnectionResetError(54, 'Connection reset by peer')), '(Request ID: 47dbd0f3-eafa-494f-ae31-027b91043797)')' thrown while requesting HEAD https://huggingface.co/microsoft/phi-1/resolve/main/tokenizer_config.json
huggingface_hub.errors.LocalEntryNotFoundError: An error happened while trying to locate the file on the Hub and we cannot find the requested files in the local cache.
OSError: We couldn't connect to 'https://huggingface.co' to load the files, and couldn't find them in the cached files.`

	d := DiagnoseFromLog(log)
	if d == nil {
		t.Fatal("expected HF network/cache-miss diagnosis")
	}
	if d.Pattern != "hf_network_cache_miss" {
		t.Fatalf("pattern = %q, want hf_network_cache_miss", d.Pattern)
	}
	if d.Category != "data" {
		t.Errorf("category = %q, want data", d.Category)
	}
	if len(d.MissingAssets) != 1 || d.MissingAssets[0] != "hf:microsoft/phi-1" {
		t.Errorf("missing assets = %v, want [hf:microsoft/phi-1]", d.MissingAssets)
	}
	if d.Remediable {
		t.Error("network/cache miss should not auto-remediate; the retry may need network or explicit input declaration")
	}
}

func TestDataPatterns_MissingHFModel(t *testing.T) {
	log := `Traceback (most recent call last):
  File "train.py", line 10, in <module>
    model = AutoModelForCausalLM.from_pretrained("meta-llama/Llama-3-8B")
FileNotFoundError: /home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B/snapshots/abc123/model.safetensors`

	d := patternByID(dataPatterns, "missing_hf_model").Match(log)
	if d == nil {
		t.Fatal("expected match for missing HF model")
	}
	if d.Pattern != "missing_hf_model" {
		t.Errorf("expected pattern missing_hf_model, got %s", d.Pattern)
	}
	if d.Category != "data" {
		t.Errorf("expected category data, got %s", d.Category)
	}
	if !d.Remediable {
		t.Error("expected remediable=true")
	}
	if len(d.MissingAssets) != 1 || d.MissingAssets[0] != "hf:meta-llama/Llama-3-8B" {
		t.Errorf("expected missing asset hf:meta-llama/Llama-3-8B, got %v", d.MissingAssets)
	}
}

func TestDataPatterns_MissingHFDataset(t *testing.T) {
	log := `OSError: No such file or directory: /home/user/.cache/huggingface/hub/datasets--squad--squad/snapshots/abc/data.json`

	d := patternByID(dataPatterns, "missing_hf_dataset").Match(log)
	if d == nil {
		t.Fatal("expected match for missing HF dataset")
	}
	if d.Pattern != "missing_hf_dataset" {
		t.Errorf("expected pattern missing_hf_dataset, got %s", d.Pattern)
	}
	if len(d.MissingAssets) != 1 || d.MissingAssets[0] != "hf:squad/squad" {
		t.Errorf("expected missing asset hf:squad/squad, got %v", d.MissingAssets)
	}
}

func TestDataPatterns_MissingFile(t *testing.T) {
	log := `FileNotFoundError: No such file or directory: '/home/user/project/data/config.yaml'`

	d := patternByID(dataPatterns, "missing_file").Match(log)
	if d == nil {
		t.Fatal("expected match for missing file")
	}
	if d.Pattern != "missing_file" {
		t.Errorf("expected pattern missing_file, got %s", d.Pattern)
	}
	if d.MissingAssets != nil {
		t.Errorf("expected nil missing assets for generic file, got %v", d.MissingAssets)
	}
}

func TestCodePatterns_MissingImport(t *testing.T) {
	log := `Traceback (most recent call last):
  File "train.py", line 1, in <module>
    import transformers
ModuleNotFoundError: No module named 'transformers'`

	d := codePatterns[0].Match(log)
	if d == nil {
		t.Fatal("expected match for missing import")
	}
	if d.Pattern != "missing_import" {
		t.Errorf("expected pattern missing_import, got %s", d.Pattern)
	}
	if d.Category != "code" {
		t.Errorf("expected category code, got %s", d.Category)
	}
	if d.Remediable {
		t.Error("expected remediable=false for code patterns")
	}
}

func TestCodePatterns_AttributeError(t *testing.T) {
	log := `AttributeError: 'Model' object has no attribute 'generate_text'`

	d := codePatterns[2].Match(log)
	if d == nil {
		t.Fatal("expected match for attribute error")
	}
	if d.Pattern != "attribute_error" {
		t.Errorf("expected pattern attribute_error, got %s", d.Pattern)
	}
}

func TestCodePatterns_NameError(t *testing.T) {
	log := `NameError: name 'torch' is not defined`

	d := codePatterns[3].Match(log)
	if d == nil {
		t.Fatal("expected match for name error")
	}
	if d.Pattern != "name_error" {
		t.Errorf("expected pattern name_error, got %s", d.Pattern)
	}
}

func TestCodePatterns_SyntaxError(t *testing.T) {
	log := `SyntaxError: unexpected EOF while parsing`

	d := codePatterns[4].Match(log)
	if d == nil {
		t.Fatal("expected match for syntax error")
	}
	if d.Pattern != "syntax_error" {
		t.Errorf("expected pattern syntax_error, got %s", d.Pattern)
	}
}

func TestEnvPatterns_GPUOOM(t *testing.T) {
	log := `torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB`

	d := envPatterns[0].Match(log)
	if d == nil {
		t.Fatal("expected match for GPU OOM")
	}
	if d.Pattern != "gpu_oom" {
		t.Errorf("expected pattern gpu_oom, got %s", d.Pattern)
	}
	if d.Category != "environment" {
		t.Errorf("expected category environment, got %s", d.Category)
	}
}

func TestEnvPatterns_GPUOOMProcessAttribution(t *testing.T) {
	log := `torch.cuda.OutOfMemoryError: CUDA out of memory.
Process 1886134 has 9.18 GiB in use.
Process 1887281 has 2.53 GiB in use.`

	d := envPatterns[0].Match(log)
	if d == nil {
		t.Fatal("expected match for GPU OOM")
	}
	if len(d.GPUOOMProcesses) != 2 {
		t.Fatalf("expected 2 gpu_oom_processes, got %d", len(d.GPUOOMProcesses))
	}
	if d.GPUOOMMainPID != 1886134 {
		t.Errorf("GPUOOMMainPID = %d, want 1886134", d.GPUOOMMainPID)
	}
	if d.GPUOOMExtraPID != 1887281 {
		t.Errorf("GPUOOMExtraPID = %d, want 1887281", d.GPUOOMExtraPID)
	}
	if d.GPUOOMExtraGiB != 2.53 {
		t.Errorf("GPUOOMExtraGiB = %.2f, want 2.53", d.GPUOOMExtraGiB)
	}
	if d.GPUOOMHintDeltaGB != 4 {
		t.Errorf("GPUOOMHintDeltaGB = %d, want 4", d.GPUOOMHintDeltaGB)
	}
	if d.GPUOOMNotes == "" {
		t.Error("expected GPUOOMNotes to be populated")
	}
}

func TestEnvPatterns_CUDAError(t *testing.T) {
	log := `RuntimeError: CUDA error: device-side assert triggered`

	d := envPatterns[1].Match(log)
	if d == nil {
		t.Fatal("expected match for CUDA error")
	}
	if d.Pattern != "cuda_error" {
		t.Errorf("expected pattern cuda_error, got %s", d.Pattern)
	}
}

func TestEnvPatterns_DiskFull_ENOSPC(t *testing.T) {
	log := `write /tmp/output/model.bin: ENOSPC`

	d := envPatterns[3].Match(log)
	if d == nil {
		t.Fatal("expected match for ENOSPC")
	}
	if d.Pattern != "disk_full" {
		t.Errorf("expected pattern disk_full, got %s", d.Pattern)
	}
	if d.Category != "environment" {
		t.Errorf("expected category environment, got %s", d.Category)
	}
}

func TestEnvPatterns_DiskFull_NoSpaceLeft(t *testing.T) {
	log := `OSError: [Errno 28] No space left on device: '/tmp/model/config.json'`

	d := envPatterns[3].Match(log)
	if d == nil {
		t.Fatal("expected match for No space left on device")
	}
	if d.Pattern != "disk_full" {
		t.Errorf("expected pattern disk_full, got %s", d.Pattern)
	}
}

func TestDiagnoseFailedAttempt_RuntimeGuidance(t *testing.T) {
	cases := []struct {
		name        string
		log         string
		wantPattern string
	}{
		{
			name:        "unusable temp dir",
			log:         "FileNotFoundError: No usable temporary directory found in ['/tmp', '/var/tmp', '/workspace/project']",
			wantPattern: "tempdir_unusable",
		},
		{
			name:        "sglang setup",
			log:         "ImportError: libnuma.so.1: cannot open shared object file while importing sglang",
			wantPattern: "sglang_setup",
		},
		{
			name:        "vllm setup",
			log:         "ModuleNotFoundError: No module named 'vllm'",
			wantPattern: "vllm_setup",
		},
		{
			name: "pep723 console script missing",
			log: `+ uv run python serve.py
Traceback (most recent call last):
  File "serve.py", line 17, in <module>
    subprocess.run(["vllm", "serve"], check=True)
FileNotFoundError: [Errno 2] No such file or directory: 'vllm'`,
			wantPattern: "pep723_console_script_missing",
		},
		{
			name:        "cli argument drift",
			log:         "usage: vllm serve [-h]\nvllm: error: unrecognized arguments: --guided-decoding-backend",
			wantPattern: "cli_argument_drift",
		},
		{
			name: "attribute version mismatch",
			log: `Traceback (most recent call last):
  File "bench.py", line 12, in <module>
    tokenizer = AutoTokenizer.from_pretrained(model)
AttributeError: 'LlamaTokenizerFast' object has no attribute 'tokenizer_config'`,
			wantPattern: "python_attribute_version_mismatch",
		},
		{
			name:        "cuda image wheel mismatch",
			log:         "vllm engine core failed to initialize: CUDA wheel cu128 is incompatible with image CUDA 12.4",
			wantPattern: "cuda_image_wheel_mismatch",
		},
		{
			name: "cuda driver too old",
			log: `RuntimeError: The NVIDIA driver on your system is too old (found version 12040).
Please update your GPU driver by downloading and installing a new version from the URL: http://www.nvidia.com/Download/index.aspx`,
			wantPattern: "cuda_driver_too_old",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DiagnoseFailedAttemptFromLog(tc.log, "post")
			if d == nil {
				t.Fatal("expected diagnosis")
			}
			if d.Pattern != tc.wantPattern {
				t.Fatalf("pattern = %q, want %q", d.Pattern, tc.wantPattern)
			}
			if d.Solution == "" {
				t.Fatal("expected solution guidance")
			}
			if tc.wantPattern == "cuda_driver_too_old" && d.StructuredDetails["found_cuda_compatibility"] != "12.4" {
				t.Fatalf("found_cuda_compatibility = %#v, want 12.4", d.StructuredDetails["found_cuda_compatibility"])
			}
		})
	}
}

func TestNoMatch(t *testing.T) {
	log := `Training completed successfully in 3h 42m`

	for _, p := range dataPatterns {
		if d := p.Match(log); d != nil {
			t.Errorf("unexpected match from data pattern %s", p.patternID)
		}
	}
	for _, p := range codePatterns {
		if d := p.Match(log); d != nil {
			t.Errorf("unexpected match from code pattern %s", p.patternID)
		}
	}
	for _, p := range envPatterns {
		if d := p.Match(log); d != nil {
			t.Errorf("unexpected match from env pattern %s", p.patternID)
		}
	}
}
