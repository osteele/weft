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

func TestSortEvictionCandidates_ReusePerGB(t *testing.T) {
	now := time.Now()
	entries := []HostDataEntryWithUsage{
		{
			HostDataEntry: HostDataEntry{Host: "host-alpha", Asset: DataAsset{Kind: AssetHFModel, ID: "hot-large"}, SizeBytes: 10 * bytesPerGB},
			LastUsedAt:    now.Add(-time.Hour),
		},
		{
			HostDataEntry: HostDataEntry{Host: "host-alpha", Asset: DataAsset{Kind: AssetHFModel, ID: "warm-small"}, SizeBytes: bytesPerGB},
			LastUsedAt:    now.Add(-2 * time.Hour),
		},
		{
			HostDataEntry: HostDataEntry{Host: "host-alpha", Asset: DataAsset{Kind: AssetHFModel, ID: "cold"}, SizeBytes: bytesPerGB},
			LastUsedAt:    now.Add(-30 * time.Minute),
		},
	}
	counts := map[string]int{
		evictionEntryKey(entries[0]): 5, // 0.5 uses/GB
		evictionEntryKey(entries[1]): 1, // 1.0 uses/GB
		evictionEntryKey(entries[2]): 0,
	}

	SortEvictionCandidates(entries, "reuse_per_gb", counts)

	got := []string{entries[0].Asset.ID, entries[1].Asset.ID, entries[2].Asset.ID}
	want := []string{"cold", "hot-large", "warm-small"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSortEvictionCandidates_LRU(t *testing.T) {
	now := time.Now()
	entries := []HostDataEntryWithUsage{
		{HostDataEntry: HostDataEntry{Host: "b", Asset: DataAsset{Kind: AssetHFModel, ID: "new"}}, LastUsedAt: now},
		{HostDataEntry: HostDataEntry{Host: "a", Asset: DataAsset{Kind: AssetHFModel, ID: "old"}}, LastUsedAt: now.Add(-time.Hour)},
	}

	SortEvictionCandidates(entries, "lru", nil)

	if entries[0].Asset.ID != "old" {
		t.Fatalf("first asset = %q, want old", entries[0].Asset.ID)
	}
}
