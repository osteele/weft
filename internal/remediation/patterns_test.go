package remediation

import "testing"

func TestDataPatterns_MissingHFModel(t *testing.T) {
	log := `Traceback (most recent call last):
  File "train.py", line 10, in <module>
    model = AutoModelForCausalLM.from_pretrained("meta-llama/Llama-3-8B")
FileNotFoundError: /home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B/snapshots/abc123/model.safetensors`

	d := dataPatterns[0].Match(log)
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

	d := dataPatterns[1].Match(log)
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

	d := dataPatterns[2].Match(log)
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
