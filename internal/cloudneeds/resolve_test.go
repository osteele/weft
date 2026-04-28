package cloudneeds

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
)

func TestResolveSpecs_PrefersArtifactFilesKey(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	relPath := "output/model.pt"
	spec := fmt.Sprintf("%s:%d", relPath, producerID)
	wantKey := r2keys.JobAttemptArtifactFilesPrefix(producerID, 0) + artifacts.LocalRelativePath(relPath)

	prev := r2resolve.ObjectExistsFunc
	t.Cleanup(func() { r2resolve.ObjectExistsFunc = prev })
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ *r2.Client, key string) (bool, error) {
		return key == wantKey, nil
	}

	got, err := ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err != nil {
		t.Fatalf("ResolveSpecs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Path != relPath {
		t.Fatalf("path = %q, want %q", got[0].Path, relPath)
	}
	if got[0].R2Key != wantKey {
		t.Fatalf("r2 key = %q, want %q", got[0].R2Key, wantKey)
	}
}

func TestResolveSpecs_FallsBackToRunZeroOutputsPrefix(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	relPath := "output/metrics.json"
	spec := fmt.Sprintf("%s:%d", relPath, producerID)
	wantKey := r2keys.JobAttemptOutputsPrefix(producerID, 0) + relPath

	prev := r2resolve.ObjectExistsFunc
	t.Cleanup(func() { r2resolve.ObjectExistsFunc = prev })

	var checked []string
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ *r2.Client, key string) (bool, error) {
		checked = append(checked, key)
		return key == wantKey, nil
	}

	got, err := ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err != nil {
		t.Fatalf("ResolveSpecs: %v", err)
	}
	if len(got) != 1 || got[0].R2Key != wantKey {
		t.Fatalf("resolved = %+v, want r2_key=%q", got, wantKey)
	}
	if !slices.Contains(checked, wantKey) {
		t.Fatalf("expected fallback key %q to be checked; checked=%v", wantKey, checked)
	}
}

func TestResolveSpecs_ReturnsErrorWhenMissing(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	spec := fmt.Sprintf("output/missing.pt:%d", producerID)
	prev := r2resolve.ObjectExistsFunc
	t.Cleanup(func() { r2resolve.ObjectExistsFunc = prev })
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ *r2.Client, _ string) (bool, error) { return false, nil }

	_, err = ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err == nil {
		t.Fatal("expected missing artifact error")
	}
}
