package dataloc

import "testing"

func TestParseHFDirName(t *testing.T) {
	tests := []struct {
		name   string
		want   DataAsset
		wantOK bool
	}{
		{"models--meta-llama--Llama-3-8B", DataAsset{AssetHFModel, "meta-llama/Llama-3-8B"}, true},
		{"models--bert-base-uncased", DataAsset{AssetHFModel, "bert-base-uncased"}, true},
		{"models--google--gemma-2b", DataAsset{AssetHFModel, "google/gemma-2b"}, true},
		{"datasets--wikitext", DataAsset{AssetHFDataset, "wikitext"}, true},
		{"datasets--allenai--c4", DataAsset{AssetHFDataset, "allenai/c4"}, true},
		{"datasets--HuggingFaceFW--fineweb", DataAsset{AssetHFDataset, "HuggingFaceFW/fineweb"}, true},
		// Invalid
		{"snapshots", DataAsset{}, false},
		{"", DataAsset{}, false},
		{"refs", DataAsset{}, false},
		{".locks", DataAsset{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseHFDirName(tt.name)
			if ok != tt.wantOK {
				t.Errorf("ParseHFDirName(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
				return
			}
			if ok && (got.Kind != tt.want.Kind || got.ID != tt.want.ID) {
				t.Errorf("ParseHFDirName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestParseHFCacheOutput(t *testing.T) {
	output := `/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B
/home/user/.cache/huggingface/hub/models--google--gemma-2b
/home/user/.cache/huggingface/hub/datasets--wikitext
`
	assets := parseHFCacheOutput(output)
	if len(assets) != 3 {
		t.Fatalf("got %d assets, want 3", len(assets))
	}

	// Check first
	if assets[0].Kind != AssetHFModel || assets[0].ID != "meta-llama/Llama-3-8B" {
		t.Errorf("asset[0] = %v", assets[0])
	}
	// Check second
	if assets[1].Kind != AssetHFModel || assets[1].ID != "google/gemma-2b" {
		t.Errorf("asset[1] = %v", assets[1])
	}
	// Check dataset
	if assets[2].Kind != AssetHFDataset || assets[2].ID != "wikitext" {
		t.Errorf("asset[2] = %v", assets[2])
	}
}

func TestParseHFCacheOutput_Empty(t *testing.T) {
	assets := parseHFCacheOutput("")
	if len(assets) != 0 {
		t.Errorf("expected empty, got %d", len(assets))
	}
}

func TestParseHFCacheOutput_WithBlankLines(t *testing.T) {
	output := `
/home/user/.cache/huggingface/hub/models--bert-base-uncased

`
	assets := parseHFCacheOutput(output)
	if len(assets) != 1 {
		t.Fatalf("got %d assets, want 1", len(assets))
	}
	if assets[0].ID != "bert-base-uncased" {
		t.Errorf("got ID %q, want bert-base-uncased", assets[0].ID)
	}
}

func TestHfDirToID(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{"meta-llama--Llama-3-8B", "meta-llama/Llama-3-8B"},
		{"bert-base-uncased", "bert-base-uncased"},
		{"google--gemma-2b", "google/gemma-2b"},
		// Single name (no org)
		{"wikitext", "wikitext"},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			got := hfDirToID(tt.dir)
			if got != tt.want {
				t.Errorf("hfDirToID(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestParseHFCacheDetailedOutput_DuFormat(t *testing.T) {
	// du -sb format: "<bytes>\t<path>"
	output := "16284561408\t/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B\n" +
		"5123456789\t/home/user/.cache/huggingface/hub/models--google--gemma-2b\n" +
		"1048576000\t/home/user/.cache/huggingface/hub/datasets--wikitext\n"

	entries := parseHFCacheDetailedOutput(output, "host-beta")
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	// Check model entry
	e := entries[0]
	if e.Host != "host-beta" {
		t.Errorf("host: got %q, want host-beta", e.Host)
	}
	if e.Asset.Kind != AssetHFModel || e.Asset.ID != "meta-llama/Llama-3-8B" {
		t.Errorf("asset: got %v", e.Asset)
	}
	if e.Path != "/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B" {
		t.Errorf("path: got %q", e.Path)
	}
	if e.SizeBytes != 16284561408 {
		t.Errorf("size: got %d, want 16284561408", e.SizeBytes)
	}

	// Check dataset entry
	ds := entries[2]
	if ds.Asset.Kind != AssetHFDataset || ds.Asset.ID != "wikitext" {
		t.Errorf("dataset: got %v", ds.Asset)
	}
	if ds.SizeBytes != 1048576000 {
		t.Errorf("dataset size: got %d, want 1048576000", ds.SizeBytes)
	}
}

func TestParseHFCacheDetailedOutput_LsFallback(t *testing.T) {
	// ls -1d format: plain paths without size
	output := "/home/user/.cache/huggingface/hub/models--bert-base-uncased\n" +
		"/home/user/.cache/huggingface/hub/datasets--allenai--c4\n"

	entries := parseHFCacheDetailedOutput(output, "host-alpha")
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if entries[0].Asset.ID != "bert-base-uncased" {
		t.Errorf("asset[0] ID: got %q, want bert-base-uncased", entries[0].Asset.ID)
	}
	if entries[0].Path != "/home/user/.cache/huggingface/hub/models--bert-base-uncased" {
		t.Errorf("path: got %q", entries[0].Path)
	}
	if entries[0].SizeBytes != 0 {
		t.Errorf("size should be 0 for ls fallback, got %d", entries[0].SizeBytes)
	}

	if entries[1].Asset.Kind != AssetHFDataset {
		t.Errorf("asset[1] should be dataset, got %s", entries[1].Asset.Kind)
	}
}

func TestParseHFCacheDetailedOutput_Empty(t *testing.T) {
	entries := parseHFCacheDetailedOutput("", "host-beta")
	if len(entries) != 0 {
		t.Errorf("expected empty, got %d", len(entries))
	}
}

func TestParseHFCacheDetailedOutput_InvalidDirNames(t *testing.T) {
	// du output with non-HF directories mixed in
	output := "12345\t/home/user/.cache/huggingface/hub/snapshots\n" +
		"67890\t/home/user/.cache/huggingface/hub/.locks\n"

	entries := parseHFCacheDetailedOutput(output, "host-beta")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for invalid dir names, got %d", len(entries))
	}
}

func TestParseHFCacheDetailedOutput_MixedFormat(t *testing.T) {
	// Unlikely but handle gracefully: mix of du and ls output
	output := "16284561408\t/home/user/.cache/huggingface/hub/models--meta-llama--Llama-3-8B\n" +
		"/home/user/.cache/huggingface/hub/datasets--wikitext\n"

	entries := parseHFCacheDetailedOutput(output, "host-beta")
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	// First has size from du
	if entries[0].SizeBytes != 16284561408 {
		t.Errorf("first entry size: got %d", entries[0].SizeBytes)
	}
	// Second has no size (ls fallback)
	if entries[1].SizeBytes != 0 {
		t.Errorf("second entry size should be 0, got %d", entries[1].SizeBytes)
	}
}
