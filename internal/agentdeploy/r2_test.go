package agentdeploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeAgentObjectStore struct {
	exists   bool
	putCalls int
}

func (s *fakeAgentObjectStore) ObjectExists(context.Context, string) (bool, error) {
	return s.exists, nil
}

func (s *fakeAgentObjectStore) PutObject(context.Context, string, io.Reader, string) error {
	s.putCalls++
	return nil
}

func TestEnsureAgentInR2BuildFailureDoesNotUseStaleArtifact(t *testing.T) {
	store := &fakeAgentObjectStore{}
	var phases []string
	key, err := ensureAgentInR2WithProgress(
		context.Background(), store, "requested-version", "linux", "amd64", io.Discard,
		func(phase string) { phases = append(phases, phase) },
		func(string, string, string, io.Writer, EnsureAgentProgressFunc) (string, error) {
			return "", errors.New("builders unavailable")
		},
	)
	if err == nil {
		t.Fatal("ensureAgentInR2WithProgress succeeded, want exact-version build failure")
	}
	if key != "" {
		t.Fatalf("key = %q, want no fallback artifact", key)
	}
	if !strings.Contains(err.Error(), "requested-version") || !strings.Contains(err.Error(), "builders unavailable") {
		t.Fatalf("error = %q, want requested version and builder diagnostic", err)
	}
	if store.putCalls != 0 {
		t.Fatalf("PutObject calls = %d, want 0", store.putCalls)
	}
	for _, phase := range phases {
		if phase == "ready" || phase == "using stale cached agent" {
			t.Fatalf("phases = %v, must not report a failed exact artifact as ready", phases)
		}
	}
}
