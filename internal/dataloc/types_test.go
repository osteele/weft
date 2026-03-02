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

func TestParseInputRef(t *testing.T) {
	tests := []struct {
		input     string
		wantAsset bool
		wantPath  string
	}{
		// Asset refs
		{"hf:meta-llama/Llama-3-8B", true, ""},
		{"hf-dataset:wikitext", true, ""},
		{"checkpoint:llama-ft-v1", true, ""},
		// File paths
		{"~/sources/vidur/data/", false, "~/sources/vidur/data/"},
		{"/absolute/path/to/data", false, "/absolute/path/to/data"},
		{"./relative/path", false, "./relative/path"},
		{"relative/path", false, "relative/path"},
		// Edge cases: unknown prefix treated as file path
		{"unknown:something", false, "unknown:something"},
		{"", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref := ParseInputRef(tt.input)
			if ref.IsAsset() != tt.wantAsset {
				t.Errorf("ParseInputRef(%q).IsAsset() = %v, want %v", tt.input, ref.IsAsset(), tt.wantAsset)
			}
			if !tt.wantAsset && ref.FilePath != tt.wantPath {
				t.Errorf("ParseInputRef(%q).FilePath = %q, want %q", tt.input, ref.FilePath, tt.wantPath)
			}
		})
	}
}

func TestClassifyInputs(t *testing.T) {
	inputs := []string{
		"hf:meta-llama/Llama-3-8B",
		"~/sources/vidur/data/",
		"hf-dataset:wikitext",
		"/tmp/local-data",
	}
	assetRefs, filePaths := ClassifyInputs(inputs)

	if len(assetRefs) != 2 {
		t.Errorf("got %d asset refs, want 2: %v", len(assetRefs), assetRefs)
	}
	if len(filePaths) != 2 {
		t.Errorf("got %d file paths, want 2: %v", len(filePaths), filePaths)
	}
	if len(assetRefs) > 0 && assetRefs[0] != "hf:meta-llama/Llama-3-8B" {
		t.Errorf("first asset ref = %q, want %q", assetRefs[0], "hf:meta-llama/Llama-3-8B")
	}
	if len(filePaths) > 0 && filePaths[0] != "~/sources/vidur/data/" {
		t.Errorf("first file path = %q, want %q", filePaths[0], "~/sources/vidur/data/")
	}
}

func TestClassifyInputs_Empty(t *testing.T) {
	assetRefs, filePaths := ClassifyInputs(nil)
	if assetRefs != nil {
		t.Errorf("expected nil asset refs for nil input, got %v", assetRefs)
	}
	if filePaths != nil {
		t.Errorf("expected nil file paths for nil input, got %v", filePaths)
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
