package dataloc

import "testing"

func TestParseAssetRef(t *testing.T) {
	tests := []struct {
		ref    string
		want   DataAsset
		wantOK bool
	}{
		{"hf:meta-llama/Llama-3-8B", DataAsset{AssetHFModel, "meta-llama/Llama-3-8B"}, true},
		{"hf:bert-base-uncased", DataAsset{AssetHFModel, "bert-base-uncased"}, true},
		{"hf-dataset:wikitext", DataAsset{AssetHFDataset, "wikitext"}, true},
		{"hf-dataset:allenai/c4", DataAsset{AssetHFDataset, "allenai/c4"}, true},
		{"checkpoint:llama-ft-v1", DataAsset{AssetCheckpoint, "llama-ft-v1"}, true},
		{"checkpoint:run-42/best", DataAsset{AssetCheckpoint, "run-42/best"}, true},
		// Invalid cases
		{"", DataAsset{}, false},
		{"hf:", DataAsset{}, false},
		{"unknown:something", DataAsset{}, false},
		{"nocolon", DataAsset{}, false},
		{":nopref", DataAsset{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, ok := ParseAssetRef(tt.ref)
			if ok != tt.wantOK {
				t.Errorf("ParseAssetRef(%q) ok = %v, want %v", tt.ref, ok, tt.wantOK)
				return
			}
			if ok && (got.Kind != tt.want.Kind || got.ID != tt.want.ID) {
				t.Errorf("ParseAssetRef(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestDataAsset_String(t *testing.T) {
	tests := []struct {
		asset DataAsset
		want  string
	}{
		{DataAsset{AssetHFModel, "meta-llama/Llama-3-8B"}, "hf:meta-llama/Llama-3-8B"},
		{DataAsset{AssetHFDataset, "wikitext"}, "hf-dataset:wikitext"},
		{DataAsset{AssetCheckpoint, "llama-ft-v1"}, "checkpoint:llama-ft-v1"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.asset.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseAssetRef_Roundtrip(t *testing.T) {
	refs := []string{
		"hf:meta-llama/Llama-3-8B",
		"hf-dataset:wikitext",
		"checkpoint:llama-ft-v1",
	}
	for _, ref := range refs {
		asset, ok := ParseAssetRef(ref)
		if !ok {
			t.Fatalf("ParseAssetRef(%q) failed", ref)
		}
		got := asset.String()
		if got != ref {
			t.Errorf("roundtrip: got %q, want %q", got, ref)
		}
	}
}
