package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
	"github.com/spf13/cobra"
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

func TestResolveArtifactOutputPathForAllPreservesRelativePath(t *testing.T) {
	dest, err := resolveArtifactOutputPathForAll("output/nested/result.json", "", 1941, false)
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if dest != filepath.Join("output", "nested", "result.json") {
		t.Fatalf("dest = %q, want preserved relative path", dest)
	}
}

func TestResolveArtifactOutputPathForAllPrefixesMultipleJobs(t *testing.T) {
	dest, err := resolveArtifactOutputPathForAll("output/nested/result.json", "", 1941, true)
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	want := filepath.Join("wj1941", "output", "nested", "result.json")
	if dest != want {
		t.Fatalf("dest = %q, want %q", dest, want)
	}
}

func TestResolveArtifactOutputPathForAllRequiresDirectory(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "out.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := resolveArtifactOutputPathForAll("output/result.json", file, 1941, false); err == nil {
		t.Fatal("expected output file rejection")
	}
	if _, err := resolveArtifactOutputPathForAll("output/result.json", filepath.Join(tmp, "missing"), 1941, false); err == nil {
		t.Fatal("expected missing output directory rejection")
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
		return nil, &types.NoSuchKey{}
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeCloudArtifactStore) GetObjectReader(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return io.NopCloser(bytes.NewReader(data)), nil
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

func (s *fakeCloudArtifactStore) DownloadObjectToWriterWithIdleTimeout(ctx context.Context, key string, dst io.Writer, _ time.Duration) (int64, error) {
	body, err := s.GetObjectReader(ctx, key)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	return io.Copy(dst, body)
}

func (s *fakeCloudArtifactStore) DownloadObjectToFileWithIdleTimeout(ctx context.Context, key string, localPath string, timeout time.Duration) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return 0, err
	}
	n, copyErr := s.DownloadObjectToWriterWithIdleTimeout(ctx, key, f, timeout)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(localPath)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(localPath)
		return n, closeErr
	}
	return n, nil
}

func setupLaunchArtifactJob(t *testing.T) *db.Job {
	t.Helper()
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
	return job
}

func TestDownloadSingleCloudFileToPath_ArtifactsPathFromConventionOutputs(t *testing.T) {
	job := setupLaunchArtifactJob(t)
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "artifacts/summary.md": []byte("summary\n"),
		},
	}

	dest := filepath.Join(t.TempDir(), "summary.md")
	if err := downloadSingleCloudFileToPath(&cobra.Command{}, store, job, runner.OutputFile{RelPath: "artifacts/summary.md"}, dest); err != nil {
		t.Fatalf("downloadSingleCloudFileToPath: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "summary\n" {
		t.Fatalf("downloaded content = %q, want summary", data)
	}
}

func TestFetchCloudArtifactByToken_BasenameMatchesArtifactsConventionOutput(t *testing.T) {
	job := setupLaunchArtifactJob(t)
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "artifacts/summary.md": []byte("summary\n"),
		},
	}

	oldOutput := artifactOutput
	artifactOutput = "-"
	t.Cleanup(func() { artifactOutput = oldOutput })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := fetchCloudArtifactByToken(cmd, store, job, "summary.md", false); err != nil {
		t.Fatalf("fetchCloudArtifactByToken: %v", err)
	}
	if got := out.String(); got != "summary\n" {
		t.Fatalf("stdout = %q, want summary", got)
	}
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

func TestSyncArtifactsForJob_FallsBackToConventionOutputs(t *testing.T) {
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

	prevCloud := syncCloudJobArtifactsFunc
	prevOutputs := syncJobOutputsFunc
	t.Cleanup(func() {
		syncCloudJobArtifactsFunc = prevCloud
		syncJobOutputsFunc = prevOutputs
	})

	outputsCalled := false
	syncCloudJobArtifactsFunc = func(*sql.DB, *r2.Client, *db.Job) (artifacts.SyncResult, error) {
		return artifacts.SyncResult{}, artifacts.ErrManifestMissing
	}
	syncJobOutputsFunc = func(*sql.DB, *db.Job) (artifacts.SyncResult, error) {
		outputsCalled = true
		return artifacts.SyncResult{Added: 2}, nil
	}

	if err := syncArtifactsForJob(database, job, nil, time.Second); err != nil {
		t.Fatalf("syncArtifactsForJob: %v", err)
	}
	if !outputsCalled {
		t.Fatal("expected convention output fallback")
	}
}

func TestRunArtifactSync_PrintsZeroConventionOutputs(t *testing.T) {
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

	prevCloud := syncCloudJobArtifactsFunc
	prevOutputs := syncJobOutputsFunc
	t.Cleanup(func() {
		syncCloudJobArtifactsFunc = prevCloud
		syncJobOutputsFunc = prevOutputs
	})
	syncCloudJobArtifactsFunc = func(*sql.DB, *r2.Client, *db.Job) (artifacts.SyncResult, error) {
		return artifacts.SyncResult{}, artifacts.ErrManifestMissing
	}
	syncJobOutputsFunc = func(*sql.DB, *db.Job) (artifacts.SyncResult, error) {
		return artifacts.SyncResult{}, nil
	}

	outBuf := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(outBuf)

	if err := runArtifactSync(c, []string{strconv.FormatInt(jobID, 10)}); err != nil {
		t.Fatalf("runArtifactSync: %v", err)
	}
	if got := outBuf.String(); !strings.Contains(got, "synced 0 convention-based outputs") {
		t.Fatalf("stdout = %q, want zero-output count", got)
	}
}

