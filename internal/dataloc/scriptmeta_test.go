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
			assertStringSlice(t, "Inputs", got.Inputs, tt.want.Inputs)
			assertStringSlice(t, "Outputs", got.Outputs, tt.want.Outputs)
			assertStringSlice(t, "Tags", got.Tags, tt.want.Tags)
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
