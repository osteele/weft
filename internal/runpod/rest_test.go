package runpod

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestFetchPodDetailRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid API key"}`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	_, err := fetchPodDetail(context.Background(), "pod-123")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("fetchPodDetail error = %v, want HTTP 401", err)
	}
}

func TestFetchPodDetailRejectsNullResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	_, err := fetchPodDetail(context.Background(), "pod-123")
	if err == nil {
		t.Fatal("fetchPodDetail accepted null response")
	}
}

func TestFetchPodDetailRejectsMissingIdentity(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"RUNNING"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()

		oldEndpoint := restEndpointURL
		t.Cleanup(func() { restEndpointURL = oldEndpoint })
		restEndpointURL = server.URL

		_, err := fetchPodDetail(context.Background(), "pod-123")
		if err == nil {
			t.Fatalf("fetchPodDetail accepted missing-identity response %q", body)
		}
	}
}

func TestFetchPodDetailReturnsValidPod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"pod-123","desiredStatus":"RUNNING","machineId":"machine-9","dataCenterId":"EU-SE-1"}`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	pod, err := fetchPodDetail(context.Background(), "pod-123")
	if err != nil {
		t.Fatalf("fetchPodDetail: %v", err)
	}
	if pod.ID != "pod-123" {
		t.Fatalf("pod.ID = %q, want pod-123", pod.ID)
	}
	if pod.Status != "RUNNING" {
		t.Fatalf("pod.Status = %q, want RUNNING", pod.Status)
	}
	if pod.MachineID != "machine-9" {
		t.Fatalf("pod.MachineID = %q, want machine-9", pod.MachineID)
	}
	if pod.DataCenter != "EU-SE-1" {
		t.Fatalf("pod.DataCenter = %q, want EU-SE-1", pod.DataCenter)
	}
}

func TestFetchPodDetailUnwrapsPodWrapper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pod":{"id":"pod-456","desiredStatus":"RUNNING"}}`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	pod, err := fetchPodDetail(context.Background(), "pod-456")
	if err != nil {
		t.Fatalf("fetchPodDetail: %v", err)
	}
	if pod.ID != "pod-456" {
		t.Fatalf("pod.ID = %q, want pod-456", pod.ID)
	}
}

func TestFetchPodDetailNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"pod not found"}`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	_, err := fetchPodDetail(context.Background(), "pod-missing")
	if !errors.Is(err, cloud.ErrInstanceNotFound) {
		t.Fatalf("fetchPodDetail error = %v, want ErrInstanceNotFound", err)
	}
}

func TestUnwrapPodObject(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"top-level", map[string]any{"id": "a"}, "a"},
		{"pod wrapper", map[string]any{"pod": map[string]any{"id": "b"}}, "b"},
		{"data wrapper", map[string]any{"data": map[string]any{"id": "c"}}, "c"},
		{"prefers pod over data", map[string]any{"pod": map[string]any{"id": "p"}, "data": map[string]any{"id": "d"}}, "p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapPodObject(tc.in)
			if got["id"] != tc.want {
				t.Fatalf("unwrapPodObject id = %v, want %v", got["id"], tc.want)
			}
		})
	}
}

func TestFetchPodDetailRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{invalid`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	_, err := fetchPodDetail(context.Background(), "pod-123")
	if err == nil {
		t.Fatal("fetchPodDetail accepted malformed JSON")
	}
}

func TestFetchPodDetailIncludesInstanceIDInURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"pod-123"}`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	_, err := fetchPodDetail(context.Background(), "pod-123")
	if err != nil {
		t.Fatalf("fetchPodDetail: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/pods/pod-123") {
		t.Fatalf("URL path = %q, want /pods/pod-123 suffix", gotPath)
	}
}

// TestFetchPodDetailDoesNotLeakKey verifies that URL path/query and error text
// do not include the API key.
func TestFetchPodDetailDoesNotLeakKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer server.Close()

	oldEndpoint := restEndpointURL
	t.Cleanup(func() { restEndpointURL = oldEndpoint })
	restEndpointURL = server.URL

	t.Setenv("RUNPOD_API_KEY", "super-secret-key")
	_, err := fetchPodDetail(context.Background(), "pod-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("error leaks API key: %v", err)
	}
}
