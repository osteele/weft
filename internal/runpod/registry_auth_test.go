package runpod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestEnsureRegistryAuthFindsExisting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"ra-123","name":"weft-ghcr-io","registryUrl":"ghcr.io"}]`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()
	t.Setenv("RUNPOD_API_KEY", "test-key")

	id, err := ensureRegistryAuth(context.Background(), &cloud.RegistryAuth{
		Host:     "ghcr.io",
		Username: "osteele",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("ensureRegistryAuth: %v", err)
	}
	if id != "ra-123" {
		t.Fatalf("id = %q, want ra-123", id)
	}
}

func TestEnsureRegistryAuthCreatesNew(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ra-new","name":"weft-ghcr-io"}`))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()
	t.Setenv("RUNPOD_API_KEY", "test-key")

	id, err := ensureRegistryAuth(context.Background(), &cloud.RegistryAuth{
		Host:     "ghcr.io",
		Username: "osteele",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("ensureRegistryAuth: %v", err)
	}
	if id != "ra-new" {
		t.Fatalf("id = %q, want ra-new", id)
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2 (list + create)", calls)
	}
}

func TestListRegistryAuthsRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()

	_, err := listRegistryAuths(context.Background(), "test-key")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("listRegistryAuths error = %v, want HTTP 403", err)
	}
}

func TestListRegistryAuthsRejectsNullResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()

	_, err := listRegistryAuths(context.Background(), "test-key")
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("listRegistryAuths error = %v, want null-response error", err)
	}
}

func TestListRegistryAuthsRejectsUnrecognizedShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total": 0}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()

	_, err := listRegistryAuths(context.Background(), "test-key")
	if err == nil || !strings.Contains(err.Error(), "unrecognized") {
		t.Fatalf("listRegistryAuths error = %v, want unrecognized-shape error", err)
	}
}

func TestListRegistryAuthsSupportsWrappedShapes(t *testing.T) {
	for _, body := range []string{
		`[{"id":"ra-1"}]`,
		`{"containerRegistryAuths":[{"id":"ra-1"}]}`,
		`{"registryAuths":[{"id":"ra-1"}]}`,
		`{"items":[{"id":"ra-1"}]}`,
		`{"data":[{"id":"ra-1"}]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()

		oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
		t.Cleanup(func() {
			registryAuthEndpointURL = oldEndpoint
			registryAuthHTTPClient = oldClient
		})
		registryAuthEndpointURL = server.URL
		registryAuthHTTPClient = server.Client()

		records, err := listRegistryAuths(context.Background(), "test-key")
		if err != nil {
			t.Fatalf("shape %q: listRegistryAuths: %v", body, err)
		}
		if len(records) != 1 || records[0].ID != "ra-1" {
			t.Fatalf("shape %q: records = %+v, want one record with id ra-1", body, records)
		}
	}
}

func TestCreateRegistryAuthRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid registry url"}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()

	_, err := createRegistryAuth(context.Background(), "test-key", "weft-ghcr-io", &cloud.RegistryAuth{
		Host:     "ghcr.io",
		Username: "osteele",
		Password: "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("createRegistryAuth error = %v, want HTTP 400", err)
	}
}

func TestCreateRegistryAuthRejectsMissingID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"weft-ghcr-io"}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()

	_, err := createRegistryAuth(context.Background(), "test-key", "weft-ghcr-io", &cloud.RegistryAuth{
		Host:     "ghcr.io",
		Username: "osteele",
		Password: "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "no id") {
		t.Fatalf("createRegistryAuth error = %v, want no-id error", err)
	}
}

func TestCreateRegistryAuthAcceptsAlternateIDFields(t *testing.T) {
	for _, body := range []string{
		`{"id":"ra-a"}`,
		`{"registryAuthId":"ra-b"}`,
		`{"containerRegistryAuthId":"ra-c"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()

		oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
		t.Cleanup(func() {
			registryAuthEndpointURL = oldEndpoint
			registryAuthHTTPClient = oldClient
		})
		registryAuthEndpointURL = server.URL
		registryAuthHTTPClient = server.Client()

		id, err := createRegistryAuth(context.Background(), "test-key", "weft-ghcr-io", &cloud.RegistryAuth{
			Host:     "ghcr.io",
			Username: "osteele",
			Password: "secret",
		})
		if err != nil {
			t.Fatalf("body %q: createRegistryAuth: %v", body, err)
		}
		if id == "" {
			t.Fatalf("body %q: got empty id", body)
		}
	}
}

func TestEnsureRegistryAuthDoesNotLeakCredentialsInError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()
	t.Setenv("RUNPOD_API_KEY", "super-secret-key")

	_, err := ensureRegistryAuth(context.Background(), &cloud.RegistryAuth{
		Host:     "ghcr.io",
		Username: "osteele",
		Password: "super-secret-password",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-key") || strings.Contains(err.Error(), "super-secret-password") {
		t.Fatalf("error leaks credentials: %v", err)
	}
}

func TestCreateRegistryAuthSendsCredentialsInBody(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ra-1"}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := registryAuthEndpointURL, registryAuthHTTPClient
	t.Cleanup(func() {
		registryAuthEndpointURL = oldEndpoint
		registryAuthHTTPClient = oldClient
	})
	registryAuthEndpointURL = server.URL
	registryAuthHTTPClient = server.Client()

	_, err := createRegistryAuth(context.Background(), "test-key", "weft-ghcr-io", &cloud.RegistryAuth{
		Host:     "ghcr.io",
		Username: "osteele",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("createRegistryAuth: %v", err)
	}
	if !strings.Contains(body, `"username":"osteele"`) {
		t.Fatalf("body missing username: %s", body)
	}
	if !strings.Contains(body, `"password":"secret"`) {
		t.Fatalf("body missing password: %s", body)
	}
}
