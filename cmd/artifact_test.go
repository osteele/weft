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
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
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
	objects              map[string][]byte
	objectExistsOverride map[string]bool
}

func (s *fakeCloudArtifactStore) GetObject(_ context.Context, key string) ([]byte, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeCloudArtifactStore) ObjectExists(_ context.Context, key string) (bool, error) {
	if s.objectExistsOverride != nil {
		if exists, ok := s.objectExistsOverride[key]; ok {
			return exists, nil
		}
	}
	_, ok := s.objects[key]
	return ok, nil
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

func setupLaunchArtifactJobWithDB(t *testing.T) (*sql.DB, *db.Job) {
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
	return database, job
}

func setupLaunchArtifactJob(t *testing.T) *db.Job {
	t.Helper()
	_, job := setupLaunchArtifactJobWithDB(t)
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
	if err := downloadSingleCloudFileToPath(&cobra.Command{}, store, job, runner.OutputFile{RelPath: "artifacts/summary.md"}, dest, false); err != nil {
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

func TestDownloadSingleCloudFileToPath_UsesListedR2Key(t *testing.T) {
	job := setupLaunchArtifactJob(t)
	runID := int64(0)
	key := r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "output/summary.md"
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			key: []byte("summary\n"),
		},
		objectExistsOverride: map[string]bool{
			key: false,
		},
	}

	dest := filepath.Join(t.TempDir(), "summary.md")
	if err := downloadSingleCloudFileToPath(&cobra.Command{}, store, job, runner.OutputFile{RelPath: "output/summary.md", SizeBytes: 8, R2Key: key}, dest, false); err != nil {
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

func TestRunArtifactCatFallsBackToCloudOutputPath(t *testing.T) {
	_, job := setupLaunchArtifactJobWithDB(t)
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "output/exp049/summary.md": []byte("summary\n"),
		},
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	t.Cleanup(func() { buildArtifactR2Client = oldBuild })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runArtifactCat(cmd, []string{strconv.FormatInt(job.ID, 10), "output/exp049/summary.md"}); err != nil {
		t.Fatalf("runArtifactCat: %v", err)
	}
	if got := out.String(); got != "summary\n" {
		t.Fatalf("stdout = %q, want summary", got)
	}
}

func TestRunArtifactCatResolvesArtifactFileWithOutputPrefix(t *testing.T) {
	_, job := setupLaunchArtifactJobWithDB(t)
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID) + "output/exp_037/results.json": []byte(`{"ok":true}` + "\n"),
		},
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	t.Cleanup(func() { buildArtifactR2Client = oldBuild })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runArtifactCat(cmd, []string{strconv.FormatInt(job.ID, 10), "output/exp_037/results.json"}); err != nil {
		t.Fatalf("runArtifactCat: %v", err)
	}
	if got := out.String(); got != "{\"ok\":true}\n" {
		t.Fatalf("stdout = %q, want JSON result", got)
	}
}

func TestRunArtifactCatFallsBackToCloudArtifactsAliasPath(t *testing.T) {
	_, job := setupLaunchArtifactJobWithDB(t)
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "artifacts/summary.md": []byte("summary\n"),
		},
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	t.Cleanup(func() { buildArtifactR2Client = oldBuild })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runArtifactCat(cmd, []string{strconv.FormatInt(job.ID, 10), "artifacts/summary.md"}); err != nil {
		t.Fatalf("runArtifactCat: %v", err)
	}
	if got := out.String(); got != "summary\n" {
		t.Fatalf("stdout = %q, want summary", got)
	}
}

