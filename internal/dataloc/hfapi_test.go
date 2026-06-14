package dataloc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// setupHFTestServer configures the package-level HF client and URL to use the
// given test server, and registers cleanup to restore the originals.
func setupHFTestServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	// Keep tests deterministic by isolating the persistent cache path.
	t.Setenv("HOME", t.TempDir())
	ClearHFModelSizeCache()

	origClient := hfHTTPClient
	hfHTTPClient = server.Client()
	t.Cleanup(func() { hfHTTPClient = origClient })

	origURL := fetchHFModelSizeURL
	fetchHFModelSizeURL = server.URL + "/api/models/%s/tree/main"
	t.Cleanup(func() { fetchHFModelSizeURL = origURL })

	origDatasetURL := fetchHFDatasetURL
	fetchHFDatasetURL = server.URL + "/api/datasets/%s"
	t.Cleanup(func() { fetchHFDatasetURL = origDatasetURL })
}

func TestFetchHFModelSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/meta-llama/Llama-3.1-8B/tree/main" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[
				{"path":"config.json","size":1234},
				{"path":"README.md","size":567},
				{"path":"model.safetensors","size":111,"lfs":{"size":32158192699}}
			]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	size, err := FetchHFModelSize("meta-llama/Llama-3.1-8B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 32158194500 {
		t.Errorf("got size %d, want 32158194500", size)
	}

	// Second call should hit cache
	size2, err := FetchHFModelSize("meta-llama/Llama-3.1-8B")
	if err != nil {
		t.Fatalf("unexpected error on cached call: %v", err)
	}
	if size2 != size {
		t.Errorf("cached size %d != original %d", size2, size)
	}
}

func TestFetchHFModelSize_SkipsRedundantNativeCheckpoints(t *testing.T) {
	// FR1: original/consolidated.*.pth is a native checkpoint vLLM/transformers
	// never load; it must not inflate the size estimate.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/meta-llama/Llama-3.1-8B/tree/main" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[
				{"path":"config.json","size":1000},
				{"path":"model.safetensors","size":1,"lfs":{"size":16000000000}},
				{"path":"original/consolidated.00.pth","size":1,"lfs":{"size":16000000000}},
				{"path":"original/params.json","size":200}
			]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	size, err := FetchHFModelSize("meta-llama/Llama-3.1-8B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(16000001000); size != want {
		t.Errorf("got size %d, want %d (original/ excluded)", size, want)
	}
}

func TestIsRedundantHFModelPath(t *testing.T) {
	redundant := []string{"original/consolidated.00.pth", "original/params.json", "./original/x"}
	kept := []string{"model.safetensors", "config.json", "tokenizer.json", "originals/x.bin"}
	for _, p := range redundant {
		if !isRedundantHFModelPath(p) {
			t.Errorf("isRedundantHFModelPath(%q) = false, want true", p)
		}
	}
	for _, p := range kept {
		if isRedundantHFModelPath(p) {
			t.Errorf("isRedundantHFModelPath(%q) = true, want false", p)
		}
	}
}

func TestFetchHFModelSize_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	_, err := FetchHFModelSize("nonexistent/model")
	if err == nil {
		t.Fatal("expected error for nonexistent model")
	}
}

func TestFetchHFModelSize_AuthRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	_, err := FetchHFModelSize("meta-llama/Llama-3.1-8B")
	if err == nil {
		t.Fatal("expected auth-required error")
	}
	if !errors.Is(err, ErrHFAuthRequired) {
		t.Fatalf("error = %v, want ErrHFAuthRequired", err)
	}
}

