package dataloc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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

	total, err := ResolveInputSizes(inputs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 32158192699 {
		t.Errorf("got total %d, want 32158192699", total)
	}
}