func TestFetchAllArtifactsDownloadsCachedAndCloudOutputs(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	t.Setenv("HOME", t.TempDir())
	runID := int64(0)
	sourceDir := t.TempDir()
	cachedSource := filepath.Join(sourceDir, "cached.txt")
	if err := os.WriteFile(cachedSource, []byte("cached\n"), 0o644); err != nil {
		t.Fatalf("write cached source: %v", err)
	}
	if err := artifacts.StoreLocalArtifact(database, job.ID, "output/cached.txt", cachedSource); err != nil {
		t.Fatalf("StoreLocalArtifact: %v", err)
	}
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "output/cached.txt": []byte("cloud duplicate\n"),
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "output/cloud.txt":  []byte("cloud\n"),
		},
	}

	oldBuild := buildArtifactR2Client
	oldOutput := artifactOutput
	oldParallel := artifactParallel
	outputDir := t.TempDir()
	buildArtifactR2Client = func() cloudOutputStore { return store }
	artifactOutput = outputDir
	artifactParallel = 1
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactOutput = oldOutput
		artifactParallel = oldParallel
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := fetchAllArtifactsForJobs(cmd, []int64{job.ID}); err != nil {
		t.Fatalf("fetchAllArtifactsForJobs: %v", err)
	}

	cachedDest := filepath.Join(outputDir, ids.FormatJobID(job.ID), "output", "cached.txt")
	cachedData, err := os.ReadFile(cachedDest)
	if err != nil {
		t.Fatalf("read cached dest: %v", err)
	}
	if string(cachedData) != "cached\n" {
		t.Fatalf("cached dest = %q, want cached content", cachedData)
	}

	cloudDest := filepath.Join(outputDir, ids.FormatJobID(job.ID), "output", "cloud.txt")
	cloudData, err := os.ReadFile(cloudDest)
	if err != nil {
		t.Fatalf("read cloud dest: %v", err)
	}
	if string(cloudData) != "cloud\n" {
		t.Fatalf("cloud dest = %q, want cloud content", cloudData)
	}
}

// stallingCloudStore reports objects as present but fails every transfer,
// simulating a download that stalls past the idle timeout.
type stallingCloudStore struct {
	*fakeCloudArtifactStore
}

func (s *stallingCloudStore) DownloadObjectToWriterWithIdleTimeout(context.Context, string, io.Writer, time.Duration) (int64, error) {
	return 0, errors.New("download stalled: no progress for 2m0s")
}

func (s *stallingCloudStore) DownloadObjectToFileWithIdleTimeout(context.Context, string, string, time.Duration) (int64, error) {
	return 0, errors.New("download stalled: no progress for 2m0s")
}

func TestFetchCloudArtifactByToken_StallErrorIsNotReportedAsNotFound(t *testing.T) {
	job := setupLaunchArtifactJob(t)
	store := &stallingCloudStore{&fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, 0) + "output/representations/train.pkl": []byte("payload"),
		},
	}}

	oldOutput := artifactOutput
	artifactOutput = t.TempDir()
	t.Cleanup(func() { artifactOutput = oldOutput })

	err := fetchCloudArtifactByToken(&cobra.Command{}, store, job, "output/representations/train.pkl", false)
	if err == nil {
		t.Fatal("expected stall error")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("error = %q, want the stall cause preserved", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, must not be masked as not-found", err)
	}
}

func TestFetchCloudArtifactByToken_FindsSupersededRunArtifact(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	instanceID := *job.LaunchID
	if _, err := db.CreateAttempt(database, job.ID, "vastai:99999", &instanceID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	job, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LatestRunID == nil || *job.LatestRunID == 0 {
		t.Fatalf("latest_run_id = %v, want non-zero", job.LatestRunID)
	}
	// Upload lives under a run that is neither latest_run_id nor run zero —
	// the superseded-attempt case (latest_run_id moved past the run that ran).
	supersededRun := *job.LatestRunID + 17
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, supersededRun) + "output/result.json": []byte("superseded\n"),
		},
	}

	oldOutput := artifactOutput
	artifactOutput = "-"
	t.Cleanup(func() { artifactOutput = oldOutput })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := fetchCloudArtifactByToken(cmd, store, job, "output/result.json", false); err != nil {
		t.Fatalf("fetchCloudArtifactByToken: %v", err)
	}
	if got := out.String(); got != "superseded\n" {
		t.Fatalf("stdout = %q, want superseded content", got)
	}
}

