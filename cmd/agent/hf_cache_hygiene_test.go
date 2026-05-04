package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestPurgeUndeclaredHFAssets(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	hubDir := filepath.Join(tmp, ".cache", "huggingface", "hub")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Three model dirs: one declared, one undeclared, one unrecognized prefix.
	mustWrite := func(rel, content string) {
		full := filepath.Join(hubDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("models--Qwen--Qwen2.5-7B/snapshots/abc/blob", "x")      // declared, keep
	mustWrite("models--EleutherAI--pythia-410m/snapshots/d/blob", "y") // undeclared, purge
	mustWrite("datasets--openai--ih-challenge/snapshots/e/blob", "z")  // undeclared, purge
	mustWrite(".locks/random-lockfile", "lock")                        // not models--/datasets--, ignore

	declared := []string{"hf:Qwen/Qwen2.5-7B"}
	purged, freed := purgeUndeclaredHFAssets(declared)
	if purged != 2 {
		t.Fatalf("purged=%d, want 2", purged)
	}
	if freed <= 0 {
		t.Fatalf("freed=%d, want >0", freed)
	}

	// Verify what's left.
	remaining, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, e := range remaining {
		names[e.Name()] = true
	}
	if !names["models--Qwen--Qwen2.5-7B"] {
		t.Errorf("declared asset was removed; have %v", names)
	}
	if names["models--EleutherAI--pythia-410m"] {
		t.Errorf("undeclared model still present; have %v", names)
	}
	if names["datasets--openai--ih-challenge"] {
		t.Errorf("undeclared dataset still present; have %v", names)
	}
	if !names[".locks"] {
		t.Errorf("non-asset directory was removed; have %v", names)
	}
}

func TestPurgeUndeclaredHFAssets_EmptyDeclared(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	hubDir := filepath.Join(tmp, ".cache", "huggingface", "hub", "models--Qwen--Qwen2.5-7B")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// With no declared inputs, everything is "undeclared". This is intentional:
	// callers gate on len(declaredInputs) > 0 in production.
	purged, _ := purgeUndeclaredHFAssets(nil)
	if purged != 1 {
		t.Fatalf("with empty declared, purged=%d, want 1", purged)
	}
}

func TestPurgeUndeclaredHFAssets_MissingDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// No HF cache dir at all.
	purged, freed := purgeUndeclaredHFAssets([]string{"hf:foo"})
	if purged != 0 || freed != 0 {
		t.Fatalf("missing dir should be no-op, got purged=%d freed=%d", purged, freed)
	}
}

func TestCollectDeclaredInputs(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, Inputs: []string{"hf:A", "hf:B"}},
		{ID: 2, Inputs: []string{"hf:B", "hf:C"}},
		{ID: 3, Inputs: nil},
	}
	got := collectDeclaredInputs(jobs)
	want := map[string]bool{"hf:A": true, "hf:B": true, "hf:C": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want set %v", got, want)
	}
	for _, in := range got {
		if !want[in] {
			t.Errorf("unexpected input %q", in)
		}
	}
}
