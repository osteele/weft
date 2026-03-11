package dataloc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPyScanPythonHFRefs(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "a.py", `
from transformers import AutoModel
from datasets import load_dataset

tokenizer = AutoModel.from_pretrained("gpt2")
model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")
dataset = load_dataset("wikitext")
# ignored = AutoModel.from_pretrained("commented/model")
local = AutoModel.from_pretrained("./checkpoints/run-1")
`)
	writePyFile(t, dir, "b.py", `
from huggingface_hub import snapshot_download
from sentence_transformers import SentenceTransformer

cache_path = snapshot_download("google/gemma-2b")
encoder = SentenceTransformer("BAAI/bge-small-en-v1.5")
`)
	writePyFile(t, dir, "c.py", `
import argparse

parser = argparse.ArgumentParser()
parser.add_argument(
    "--model",
    type=str,
    default="Qwen/Qwen2.5-7B-Instruct",
)
parser.add_argument("--base-model", default="google/gemma-2b")
`)

	refs := ScanPythonHFRefs(dir)
	assertSetEqual(t, refs, []string{
		"hf:gpt2",
		"hf:meta-llama/Llama-3-8B",
		"hf-dataset:wikitext",
		"hf:google/gemma-2b",
		"hf:BAAI/bge-small-en-v1.5",
		"hf:Qwen/Qwen2.5-7B-Instruct",
	})
}

func TestPyScanPythonHFRefs_DedupAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "a.py", `model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")`)
	writePyFile(t, dir, "b.py", `model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")`)

	refs := ScanPythonHFRefs(dir)
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1; refs=%v", len(refs), refs)
	}
	if refs[0] != "hf:meta-llama/Llama-3-8B" {
		t.Fatalf("refs[0] = %q, want hf:meta-llama/Llama-3-8B", refs[0])
	}
}

func TestPyScanPythonHFRefs_EmptyAndMissingDir(t *testing.T) {
	emptyDir := t.TempDir()
	if refs := ScanPythonHFRefs(emptyDir); len(refs) != 0 {
		t.Fatalf("empty dir refs = %v, want empty", refs)
	}
	if refs := ScanPythonHFRefs(filepath.Join(emptyDir, "does-not-exist")); len(refs) != 0 {
		t.Fatalf("missing dir refs = %v, want empty", refs)
	}
}

func TestPyScanPythonHFRefs_TestdataInferenceScript(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "campaign", "ml-inference")
	refs := ScanPythonHFRefs(dir)
	assertContains(t, refs, "hf:distilbert-base-uncased")
}

func TestPyScanCommandHFRefs(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "model flag with value",
			command: "uv run python train.py --model Qwen/Qwen2.5-7B-Instruct",
			want:    []string{"hf:Qwen/Qwen2.5-7B-Instruct"},
		},
		{
			name:    "equals and quoted values",
			command: `python train.py --model-name=meta-llama/Llama-3-8B --base-model "google/gemma-2b"`,
			want:    []string{"hf:meta-llama/Llama-3-8B", "hf:google/gemma-2b"},
		},
		{
			name:    "reject local and cache paths",
			command: "python train.py --model ./checkpoints/x --base-model output/model",
			want:    nil,
		},
		{
			name:    "reject bare model name in command parser",
			command: "python train.py --model gpt2",
			want:    nil,
		},
		{
			name:    "deduplicate repeated flag values",
			command: "python train.py --model Qwen/Qwen2.5-7B-Instruct --model-name Qwen/Qwen2.5-7B-Instruct",
			want:    []string{"hf:Qwen/Qwen2.5-7B-Instruct"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScanCommandHFRefs(tt.command)
			assertSetEqual(t, got, tt.want)
		})
	}
}

func TestPyScanIsHFModelID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"gpt2", true},
		{"meta-llama/Llama-3-8B", true},
		{"./model", false},
		{"/abs/model", false},
		{"~/model", false},
		{"org/model/extra", false},
		{"cache/model", false},
		{"output/model", false},
		{"checkpoint/run-1", false},
		{`org\model`, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isHFModelID(tt.input); got != tt.want {
				t.Fatalf("isHFModelID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func writePyFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, item := range got {
		if item == want {
			return
		}
	}
	t.Fatalf("missing %q in %v", want, got)
}

func assertSetEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d; got=%v want=%v", len(got), len(want), got, want)
	}
	counts := map[string]int{}
	for _, s := range got {
		counts[s]++
	}
	for _, s := range want {
		counts[s]--
	}
	for key, v := range counts {
		if v != 0 {
			t.Fatalf("set mismatch for %q: delta=%d; got=%v want=%v", key, v, got, want)
		}
	}
}
