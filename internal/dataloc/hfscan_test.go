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
			got, ok := parseHFDirName(tt.name)
			if ok != tt.wantOK {
				t.Errorf("parseHFDirName(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
				return
			}
			if ok && (got.Kind != tt.want.Kind || got.ID != tt.want.ID) {
				t.Errorf("parseHFDirName(%q) = %v, want %v", tt.name, got, tt.want)
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
