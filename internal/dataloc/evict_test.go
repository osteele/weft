package dataloc

import (
	"testing"
	"time"
)

func TestParseHFCacheAtimeOutput_GNUFind(t *testing.T) {
	// find -printf '%A@\t%s\t%p\n' output (GNU find, Linux)
	output := "1710000000.0\t14284561408\t/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B\n" +
		"1700000000.5\t1048576000\t/home/user/.cache/huggingface/hub/datasets--wikitext\n"

	entries := parseHFCacheAtimeOutput(output, "cool100")
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	e := entries[0]
	if e.Host != "cool100" {
		t.Errorf("Host = %q, want %q", e.Host, "cool100")
	}
	if e.Asset.Kind != AssetHFModel {
		t.Errorf("Kind = %q, want %q", e.Asset.Kind, AssetHFModel)
	}
	if e.Asset.ID != "meta-llama/Llama-3-8B" {
		t.Errorf("ID = %q, want %q", e.Asset.ID, "meta-llama/Llama-3-8B")
	}
	if e.SizeBytes != 14284561408 {
		t.Errorf("SizeBytes = %d, want 14284561408", e.SizeBytes)
	}
	wantAtime := time.Unix(1710000000, 0)
	if !e.LastAccessed.Equal(wantAtime) {
		t.Errorf("LastAccessed = %v, want %v", e.LastAccessed, wantAtime)
	}
	if e.Path != "/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B" {
		t.Errorf("Path = %q", e.Path)
	}

	e2 := entries[1]
	if e2.Asset.Kind != AssetHFDataset {
		t.Errorf("Kind = %q, want %q", e2.Asset.Kind, AssetHFDataset)
	}
	if e2.Asset.ID != "wikitext" {
		t.Errorf("ID = %q, want %q", e2.Asset.ID, "wikitext")
	}
}

func TestParseHFCacheAtimeOutput_StatFallback(t *testing.T) {
	// Shell loop using stat (BSD/macOS) — integer atime, size from du -sb
	output := "1710000000\t5123456789\t/Users/user/.cache/huggingface/hub/models--bert-base-cased\n"

	entries := parseHFCacheAtimeOutput(output, "studio")
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Asset.ID != "bert-base-cased" {
		t.Errorf("ID = %q, want %q", e.Asset.ID, "bert-base-cased")
	}
	if e.SizeBytes != 5123456789 {
		t.Errorf("SizeBytes = %d, want 5123456789", e.SizeBytes)
	}
}

func TestParseHFCacheAtimeOutput_SkipsNonHF(t *testing.T) {
	output := "1710000000\t1000\t/home/user/.cache/huggingface/hub/.locks\n" +
		"1710000000\t1000\t/home/user/.cache/huggingface/hub/models--bert-base-cased\n"

	entries := parseHFCacheAtimeOutput(output, "cool30")
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1 (should skip .locks)", len(entries))
	}
}

func TestParseHFCacheAtimeOutput_Empty(t *testing.T) {
	entries := parseHFCacheAtimeOutput("", "cool100")
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}