func TestFetchHFModelSize_SendsHFToken(t *testing.T) {
	tokenSeen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/meta-llama/Llama-3.1-8B/tree/main" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer test-token" {
			tokenSeen = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"path":"model.safetensors","size":2048}]`))
	}))
	defer server.Close()
	setupHFTestServer(t, server)
	t.Setenv("HF_TOKEN", "test-token")

	size, err := FetchHFModelSize("meta-llama/Llama-3.1-8B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 2048 {
		t.Fatalf("size = %d, want 2048", size)
	}
	if !tokenSeen {
		t.Fatal("expected Authorization header with HF token")
	}
}

func TestFetchHFModelSize_EmptyTree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/meta-llama/Llama-3.1-8B/tree/main" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	_, err := FetchHFModelSize("meta-llama/Llama-3.1-8B")
	if err == nil {
		t.Fatal("expected error for empty tree response")
	}
	if !errors.Is(err, ErrHFNoFiles) {
		t.Fatalf("error = %v, want ErrHFNoFiles", err)
	}
}

func TestResolveInputSizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/meta-llama/Llama-3.1-8B/tree/main" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"path":"model.safetensors","size":32158192699}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	inputs := []string{
		"hf:meta-llama/Llama-3.1-8B",
		"hf:meta-llama/Llama-3.1-8B", // duplicate, should only count once
		"~/data/some-file",           // non-HF, ignored
		"checkpoint:my-model",        // non-HF asset, ignored
	}

	total, unresolved, err := ResolveInputSizes(inputs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unexpected unresolved refs: %v", unresolved)
	}
	if total != 32158192699 {
		t.Errorf("got total %d, want 32158192699", total)
	}
}

func TestResolveInputSizes_UsesFullHFSizeOverPartialHostCache(t *testing.T) {
	db := setupTestDB(t)
	if err := RecordAsset(db, HostDataEntry{
		Host:      "host-alpha",
		Asset:     DataAsset{Kind: AssetHFModel, ID: "openai-community/gpt2"},
		SizeBytes: 550_000_000,
		LastSeen:  time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/openai-community/gpt2/tree/main" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"path":"model.safetensors","size":3666000000}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	total, unresolved, err := ResolveInputSizes([]string{"hf:openai-community/gpt2"}, db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unexpected unresolved refs: %v", unresolved)
	}
	if total != 3_666_000_000 {
		t.Errorf("got total %d, want full HF repo size", total)
	}
}

// TestResolveInputSizes_PartialFailureReturnsResolvedPlusUnresolved verifies
// that a per-ref failure (e.g. 404) does not abort the whole batch: the
// returned total is the sum of the successful refs, and the failing ref
// appears in unresolved. This is the contract the disk estimator relies on
// to keep sizing correct in the presence of one bad input ref.
func TestResolveInputSizes_PartialFailureReturnsResolvedPlusUnresolved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/good/model/tree/main" {
			w.Write([]byte(`[{"path":"m.bin","size":5000}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	total, unresolved, err := ResolveInputSizes([]string{"hf:good/model", "hf:missing/model"}, nil)
	if err == nil {
		t.Fatalf("expected non-nil error listing the unresolved ref")
	}
	if total != 5000 {
		t.Errorf("got total %d, want 5000 (the resolved ref's size)", total)
	}
	if len(unresolved) != 1 || unresolved[0] != "hf:missing/model" {
		t.Errorf("unresolved = %v, want [hf:missing/model]", unresolved)
	}
}

// TestResolveModelSize_DatasetRefSurfacesClearError verifies that a bare
// "hf:<id>" ref that exists on HF as a dataset (not a model) surfaces
// ErrHFRefIsDataset rather than a generic 404. This is the signal users
// need to fix their PEP 723 metadata from hf: to hf-dataset:.
func TestResolveModelSize_DatasetRefSurfacesClearError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/datasets/wikitext" {
			w.Write([]byte(`{"id":"wikitext"}`))
			return
		}
		http.NotFound(w, r) // models endpoint 404s for "wikitext"
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	_, err := resolveModelSize(DataAsset{Kind: AssetHFModel, ID: "wikitext"}, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrHFRefIsDataset) {
		t.Fatalf("err = %v, want ErrHFRefIsDataset", err)
	}
}
