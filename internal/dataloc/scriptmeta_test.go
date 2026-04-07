package dataloc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseScriptMeta(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *ScriptMeta
	}{
		{
			name: "full metadata",
			content: `#!/usr/bin/env python3
# /// script
# [tool.weft]
# gpu = "nvidia>=24GB"
# inputs = ["hf:gpt2", "hf:bert-base-uncased"]
# tags = ["benchmark"]
# ///

import torch
`,
			want: &ScriptMeta{
				GPU:    "nvidia>=24GB",
				Inputs: []string{"hf:gpt2", "hf:bert-base-uncased"},
				Tags:   []string{"benchmark"},
			},
		},
		{
			name: "gpu-mem integer",
			content: `# /// script
# [tool.weft]
# gpu-mem = 40
# ///
`,
			want: &ScriptMeta{GPUMemGB: 40},
		},
		{
			name: "gpu-mem string with >= and GB",
			content: `# /// script
# [tool.weft]
# gpu-mem = ">=80GB"
# ///
`,
			want: &ScriptMeta{GPUMemGB: 80},
		},
		{
			name: "gpu-mem strict",
			content: `# /// script
# [tool.weft]
# gpu-mem = 8
# gpu-mem-strict = true
# ///
`,
			want: &ScriptMeta{GPUMemGB: 8, GPUMemStrict: boolPtr(true)},
		},
		{
			name:    "no metadata block",
			content: `import torch\nprint("hello")\n`,
			want:    nil,
		},
		{
			name: "script block without tool.weft",
			content: `# /// script
# requires-python = ">=3.10"
# dependencies = ["torch"]
# ///
`,
			want: nil,
		},
		{
			name: "gpu-class key",
			content: `# /// script
# [tool.weft]
# gpu-class = "ampere+"
# ///
`,
			want: &ScriptMeta{GPUClass: "ampere+"},
		},
		{
			name: "local: inputs with outputs",
			content: `# /// script
# [tool.weft]
# inputs = ["local:data/conllu/", "hf:gpt2"]
# outputs = ["local:cache/representations/"]
# ///
`,
			want: &ScriptMeta{
				Inputs:  []string{"local:data/conllu/", "hf:gpt2"},
				Outputs: []string{"local:cache/representations/"},
			},
		},
		{
			name: "corpus input",
			content: `# /// script
# [tool.weft]
# gpu-mem = 24
# inputs = ["hf:bert-base-uncased", "corpus:penn-treebank/conllu"]
# ///
`,
			want: &ScriptMeta{
				GPUMemGB: 24,
				Inputs:   []string{"hf:bert-base-uncased", "corpus:penn-treebank/conllu"},
			},
		},
		{
			name: "image field",
			content: `# /// script
# [tool.weft]
# image = "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
# ///
`,
			want: &ScriptMeta{Image: "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"},
		},
		{
			name: "image with gpu-mem",
			content: `# /// script
# [tool.weft]
# gpu-mem = 40
# image = "nvcr.io/nvidia/pytorch:23.10-py3"
# inputs = ["hf:gpt2"]
# ///
`,
			want: &ScriptMeta{
				GPUMemGB: 40,
				Image:    "nvcr.io/nvidia/pytorch:23.10-py3",
				Inputs:   []string{"hf:gpt2"},
			},
		},
		{
			name: "mixed with uv dependencies",
			content: `# /// script
# requires-python = ">=3.10"
# dependencies = ["torch", "numpy"]
#
# [tool.weft]
# gpu-mem = 24
# inputs = ["hf:gpt2"]
# ///
`,
			want: &ScriptMeta{
				GPUMemGB: 24,
				Inputs:   []string{"hf:gpt2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScriptMeta(tt.content)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %+v, got nil", tt.want)
			}
			if got.GPU != tt.want.GPU {
				t.Errorf("GPU: got %q, want %q", got.GPU, tt.want.GPU)
			}
			if got.GPUClass != tt.want.GPUClass {
				t.Errorf("GPUClass: got %q, want %q", got.GPUClass, tt.want.GPUClass)
			}
			if got.GPUMemGB != tt.want.GPUMemGB {
				t.Errorf("GPUMemGB: got %d, want %d", got.GPUMemGB, tt.want.GPUMemGB)
			}
			if !equalBoolPtr(got.GPUMemStrict, tt.want.GPUMemStrict) {
				t.Errorf("GPUMemStrict: got %v, want %v", got.GPUMemStrict, tt.want.GPUMemStrict)
			}
			assertStringSlice(t, "Inputs", got.Inputs, tt.want.Inputs)
			assertStringSlice(t, "Outputs", got.Outputs, tt.want.Outputs)
			assertStringSlice(t, "Tags", got.Tags, tt.want.Tags)
			if got.Image != tt.want.Image {
				t.Errorf("Image: got %q, want %q", got.Image, tt.want.Image)
			}
		})
	}
}

func TestScanScriptMeta(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	os.WriteFile(script, []byte(`# /// script
# [tool.weft]
# gpu-mem = 40
# inputs = ["hf:gpt2"]
# ///

import torch
`), 0o644)

	meta, err := ScanScriptMeta(dir, "uv run python train.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta == nil {
		t.Fatal("expected metadata, got nil")
	}
	if meta.GPUMemGB != 40 {
		t.Errorf("GPUMemGB: got %d, want 40", meta.GPUMemGB)
	}
	if len(meta.Inputs) != 1 || meta.Inputs[0] != "hf:gpt2" {
		t.Errorf("Inputs: got %v, want [hf:gpt2]", meta.Inputs)
	}
}

func TestScanScriptMetaWithImage(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	os.WriteFile(script, []byte(`# /// script
# [tool.weft]
# gpu-mem = 40
# image = "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
# ///
`), 0o644)

	meta, err := ScanScriptMeta(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta == nil {
		t.Fatal("expected metadata, got nil")
	}
	if meta.Image != "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime" {
		t.Errorf("Image: got %q, want %q", meta.Image, "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime")
	}
	if meta.GPUMemGB != 40 {
		t.Errorf("GPUMemGB: got %d, want 40", meta.GPUMemGB)
	}
}

func TestScanScriptMetaNoScript(t *testing.T) {
	meta, err := ScanScriptMeta(t.TempDir(), "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta != nil {
		t.Fatalf("expected nil, got %+v", meta)
	}
}

func assertStringSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", name, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %q, want %q", name, i, got[i], want[i])
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func equalBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