func TestRunArtifactCat_InventoryHostJobReadsR2Outputs(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "inventory r2")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(jobID, 0) + "output/metrics.json": []byte("{\"acc\":1}\n"),
		},
	}
	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	t.Cleanup(func() { buildArtifactR2Client = oldBuild })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runArtifactCat(cmd, []string{strconv.FormatInt(jobID, 10), "output/metrics.json"}); err != nil {
		t.Fatalf("runArtifactCat: %v", err)
	}
	if got := out.String(); got != "{\"acc\":1}\n" {
		t.Fatalf("stdout = %q, want R2 content", got)
	}
}

func TestFetchArtifactForJobs_SyncsOnDemandFromInventoryHost(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "on-demand sync")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return nil }
	oldSync := syncArtifactsForJobFunc
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		syncArtifactsForJobFunc = oldSync
	})
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "result.json")
	if err := os.WriteFile(sourcePath, []byte("from host\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	syncCalled := false
	syncArtifactsForJobFunc = func(database *sql.DB, job *db.Job, _ *r2.Client, _ time.Duration) error {
		syncCalled = true
		return artifacts.StoreLocalArtifact(database, job.ID, "output/result.json", sourcePath)
	}

	outDir := t.TempDir()
	oldOutput := artifactOutput
	artifactOutput = filepath.Join(outDir, "result.json")
	t.Cleanup(func() { artifactOutput = oldOutput })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := fetchArtifactForJobs(cmd, []int64{jobID}, "output/result.json"); err != nil {
		t.Fatalf("fetchArtifactForJobs: %v", err)
	}
	if !syncCalled {
		t.Fatal("expected on-demand sync")
	}
	data, err := os.ReadFile(filepath.Join(outDir, "result.json"))
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(data) != "from host\n" {
		t.Fatalf("dest = %q, want host content", data)
	}
}

func TestFetchArtifactForJobs_NotFoundNamesCheckedSources(t *testing.T) {
	db.SetupTestDB(t)
	database, err := db.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "not found msg")
	database.Close()
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return nil }
	oldSync := syncArtifactsForJobFunc
	syncArtifactsForJobFunc = func(*sql.DB, *db.Job, *r2.Client, time.Duration) error { return nil }
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		syncArtifactsForJobFunc = oldSync
	})

	err = fetchArtifactForJobs(&cobra.Command{}, []int64{jobID}, "output/missing.json")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "host-alpha") {
		t.Fatalf("error = %q, want the checked host named", err)
	}
}

