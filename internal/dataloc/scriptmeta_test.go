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
			name: "vast cap-add",
			content: `# /// script
# [tool.weft]
# vast-cap-add = ["sys_admin", "NET_ADMIN", "SYS_ADMIN", ""]
# ///
`,
			want: &ScriptMeta{VastCapAdd: []string{"SYS_ADMIN", "NET_ADMIN"}},
		},
		{
			name: "uv-args",
			content: `# /// script
# [tool.weft]
# uv-args = ["--system"]
# ///
`,
			want: &ScriptMeta{UvArgs: []string{"--system"}},
		},
		{
			name: "uv-args with gpu",
			content: `# /// script
# [tool.weft]
# gpu = "nvidia>=20GB"
# uv-args = ["--system", "--no-cache"]
# ///
`,
			want: &ScriptMeta{GPU: "nvidia>=20GB", UvArgs: []string{"--system", "--no-cache"}},
		},
		{
			name: "env vars",
			content: `# /// script
# [tool.weft]
# [tool.weft.env]
# UV_SYSTEM_PYTHON = "1"
# ///
`,
			want: &ScriptMeta{Env: map[string]string{"UV_SYSTEM_PYTHON": "1"}},
		},
		{
			name: "env vars with other fields",
			content: `# /// script
# [tool.weft]
# gpu-mem = 40
# image = "nvcr.io/nvidia/tensorrt-llm/release:1.3.0rc10"
# [tool.weft.env]
# UV_SYSTEM_PYTHON = "1"
# CUDA_HOME = "/usr/local/cuda"
# ///
`,
			want: &ScriptMeta{
				GPUMemGB: 40,
				Image:    "nvcr.io/nvidia/tensorrt-llm/release:1.3.0rc10",
				Env:      map[string]string{"UV_SYSTEM_PYTHON": "1", "CUDA_HOME": "/usr/local/cuda"},
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
		{
			name: "pre-install",
			content: `# /// script
# [tool.weft]
# gpu = "nvidia>=20GB"
# pre-install = "apt-get update && apt-get install -y libnuma-dev"
# ///
`,
			want: &ScriptMeta{
				GPU:        "nvidia>=20GB",
				PreInstall: "apt-get update && apt-get install -y libnuma-dev",
			},
		},
		{
			name: "preemptible",
			content: `# /// script
# [tool.weft]
# preemptible = true
# ///
`,
			want: &ScriptMeta{
				Preemptible: true,
			},
		},
		{
			name: "interruptible",
			content: `# /// script
# [tool.weft]
# interruptible = true
# ///
`,
			want: &ScriptMeta{
				Preemptible: true,
			},
		},
		{
			name: "tool.uv index-url",
			content: `# /// script
# dependencies = ["sglang"]
# [tool.uv]
# index-url = "https://docs.sglang.ai/whl/cu124"
# ///
`,
			want: &ScriptMeta{
				Env: map[string]string{"UV_INDEX_URL": "https://docs.sglang.ai/whl/cu124"},
			},
		},
		{
			name: "tool.uv extra-index-url array",
			content: `# /// script
# dependencies = ["sglang"]
# [tool.uv]
# index-url = "https://docs.sglang.ai/whl/cu124"
# extra-index-url = ["https://pypi.org/simple", "https://flashinfer.ai/whl/cu124"]
# ///
`,
			want: &ScriptMeta{
				Env: map[string]string{
					"UV_INDEX_URL":       "https://docs.sglang.ai/whl/cu124",
					"UV_EXTRA_INDEX_URL": "https://pypi.org/simple https://flashinfer.ai/whl/cu124",
				},
			},
		},
		{
			name: "tool.uv extra-index-url string",
			content: `# /// script
# dependencies = ["sglang"]
# [tool.uv]
# extra-index-url = "https://pypi.org/simple"
# ///
`,
			want: &ScriptMeta{
				Env: map[string]string{"UV_EXTRA_INDEX_URL": "https://pypi.org/simple"},
			},
		},
		{
			name: "tool.weft.env overrides tool.uv",
			content: `# /// script
# dependencies = ["sglang"]
# [tool.uv]
# index-url = "https://docs.sglang.ai/whl/cu124"
# [tool.weft]
# gpu = "nvidia"
# [tool.weft.env]
# UV_INDEX_URL = "https://custom.example.com/simple"
# ///
`,
			want: &ScriptMeta{
				GPU: "nvidia",
				Env: map[string]string{"UV_INDEX_URL": "https://custom.example.com/simple"},
			},
		},
		{
			name: "tool.uv only (no tool.weft)",
			content: `# /// script
# dependencies = ["sglang"]
# [tool.uv]
# index-url = "https://docs.sglang.ai/whl/cu124"
# ///
`,
			want: &ScriptMeta{
				Env: map[string]string{"UV_INDEX_URL": "https://docs.sglang.ai/whl/cu124"},
			},
		},
		{
			name: "full sglang-style script",
			content: `# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = ["sglang[srt]>=0.4", "pynvml>=12.0"]
# [tool.uv]
# index-url = "https://docs.sglang.ai/whl/cu124"
# extra-index-url = ["https://pypi.org/simple", "https://flashinfer.ai/whl/cu124/torch2.5/flashinfer-python"]
# [tool.weft]
# gpu = "nvidia>=20GB"
# image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"
# inputs = ["hf:gpt2"]
# pre-install = "apt-get update && apt-get install -y libnuma-dev"
# ///
`,
			want: &ScriptMeta{
				GPU:        "nvidia>=20GB",
				Image:      "nvidia/cuda:12.4.1-devel-ubuntu22.04",
				Inputs:     []string{"hf:gpt2"},
				PreInstall: "apt-get update && apt-get install -y libnuma-dev",
				Env: map[string]string{
					"UV_INDEX_URL":       "https://docs.sglang.ai/whl/cu124",
					"UV_EXTRA_INDEX_URL": "https://pypi.org/simple https://flashinfer.ai/whl/cu124/torch2.5/flashinfer-python",
				},
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
			assertStringSlice(t, "VastCapAdd", got.VastCapAdd, tt.want.VastCapAdd)
			assertStringSlice(t, "UvArgs", got.UvArgs, tt.want.UvArgs)
			assertStringMap(t, "Env", got.Env, tt.want.Env)
			if got.PreInstall != tt.want.PreInstall {
				t.Errorf("PreInstall: got %q, want %q", got.PreInstall, tt.want.PreInstall)
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

func TestInjectUvArgs(t *testing.T) {
	tests := []struct {
		name    string
		command string
		uvArgs  []string
		want    string
	}{
		{
			name:    "basic injection",
			command: "uv run script.py",
			uvArgs:  []string{"--system"},
			want:    "uv run --system script.py",
		},
		{
			name:    "multiple args",
			command: "uv run script.py",
			uvArgs:  []string{"--system", "--no-cache"},
			want:    "uv run --system --no-cache script.py",
		},
		{
			name:    "no uv run",
			command: "python script.py",
			uvArgs:  []string{"--system"},
			want:    "python script.py",
		},
		{
			name:    "compound command",
			command: "pip install foo && uv run script.py",
			uvArgs:  []string{"--system"},
			want:    "pip install foo && uv run --system script.py",
		},
		{
			name:    "empty args",
			command: "uv run script.py",
			uvArgs:  nil,
			want:    "uv run script.py",
		},
		{
			name:    "uv run with existing flags",
			command: "uv run --python 3.11 script.py",
			uvArgs:  []string{"--system"},
			want:    "uv run --system --python 3.11 script.py",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InjectUvArgs(tt.command, tt.uvArgs)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyUvArgs(t *testing.T) {
	tests := []struct {
		name    string
		command string
		uvArgs  []string
		want    string
	}{
		{
			name:    "existing uv run",
			command: "uv run script.py",
			uvArgs:  []string{"--system"},
			want:    "uv run --system script.py",
		},
		{
			name:    "python script rewritten",
			command: "python script.py",
			uvArgs:  []string{"--system"},
			want:    "uv run --system script.py",
		},
		{
			name:    "python with flags preserved",
			command: "python -u script.py --epochs 10",
			uvArgs:  []string{"--system"},
			want:    "uv run --system python -u script.py --epochs 10",
		},
		{
			name:    "python3 script rewritten",
			command: "python3 script.py",
			uvArgs:  []string{"--system"},
			want:    "uv run --system script.py",
		},
		{
			name:    "compound command unchanged",
			command: "pip install foo && python script.py",
			uvArgs:  []string{"--system"},
			want:    "pip install foo && python script.py",
		},
		{
			name:    "empty args unchanged",
			command: "python script.py",
			uvArgs:  nil,
			want:    "python script.py",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyUvArgs(tt.command, tt.uvArgs)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseScriptMeta_Isolated(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "isolated true",
			content: `# /// script
# dependencies = ["vllm>=0.17"]
# [tool.weft]
# gpu = "nvidia>=20GB"
# isolated = true
# ///
`,
			want: true,
		},
		{
			name: "isolated false",
			content: `# /// script
# dependencies = ["torch"]
# [tool.weft]
# gpu = "nvidia"
# isolated = false
# ///
`,
			want: false,
		},
		{
			name: "no isolated key",
			content: `# /// script
# dependencies = ["torch"]
# [tool.weft]
# gpu = "nvidia"
# ///
`,
			want: false,
		},
		{
			name: "isolated only weft field",
			content: `# /// script
# [tool.weft]
# isolated = true
# ///
`,
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := ParseScriptMeta(tt.content)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if meta == nil {
				if tt.want {
					t.Fatal("got nil meta, want Isolated=true")
				}
				return
			}
			if meta.Isolated != tt.want {
				t.Errorf("Isolated = %v, want %v", meta.Isolated, tt.want)
			}
		})
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

func assertStringMap(t *testing.T, name string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", name, got, want)
		return
	}
	for k, wv := range want {
		if gv, ok := got[k]; !ok || gv != wv {
			t.Errorf("%s[%q]: got %q, want %q", name, k, gv, wv)
		}
	}
}
