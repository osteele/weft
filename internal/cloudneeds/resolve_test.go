package cloudneeds

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
)

func TestResolveSpecs_ResolvesConventionOutputKey(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	relPath := "output/model.pt"
	spec := fmt.Sprintf("%s:%d", relPath, producerID)
	wantKey := r2keys.JobAttemptOutputsPrefix(producerID, 0) + relPath

	prev := r2resolve.ObjectExistsFunc
	t.Cleanup(func() { r2resolve.ObjectExistsFunc = prev })
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, key string) (bool, error) {
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

func TestResolveSpecs_NormalizesConventionOutputPath(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	relPath := "./output/checkpoints/../model.pt"
	spec := fmt.Sprintf("%s:%d", relPath, producerID)
	wantKey := r2keys.JobAttemptOutputsPrefix(producerID, 0) + "output/model.pt"

	prev := r2resolve.ObjectExistsFunc
	t.Cleanup(func() { r2resolve.ObjectExistsFunc = prev })
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, key string) (bool, error) {
		return key == wantKey, nil
	}

	got, err := ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err != nil {
		t.Fatalf("ResolveSpecs: %v", err)
	}
	if len(got) != 1 || got[0].R2Key != wantKey {
		t.Fatalf("resolved = %+v, want normalized r2_key=%q", got, wantKey)
	}
}

func TestResolveSpecs_FallsBackToLegacyArtifactFilesKey(t *testing.T) {
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
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, key string) (bool, error) {
		return key == wantKey, nil
	}

	got, err := ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err != nil {
		t.Fatalf("ResolveSpecs: %v", err)
	}
	if len(got) != 1 || got[0].R2Key != wantKey {
		t.Fatalf("resolved = %+v, want legacy r2_key=%q", got, wantKey)
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
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, key string) (bool, error) {
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
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, _ string) (bool, error) { return false, nil }
	prevList := r2resolve.ListRunIDsFunc
	t.Cleanup(func() { r2resolve.ListRunIDsFunc = prevList })
	r2resolve.ListRunIDsFunc = func(_ context.Context, _ r2resolve.Lister, _ int64) ([]int64, error) { return nil, nil }

	_, err = ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err == nil {
		t.Fatal("expected missing artifact error")
	}
	if !errors.Is(err, ErrArtifactPublicationUnknown) {
		t.Fatalf("error = %v, want unknown publication", err)
	}
}

func TestClassifyMissingArtifactUsesPerArtifactReadiness(t *testing.T) {
	state := &db.AttemptPublicationState{
		RequiredArtifactsState: db.PublicationStatePending,
		Artifacts: []db.AttemptPublicationArtifact{{
			Name: "model", Path: "./output/model.pt", State: db.PublicationStatePending,
		}},
	}
	err := classifyMissingArtifact("output/model.pt:17", "output/model.pt", 17, state)
	if !errors.Is(err, ErrArtifactPublicationPending) {
		t.Fatalf("pending error = %v", err)
	}

	state.Artifacts[0].State = db.PublicationStateReady
	err = classifyMissingArtifact("output/model.pt:17", "output/model.pt", 17, state)
	if !errors.Is(err, ErrArtifactPublicationMissing) {
		t.Fatalf("ready-but-missing error = %v, want confirmed missing", err)
	}

	state.Artifacts[0].State = db.PublicationStateFailed
	err = classifyMissingArtifact("output/model.pt:17", "output/model.pt", 17, state)
	if !errors.Is(err, ErrArtifactPublicationFailed) {
		t.Fatalf("failed error = %v", err)
	}
}

func TestResolveSpecs_FallbackFindsArtifactUnderHistoricalRun(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	relPath := "output/model.pt"
	spec := fmt.Sprintf("%s:%d", relPath, producerID)
	const historicalRunID int64 = 22095
	wantKey := r2keys.JobAttemptArtifactFilesPrefix(producerID, historicalRunID) + artifacts.LocalRelativePath(relPath)

	prev := r2resolve.ObjectExistsFunc
	t.Cleanup(func() { r2resolve.ObjectExistsFunc = prev })
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, key string) (bool, error) {
		return key == wantKey, nil
	}
	prevList := r2resolve.ListRunIDsFunc
	t.Cleanup(func() { r2resolve.ListRunIDsFunc = prevList })
	r2resolve.ListRunIDsFunc = func(_ context.Context, _ r2resolve.Lister, jobID int64) ([]int64, error) {
		if jobID != producerID {
			return nil, nil
		}
		return []int64{historicalRunID}, nil
	}

	got, err := ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
	if err != nil {
		t.Fatalf("ResolveSpecs: %v", err)
	}
	if len(got) != 1 || got[0].R2Key != wantKey {
		t.Fatalf("resolved = %+v, want r2_key=%q (run_id-mismatch fallback)", got, wantKey)
	}
}

