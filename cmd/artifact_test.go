package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

func TestResolveArtifactOutputPath(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "artifact.txt")
	if err := os.WriteFile(source, []byte("data"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	dest, err := resolveArtifactOutputPath(source, "")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if dest != "artifact.txt" {
		t.Fatalf("expected default basename, got %q", dest)
	}

	dest, err = resolveArtifactOutputPath(source, "-")
	if err != nil {
		t.Fatalf("resolve stdout: %v", err)
	}
	if dest != "-" {
		t.Fatalf("expected '-', got %q", dest)
	}

	dest, err = resolveArtifactOutputPath(source, tmp)
	if err != nil {
		t.Fatalf("resolve dir: %v", err)
	}
	if dest != filepath.Join(tmp, "artifact.txt") {
		t.Fatalf("expected dir join, got %q", dest)
	}

	dest, err = resolveArtifactOutputPath(source, filepath.Join(tmp, "out.txt"))
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	if dest != filepath.Join(tmp, "out.txt") {
		t.Fatalf("expected explicit file, got %q", dest)
	}
}

func TestCopyToWriter(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "artifact.txt")
	want := []byte("artifact contents\n")
	if err := os.WriteFile(source, want, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var buf bytes.Buffer
	if err := copyToWriter(source, &buf); err != nil {
		t.Fatalf("copy to writer: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("unexpected content: %q", buf.Bytes())
	}
}

type fakeCloudArtifactStore struct {
	objects map[string][]byte
}

func (s *fakeCloudArtifactStore) GetObject(_ context.Context, key string) ([]byte, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeCloudArtifactStore) ListObjects(_ context.Context, prefix string) ([]r2.ObjectInfo, error) {
	var objects []r2.ObjectInfo
	for key, data := range s.objects {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			objects = append(objects, r2.ObjectInfo{Key: key, SizeBytes: int64(len(data))})
		}
	}
	return objects, nil
}

func TestSyncCloudJobArtifactsWithStore_DirectoryArtifact(t *testing.T) {
	database := db.SetupTestDB(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	jobID, err := db.RecordQueued(database, "", "/tmp/project", "echo hi", "cloud")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "H200"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}
	manifestKey := r2keys.JobAttemptArtifactManifest(job.ID, runID)
	filesPrefix := r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID)

	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			manifestKey: []byte(`{"artifacts":[{"path":"cache/representations"}]}`),
			filesPrefix + "cache/representations/a.txt":        []byte("alpha"),
			filesPrefix + "cache/representations/nested/b.txt": []byte("beta"),
		},
	}

	result, err := syncCloudJobArtifactsWithStore(database, store, job)
	if err != nil {
		t.Fatalf("syncCloudJobArtifactsWithStore: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("added = %d, want 1", result.Added)
	}

	root, err := artifacts.LocalArtifactsDir()
	if err != nil {
		t.Fatalf("LocalArtifactsDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, strconv.FormatInt(job.ID, 10), "cache", "representations", "a.txt")); err != nil {
		t.Fatalf("missing first artifact file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, strconv.FormatInt(job.ID, 10), "cache", "representations", "nested", "b.txt")); err != nil {
		t.Fatalf("missing nested artifact file: %v", err)
	}

	entries, err := db.ListArtifactsByJob(database, job.ID)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("artifact entries = %d, want 1", len(entries))
	}
	if entries[0].SHA256 != "" {
		t.Fatalf("directory artifact sha = %q, want empty", entries[0].SHA256)
	}
}

func TestSyncArtifactsForJob_UsesCloudSyncForLaunchJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", "/tmp/project", "echo hi", "cloud")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "H200"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevLocal := syncLocalJobArtifacts
	prevCloud := syncCloudJobArtifactsFunc
	t.Cleanup(func() {
		syncLocalJobArtifacts = prevLocal
		syncCloudJobArtifactsFunc = prevCloud
	})

	localCalled := false
	cloudCalled := false
	syncLocalJobArtifacts = func(*sql.DB, *db.Job, time.Duration) (artifacts.SyncResult, error) {
		localCalled = true
		return artifacts.SyncResult{}, nil
	}
	syncCloudJobArtifactsFunc = func(*sql.DB, *r2.Client, *db.Job) (artifacts.SyncResult, error) {
		cloudCalled = true
		return artifacts.SyncResult{}, nil
	}

	if err := syncArtifactsForJob(database, job, nil, time.Second); err != nil {
		t.Fatalf("syncArtifactsForJob: %v", err)
	}
	if !cloudCalled {
		t.Fatal("expected cloud sync path")
	}
	if localCalled {
		t.Fatal("did not expect local sync path")
	}
}