func TestListCloudJobOutputFiles_DedupesArtifactSpelling(t *testing.T) {
	job := setupLaunchArtifactJob(t)
	payload := []byte("same payload")
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, 0) + "output/train.pkl":       payload,
			r2keys.JobAttemptArtifactFilesPrefix(job.ID, 0) + "output/train.pkl": payload,
		},
	}

	files := listCloudJobOutputFiles(store, job)
	if len(files) != 1 {
		t.Fatalf("files = %+v, want the dual upload collapsed to one row", files)
	}
	if files[0].RelPath != "output/train.pkl" {
		t.Fatalf("RelPath = %q, want plain spelling preferred", files[0].RelPath)
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

func TestSyncRecordedJobOutputAssetsUsesSnapshotPath(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := dataloc.InitSchema(database); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	entry := dataloc.HostDataEntry{
		Host: "studio",
		Asset: dataloc.DataAsset{
			Kind: dataloc.AssetJobOutput,
			ID:   "52/output/result.json",
		},
		Path:     "~/.cache/weft/artifacts/52/9/outputs/output/result.json",
		LastSeen: time.Now(),
	}
	if err := dataloc.RecordAsset(database, entry); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	prev := storeRemoteJobOutputAssetFunc
	t.Cleanup(func() { storeRemoteJobOutputAssetFunc = prev })
	var gotRemotePath string
	storeRemoteJobOutputAssetFunc = func(database *sql.DB, job *db.Job, relPath, remotePath string, _ func(string, string, string) error) error {
		gotRemotePath = remotePath
		return db.UpsertArtifact(database, db.Artifact{
			JobID:      job.ID,
			Path:       relPath,
			StoredPath: filepath.Join("52", relPath),
		})
	}

	job := &db.Job{
		ID:         52,
		Host:       "studio",
		WorkingDir: "/tmp/project",
	}
	result, err := syncRecordedJobOutputAssets(database, job)
	if err != nil {
		t.Fatalf("syncRecordedJobOutputAssets: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("Added = %d, want 1", result.Added)
	}
	if gotRemotePath != "~/.cache/weft/artifacts/52/9/outputs/output/result.json" {
		t.Fatalf("remote path = %q, want snapshot path", gotRemotePath)
	}
	art, err := db.FindArtifactByNameOrPath(database, 52, "output/result.json")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
	if art.Path != "output/result.json" {
		t.Fatalf("artifact path = %q", art.Path)
	}
}

func TestRecordedJobOutputRemotePathResolvesLegacyRelativePath(t *testing.T) {
	job := &db.Job{WorkingDir: "~/project"}
	got := recordedJobOutputRemotePath(job, "output/result.json")
	if got != "~/project/output/result.json" {
		t.Fatalf("remote path = %q", got)
	}
}

func TestSyncArtifactsForJobFallsBackToDeclaredOutputFilesWhenManifestDirectorySyncFails(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	outDir := filepath.Join(workDir, "output", "artifact-smoke")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "result.json"), []byte(`{"ok": true}`), 0o644); err != nil {
		t.Fatalf("write output file: %v", err)
	}

	jobID, err := db.RecordQueued(database, "studio", workDir, "python artifact_smoke_test.py", "artifact smoke")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobOutputs(database, jobID, []string{"local:output/artifact-smoke/"}); err != nil {
		t.Fatalf("SetJobOutputs: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevSync := syncLocalJobArtifacts
	prevCompletion := completionOutputFilesFunc
	t.Cleanup(func() {
		syncLocalJobArtifacts = prevSync
		completionOutputFilesFunc = prevCompletion
	})
	syncLocalJobArtifacts = func(*sql.DB, *db.Job, time.Duration) (artifacts.SyncResult, error) {
		return artifacts.SyncResult{}, errors.New("scp: output/artifact-smoke/: not a regular file")
	}
	completionOutputFilesFunc = func(*db.Job) []runner.OutputFile {
		return nil
	}

	if err := syncArtifactsForJob(database, job, nil, time.Second); err != nil {
		t.Fatalf("syncArtifactsForJob: %v", err)
	}

	entry, err := db.FindArtifactByNameOrPath(database, jobID, "output/artifact-smoke/result.json")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
	if entry.Path != "output/artifact-smoke/result.json" {
		t.Fatalf("path = %q, want output/artifact-smoke/result.json", entry.Path)
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

func TestRunArtifactListOnPremEmptyShowsHostOutputHint(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordJobStarting(database, "cool30", "/mnt/project", "echo hi", "artifact list")
	if err != nil {
		t.Fatalf("RecordJobStarting: %v", err)
	}
	if err := db.UpdateJobRunning(database, jobID); err != nil {
		t.Fatalf("UpdateJobRunning: %v", err)
	}
	if err := db.RecordCompletionByID(database, jobID, 0, time.Now().Unix()); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}
	if err := db.SetJobOutputDirs(database, jobID, []string{"output/"}); err != nil {
		t.Fatalf("SetJobOutputDirs: %v", err)
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
	for _, want := range []string{
		"No cached artifacts.",
		"On-prem job outputs are not uploaded to R2.",
		"cool30:/mnt/project/output/",
		"weft artifact sync " + ids.FormatJobID(jobID),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestArtifactNotFoundMessageForOnPremNamesHostOutputLocations(t *testing.T) {
	job := &db.Job{
		ID:         42,
		Host:       "cool30",
		WorkingDir: "/mnt/project",
		OutputDirs: []string{"output/"},
	}

	got := artifactNotFoundMessage(job, "metrics.json", true)
	for _, want := range []string{
		"checked local cache, R2, cool30 via artifact sync",
		"on-prem outputs are not uploaded to R2",
		"cool30:/mnt/project/output/",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q, got %q", want, got)
		}
	}
}
