package dataloc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// setupHFTestServer configures the package-level HF client and URL to use the
// given test server, and registers cleanup to restore the originals.
func setupHFTestServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	ClearHFModelSizeCache()

	origClient := hfHTTPClient
	hfHTTPClient = server.Client()
	t.Cleanup(func() { hfHTTPClient = origClient })

	origURL := fetchHFModelSizeURL
	fetchHFModelSizeURL = server.URL + "/api/models/%s"
	t.Cleanup(func() { fetchHFModelSizeURL = origURL })
}

func TestFetchHFModelSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/meta-llama/Llama-3.1-8B" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"_id":"66a39ef1b65d5ec98a553449","id":"meta-llama/Llama-3.1-8B","modelId":"meta-llama/Llama-3.1-8B","usedStorage":32158192699}`))
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
	if size != 32158192699 {
		t.Errorf("got size %d, want 32158192699", size)
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

func TestFetchHFModelSize_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()
	setupHFTestServer(t, server)

	_, err := FetchHFModelSize("nonexistent/model")
	if err == nil {
		t.Fatal("expected error for nonexistent model")
	}
}

func TestResolveInputSizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/meta-llama/Llama-3.1-8B" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"usedStorage":32158192699}`))
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

	total, err := ResolveInputSizes(inputs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 32158192699 {
		t.Errorf("got total %d, want 32158192699", total)
	}
}