func TestResolveSpecs_PublishedDirectoryReadiness(t *testing.T) {
	for _, tc := range []struct {
		name          string
		artifactState string
		pathPrefix    string
		undeclared    bool
		drainState    string
		children      []string
		listErr       error
		wantErr       error
	}{
		{name: "ready directory while global publication pending", artifactState: db.PublicationStateReady, children: []string{"weights.bin", "nested/config.json"}},
		{name: "leading slash matches individually ready directory", pathPrefix: "/", artifactState: db.PublicationStateReady, children: []string{"weights.bin", "nested/config.json"}},
		{name: "pending directory with partial bytes", artifactState: db.PublicationStatePending, children: []string{"weights.bin"}, wantErr: ErrArtifactPublicationPending},
		{name: "failed directory with partial bytes", artifactState: db.PublicationStateFailed, children: []string{"weights.bin"}, wantErr: ErrArtifactPublicationFailed},
		{name: "ready directory missing", artifactState: db.PublicationStateReady, wantErr: ErrArtifactPublicationMissing},
		{name: "directory listing unavailable", artifactState: db.PublicationStateReady, listErr: errors.New("network unavailable"), wantErr: ErrArtifactPublicationUnknown},
		{name: "undeclared directory with partial bytes awaits drain", undeclared: true, drainState: db.PublicationStatePending, children: []string{"weights.bin"}, wantErr: ErrArtifactPublicationPending},
		{name: "undeclared directory with no bytes awaits drain", undeclared: true, drainState: db.PublicationStatePending, wantErr: ErrArtifactPublicationPending},
		{name: "undeclared directory with unknown drain", undeclared: true, drainState: db.PublicationStateUnknown, children: []string{"weights.bin"}, wantErr: ErrArtifactPublicationUnknown},
		{name: "undeclared directory with failed drain", undeclared: true, drainState: db.PublicationStateFailed, children: []string{"weights.bin"}, wantErr: ErrArtifactPublicationFailed},
		{name: "undeclared directory after successful drain", undeclared: true, drainState: db.PublicationStateReady, children: []string{"weights.bin", "nested/config.json"}},
		{name: "undeclared directory absent after successful drain", undeclared: true, drainState: db.PublicationStateReady, wantErr: ErrArtifactPublicationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := db.SetupTestDB(t)
			jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "produce", "producer")
			if err != nil {
				t.Fatal(err)
			}
			producer, err := db.GetJobByID(database, jobID)
			if err != nil || producer.LatestRunID == nil {
				t.Fatalf("producer attempt: %+v, %v", producer, err)
			}
			runID := *producer.LatestRunID
			key := r2keys.JobAttemptOutputsPrefix(jobID, runID) + "output/model"
			spec := fmt.Sprintf("%soutput/model:%d", tc.pathPrefix, jobID)
			publication := &db.AttemptPublicationState{
				AttemptID: runID, JobID: jobID, Sequence: 1, ObservedAt: 1,
				ExecutionState:         db.PublicationExecutionComplete,
				RequiredArtifactsState: db.PublicationStatePending, DrainState: db.PublicationStatePending,
			}
			if tc.undeclared {
				publication.RequiredArtifactsState = db.PublicationStateReady
				publication.DrainState = tc.drainState
			} else {
				publication.Artifacts = []db.AttemptPublicationArtifact{{Name: "model", Path: "output/model", State: tc.artifactState, PayloadKey: key}}
			}
			if _, err := db.UpsertAttemptPublicationState(database, publication); err != nil {
				t.Fatal(err)
			}
			oldReport, oldExists, oldList := getPublicationReport, r2resolve.ObjectExistsFunc, r2resolve.ListObjectsFunc
			t.Cleanup(func() {
				getPublicationReport, r2resolve.ObjectExistsFunc, r2resolve.ListObjectsFunc = oldReport, oldExists, oldList
			})
			getPublicationReport = func(context.Context, *r2.Client, string) ([]byte, error) { return nil, nil }
			r2resolve.ObjectExistsFunc = func(context.Context, r2resolve.Store, string) (bool, error) { return false, nil }
			r2resolve.ListObjectsFunc = func(_ context.Context, _ r2resolve.Lister, prefix string) ([]r2.ObjectInfo, error) {
				if prefix != key+"/" {
					return nil, nil
				}
				var objects []r2.ObjectInfo
				for _, child := range tc.children {
					objects = append(objects, r2.ObjectInfo{Key: prefix + child})
				}
				return objects, tc.listErr
			}
			got, err := ResolveSpecs(context.Background(), database, &r2.Client{}, []string{spec})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || len(got) != 0 {
					t.Fatalf("resolved = %+v, %v; want %v", got, err, tc.wantErr)
				}
				if tc.listErr != nil && !errors.Is(err, tc.listErr) {
					t.Fatalf("lost lookup cause: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []cloud.CloudNeed{
				{Spec: spec, Path: "output/model/nested/config.json", R2Key: key + "/nested/config.json"},
				{Spec: spec, Path: "output/model/weights.bin", R2Key: key + "/weights.bin"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("directory needs = %+v, want %+v", got, want)
			}
		})
	}
}

func TestResolveSpecs_PublicationLookupErrorRemainsUnknown(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "produce", "producer")
	if err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("publication lookup timed out")
	oldReport := getPublicationReport
	t.Cleanup(func() { getPublicationReport = oldReport })
	getPublicationReport = func(context.Context, *r2.Client, string) ([]byte, error) { return nil, lookupErr }
	_, err = ResolveSpecs(context.Background(), database, &r2.Client{}, []string{fmt.Sprintf("output/model:%d", jobID)})
	if !errors.Is(err, ErrArtifactPublicationUnknown) || !errors.Is(err, lookupErr) ||
		errors.Is(err, ErrArtifactPublicationMissing) || errors.Is(err, ErrArtifactPublicationFailed) {
		t.Fatalf("failed publication lookup classified as %v", err)
	}
}