func TestSyncJobOutputsCachesDeclaredOutputDirectoryWithoutCompletionRecord(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	outDir := filepath.Join(workDir, "output", "bayes_course")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	outFile := filepath.Join(outDir, "exp_178.json")
	if err := os.WriteFile(outFile, []byte(`{"ok": true}`), 0o644); err != nil {
		t.Fatalf("write output file: %v", err)
	}

	jobID, err := db.RecordQueued(database, "studio", workDir, "python exp.py", "pep output")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobOutputs(database, jobID, []string{"output/bayes_course/"}); err != nil {
		t.Fatalf("SetJobOutputs: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevCompletion := completionOutputFilesFunc
	t.Cleanup(func() {
		completionOutputFilesFunc = prevCompletion
	})
	completionOutputFilesFunc = func(*db.Job) []runner.OutputFile {
		return nil
	}

	result, err := syncJobOutputs(database, job)
	if err != nil {
		t.Fatalf("syncJobOutputs: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("Added = %d, want 1", result.Added)
	}

	entries, err := db.ListArtifactsByJob(database, jobID)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("artifact entries = %+v, want one", entries)
	}
	if entries[0].Path != "output/bayes_course/exp_178.json" {
		t.Fatalf("artifact path = %q", entries[0].Path)
	}
}

func TestSyncCloudJobArtifactsWithStore_FallsBackToRunZeroManifest(t *testing.T) {
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
	if _, err := db.CreateAttempt(database, jobID, "vastai:12345", &instanceID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LatestRunID == nil || *job.LatestRunID == 0 {
		t.Fatalf("latest_run_id = %v, want non-zero", job.LatestRunID)
	}

	manifestKey := r2keys.JobAttemptArtifactManifest(job.ID, 0)
	filesPrefix := r2keys.JobAttemptArtifactFilesPrefix(job.ID, 0)

	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			manifestKey:                          []byte(`{"artifacts":[{"path":"results/metrics.json"}]}`),
			filesPrefix + "results/metrics.json": []byte(`{"acc":0.9}`),
		},
	}

	result, err := syncCloudJobArtifactsWithStore(database, store, job)
	if err != nil {
		t.Fatalf("syncCloudJobArtifactsWithStore: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("added = %d, want 1", result.Added)
	}

	entry, err := db.FindArtifactByNameOrPath(database, job.ID, "results/metrics.json")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
	if entry.Path != "results/metrics.json" {
		t.Fatalf("path = %q, want results/metrics.json", entry.Path)
	}
}

func TestRunArtifactListSync_ReadOnlySyncFailureFallsBackToCachedArtifacts(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "echo hi", "artifact list", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.UpsertArtifact(database, db.Artifact{
		JobID:      jobID,
		Name:       "model",
		Path:       "output/model.bin",
		StoredPath: "output/model.bin",
		SizeBytes:  1234,
		SHA256:     "abc123",
	}); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	prevSync := syncLocalJobArtifacts
	prevListSync := artifactListSync
	t.Cleanup(func() {
		syncLocalJobArtifacts = prevSync
		artifactListSync = prevListSync
	})
	syncLocalJobArtifacts = func(*sql.DB, *db.Job, time.Duration) (artifacts.SyncResult, error) {
		return artifacts.SyncResult{}, errors.New("attempt to write a readonly database (8)")
	}
	artifactListSync = true

	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(outBuf)
	c.SetErr(errBuf)

	if err := runArtifactList(c, []string{strconv.FormatInt(jobID, 10)}); err != nil {
		t.Fatalf("runArtifactList: %v", err)
	}

	if got := outBuf.String(); !strings.Contains(got, "output/model.bin") {
		t.Fatalf("stdout missing cached artifact listing, got:\n%s", got)
	}
	if got := errBuf.String(); !strings.Contains(got, "skipped artifact sync") {
		t.Fatalf("stderr missing readonly sync warning, got:\n%s", got)
	}
}

func TestRunArtifactListQueuedNotStartedShowsPlacementContext(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "H200"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, db.LaunchHost(instanceID), "/tmp/project", "echo hi", "artifact list", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	prevListSync := artifactListSync
	t.Cleanup(func() {
		artifactListSync = prevListSync
	})
	artifactListSync = false

	outBuf := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(outBuf)

	if err := runArtifactList(c, []string{strconv.FormatInt(jobID, 10)}); err != nil {
		t.Fatalf("runArtifactList: %v", err)
	}

	out := outBuf.String()
	if !strings.Contains(out, "No cached artifacts.") {
		t.Fatalf("missing empty artifact message, got:\n%s", out)
	}
	if !strings.Contains(out, "has not started yet; no artifacts are available") {
		t.Fatalf("missing not-started explanation, got:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("Placement:   assigned to wi%d", instanceID)) {
		t.Fatalf("missing placement context, got:\n%s", out)
	}
}
