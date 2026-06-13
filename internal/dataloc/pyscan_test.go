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

func TestPyScanPythonHFRefs_IgnoresNearbyNonModelDefaults(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "a.py", `
import argparse

parser = argparse.ArgumentParser()
parser.add_argument("--model", default="gpt2")
parser.add_argument("--device", default="cuda", choices=["cuda", "mps", "cpu"])
parser.add_argument("--mode", default="both")
parser.add_argument("--output-dir", default="outputs")
`)

	refs := ScanPythonHFRefs(dir)
	assertSetEqual(t, refs, []string{"hf:gpt2"})
}

func TestPyScanPythonHFRefs_IgnoresMIMETypes(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "a.py", `
HEADERS = {
    "Content-Type": "application/json",
    "Accept": "text/plain",
}

MODEL_CONFIGS = {
    "base": "meta-llama/Llama-3-8B",
}
`)

	refs := ScanPythonHFRefs(dir)
	assertSetEqual(t, refs, nil)
}

func TestPyScanPythonHFRefs_ModelArgBlockWithSlashDefault(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "a.py", `
import argparse

parser = argparse.ArgumentParser()
parser.add_argument(
    "--base-model",
    type=str,
    default="meta-llama/Llama-3-8B",
    help="Base model",
)
`)

	refs := ScanPythonHFRefs(dir)
	assertSetEqual(t, refs, []string{"hf:meta-llama/Llama-3-8B"})
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
		{"application/json", false},
		{"text/plain", false},
		{`org\model`, false},
		{"N/M", false},
		{"A/B", false},
		{"X/Y", false},
		{"a/b", false},
		{"ab/c", true},
		{"BAAI/bge-small-en-v1.5", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := IsHFModelID(tt.input); got != tt.want {
				t.Fatalf("IsHFModelID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestPyScanPythonHFRefs_IgnoresShortMathNotation(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "config.py", `
SPARSITY_PATTERNS = {
    "default": "N/M",
    "alt": "A/B",
}

MODEL_CONFIGS = {
    "base": "meta-llama/Llama-3-8B",
}
`)
	refs := ScanPythonHFRefs(dir)
	assertSetEqual(t, refs, nil)
}

func TestPyScanPythonHFRefs_IgnoresAmbiguousDictValues(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "config.py", `
MODEL_CONFIGS = {
    "pythia-1.4b": "EleutherAI/pythia-1.4b",
    "llama-8b": "meta-llama/Llama-3-8B",
    "gpt2": "openai/gpt2",
    "local_only": "gpt2",
    "some_key": "not-a-model",
}
`)
	refs := ScanPythonHFRefs(dir)
	assertSetEqual(t, refs, nil)
}

func TestExtractPythonScripts(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"simple", "python train.py", []string{"train.py"}},
		{"uv run", "uv run python foo.py", []string{"foo.py"}},
		{"uv run without python", "uv run train.py", []string{"train.py"}},
		{"uv run with flags", "uv run --with numpy train.py", []string{"train.py"}},
		{"with flags", "python -u train.py --epochs 10", []string{"train.py"}},
		{"subdirectory", "python experiments/train.py", []string{"experiments/train.py"}},
		{"no py", "bash run.sh", nil},
		{"module mode", "python -m pytest", nil},
		{"multiple scripts", "python setup.py && python train.py", []string{"setup.py", "train.py"}},
		{"env var assignment", "CONFIG=train.py python other.py", []string{"other.py"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPythonScripts(tt.command)
			assertSetEqual(t, got, tt.want)
		})
	}
}

func TestExtractPythonScriptsInDirResolvesLocalModules(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "scripts/exp032_gpt2_medium_probe_sweep.py", "")
	writePyFile(t, dir, "pkg/runner/__main__.py", "")

	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"python module", "python -m scripts.exp032_gpt2_medium_probe_sweep", []string{"scripts/exp032_gpt2_medium_probe_sweep.py"}},
		{"uv python module", "uv run python -m scripts.exp032_gpt2_medium_probe_sweep", []string{"scripts/exp032_gpt2_medium_probe_sweep.py"}},
		{"python flags before module", "python -u -m scripts.exp032_gpt2_medium_probe_sweep --epochs 10", []string{"scripts/exp032_gpt2_medium_probe_sweep.py"}},
		{"package main", "python -m pkg.runner", []string{"pkg/runner/__main__.py"}},
		{"third-party module", "python -m pytest", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPythonScriptsInDir(dir, tt.command)
			assertSetEqual(t, got, tt.want)
		})
	}
}

func TestScanPythonHFRefsForCommand_ScopesToScript(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "train.py", `
model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")
`)
	writePyFile(t, dir, "other.py", `
model = AutoModel.from_pretrained("google/gemma-2b")
`)

	refs := ScanPythonHFRefsForCommand(dir, "python train.py")
	assertSetEqual(t, refs, []string{"hf:meta-llama/Llama-3-8B"})
}

func TestScanPythonHFRefsForCommand_FollowsLocalImports(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "train.py", `
from _utils import load_model
model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")
`)
	writePyFile(t, dir, "_utils.py", `
tokenizer = AutoModel.from_pretrained("meta-llama/Llama-3-8B-Tokenizer")
`)
	writePyFile(t, dir, "unrelated.py", `
model = AutoModel.from_pretrained("google/gemma-2b")
`)

	refs := ScanPythonHFRefsForCommand(dir, "python train.py")
	assertSetEqual(t, refs, []string{
		"hf:meta-llama/Llama-3-8B",
		"hf:meta-llama/Llama-3-8B-Tokenizer",
	})
}

func TestScanPythonHFRefsForCommand_SubdirectoryScript(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "experiments/train.py", `
from _utils import helper
model = AutoModel.from_pretrained("EleutherAI/pythia-1.4b")
`)
	writePyFile(t, dir, "experiments/_utils.py", `
TOKENIZER = "EleutherAI/pythia-1.4b"
`)

	refs := ScanPythonHFRefsForCommand(dir, "python experiments/train.py")
	assertContains(t, refs, "hf:EleutherAI/pythia-1.4b")
}

func TestScanPythonHFRefsForCommand_IgnoresInstalledPackages(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "train.py", `
import transformers
model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")
`)
	// No transformers.py exists in dir — should not error

	refs := ScanPythonHFRefsForCommand(dir, "python train.py")
	assertSetEqual(t, refs, []string{"hf:meta-llama/Llama-3-8B"})
}

func TestScanPythonHFRefsForCommand_NoPyInCommand(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "train.py", `
model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")
`)

	refs := ScanPythonHFRefsForCommand(dir, "bash run.sh")
	if refs != nil {
		t.Fatalf("expected nil, got %v", refs)
	}
}

func TestScanPythonHFRefsForCommand_DottedImport(t *testing.T) {
	dir := t.TempDir()
	writePyFile(t, dir, "train.py", `
from experiments._utils import helper
model = AutoModel.from_pretrained("meta-llama/Llama-3-8B")
`)
	writePyFile(t, dir, "experiments/_utils.py", `
extra = AutoModel.from_pretrained("google/gemma-2b")
`)

	refs := ScanPythonHFRefsForCommand(dir, "python train.py")
	assertSetEqual(t, refs, []string{
		"hf:meta-llama/Llama-3-8B",
		"hf:google/gemma-2b",
	})
}

func writePyFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
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
