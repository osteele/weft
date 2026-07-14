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
	"github.com/osteele/weft/internal/r2resolve"
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
	listErr              error // when set, ListObjects returns this for every prefix
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
	if s.listErr != nil {
		return nil, s.listErr
	}
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
	if job.LatestRunID == nil {
		t.Fatal("job missing latest run id")
	}
	runID := *job.LatestRunID
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
	if job.LatestRunID == nil {
		t.Fatal("job missing latest run id")
	}
	runID := *job.LatestRunID
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

func TestDeliverArtifactToken_BasenameMatchesArtifactsConventionOutput(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "artifacts/summary.md": []byte("summary\n"),
		},
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	oldOutput := artifactOutput
	artifactOutput = "-"
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactOutput = oldOutput
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := deliverArtifactToken(cmd, database, job.ID, job, "summary.md", false, false); err != nil {
		t.Fatalf("deliverArtifactToken: %v", err)
	}
	if got := out.String(); got != "summary\n" {
		t.Fatalf("stdout = %q, want summary", got)
	}
}

func TestDeliverArtifactToken_PrefersFinalCloudObjectOverPreEndCache(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	if job.LatestRunID == nil {
		t.Fatal("job missing latest run id")
	}
	runID := *job.LatestRunID

	staleSource := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(staleSource, []byte(`{"policies":["early"]}`+"\n"), 0o644); err != nil {
		t.Fatalf("write stale source: %v", err)
	}
	if err := artifacts.StoreLocalArtifact(database, job.ID, "output/result.json", staleSource); err != nil {
		t.Fatalf("StoreLocalArtifact: %v", err)
	}
	end := time.Now().Unix()
	if _, err := database.Exec(`UPDATE artifacts SET created_at = ? WHERE job_id = ? AND path = ?`, end-10, job.ID, "output/result.json"); err != nil {
		t.Fatalf("backdate cached artifact: %v", err)
	}
	if err := db.CloseAttempt(database, job.ID, db.StatusCompleted, intPtr(0), end); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	job, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "output/result.json": []byte(`{"policies":["early","final"]}` + "\n"),
		},
	}
	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	oldOutput := artifactOutput
	artifactOutput = "-"
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactOutput = oldOutput
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := deliverArtifactToken(cmd, database, job.ID, job, "output/result.json", false, false); err != nil {
		t.Fatalf("deliverArtifactToken: %v", err)
	}
	if got := out.String(); got != "{\"policies\":[\"early\",\"final\"]}\n" {
		t.Fatalf("stdout = %q, want final cloud artifact", got)
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

func TestDeliverArtifactToken_StallErrorIsNotReportedAsNotFound(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	store := &stallingCloudStore{&fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, 0) + "output/representations/train.pkl": []byte("payload"),
		},
	}}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	oldOutput := artifactOutput
	artifactOutput = t.TempDir()
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactOutput = oldOutput
	})

	err := deliverArtifactToken(&cobra.Command{}, database, job.ID, job, "output/representations/train.pkl", false, false)
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

func TestDeliverArtifactToken_FindsSupersededRunArtifact(t *testing.T) {
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

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	oldOutput := artifactOutput
	artifactOutput = "-"
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactOutput = oldOutput
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := deliverArtifactToken(cmd, database, job.ID, job, "output/result.json", false, false); err != nil {
		t.Fatalf("deliverArtifactToken: %v", err)
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

// TestArtifactListRowsAreRetrievableByCat is the list/get agreement
// regression (bugs wb11, wb16, wb20, wb40): every row `artifact list`
// prints must be retrievable by `artifact cat` using the exact path the
// listing showed, because both sides now consume resolveJobArtifacts.
func TestArtifactListRowsAreRetrievableByCat(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	t.Setenv("HOME", t.TempDir())

	cachedSource := filepath.Join(t.TempDir(), "cached.txt")
	if err := os.WriteFile(cachedSource, []byte("cached\n"), 0o644); err != nil {
		t.Fatalf("write cached source: %v", err)
	}
	if err := artifacts.StoreLocalArtifact(database, job.ID, "output/cached.txt", cachedSource); err != nil {
		t.Fatalf("StoreLocalArtifact: %v", err)
	}
	runID := int64(0)
	store := &fakeCloudArtifactStore{
		objects: map[string][]byte{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "output/cloud.txt":            []byte("cloud\n"),
			r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID) + "output/produced.bin":   []byte("produced\n"),
			r2keys.JobAttemptOutputsPrefix(job.ID, runID) + "artifacts/literal-alias.txt": []byte("alias\n"),
		},
	}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	oldListSync := artifactListSync
	artifactListSync = false
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		artifactListSync = oldListSync
	})

	var listOut bytes.Buffer
	listCmd := &cobra.Command{}
	listCmd.SetOut(&listOut)
	if err := runArtifactList(listCmd, []string{strconv.FormatInt(job.ID, 10)}); err != nil {
		t.Fatalf("runArtifactList: %v", err)
	}

	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(listOut.String()), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			t.Fatalf("unexpected list line %q", line)
		}
		paths = append(paths, fields[1])
	}
	if len(paths) != 4 {
		t.Fatalf("list rows = %v, want cached + 3 cloud rows", paths)
	}

	for _, path := range paths {
		var catOut bytes.Buffer
		catCmd := &cobra.Command{}
		catCmd.SetOut(&catOut)
		if err := runArtifactCat(catCmd, []string{strconv.FormatInt(job.ID, 10), path}); err != nil {
			t.Fatalf("runArtifactCat(%q): listed row not retrievable: %v", path, err)
		}
		if catOut.Len() == 0 {
			t.Fatalf("runArtifactCat(%q): no content", path)
		}
	}
}

func TestArtifactCatFallsThroughWhenCachedFileMissing(t *testing.T) {
	database, job := setupLaunchArtifactJobWithDB(t)
	t.Setenv("HOME", t.TempDir())

	_, err := database.Exec(
		`INSERT INTO artifacts (job_id, attempt_id, name, path, stored_path, size_bytes, sha256, created_at)
		 VALUES (?, NULL, ?, ?, ?, ?, ?, ?)`,
		job.ID, "result", "output/result.txt",
		filepath.Join(strconv.FormatInt(job.ID, 10), "missing", "result.txt"),
		6, "", time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("insert stale artifact: %v", err)
	}
	store := &fakeCloudArtifactStore{}

	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	t.Cleanup(func() { buildArtifactR2Client = oldBuild })

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err = runArtifactCat(cmd, []string{strconv.FormatInt(job.ID, 10), "output/result.txt"})
	if err == nil {
		t.Fatal("runArtifactCat succeeded unexpectedly")
	}
	if strings.Contains(err.Error(), "missing/result.txt") || strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("runArtifactCat returned stale local cache error: %v", err)
	}
}

// TestArtifactListHostRowRetrievableByCat covers the host_data pointer
// source of the shared resolver: a listed pointer row is retrieved through
// the on-demand host sync the row's handle encodes.
func TestArtifactListHostRowRetrievableByCat(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())
	if err := dataloc.InitSchema(database); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "host pointer row")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host: "host-alpha",
		Asset: dataloc.DataAsset{
			Kind: dataloc.AssetJobOutput,
			ID:   fmt.Sprintf("%d/output/result.json", jobID),
		},
		Path:      fmt.Sprintf("~/.cache/weft/artifacts/%d/1/outputs/output/result.json", jobID),
		SizeBytes: 12,
		LastSeen:  time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	sourcePath := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(sourcePath, []byte("from host\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return nil }
	oldSync := syncArtifactsForJobFunc
	syncArtifactsForJobFunc = func(database *sql.DB, job *db.Job, _ *r2.Client, _ time.Duration) error {
		return artifacts.StoreLocalArtifact(database, job.ID, "output/result.json", sourcePath)
	}
	oldListSync := artifactListSync
	artifactListSync = false
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
		syncArtifactsForJobFunc = oldSync
		artifactListSync = oldListSync
	})

	var listOut bytes.Buffer
	listCmd := &cobra.Command{}
	listCmd.SetOut(&listOut)
	if err := runArtifactList(listCmd, []string{strconv.FormatInt(jobID, 10)}); err != nil {
		t.Fatalf("runArtifactList: %v", err)
	}
	line := strings.TrimSpace(listOut.String())
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] != "output" {
		t.Fatalf("list output = %q, want a host pointer row", line)
	}

	var catOut bytes.Buffer
	catCmd := &cobra.Command{}
	catCmd.SetOut(&catOut)
	if err := runArtifactCat(catCmd, []string{strconv.FormatInt(jobID, 10), fields[1]}); err != nil {
		t.Fatalf("runArtifactCat(%q): listed host row not retrievable: %v", fields[1], err)
	}
	if got := catOut.String(); got != "from host\n" {
		t.Fatalf("stdout = %q, want host content", got)
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

	files, err := listCloudJobOutputFiles(store, job)
	if err != nil {
		t.Fatalf("listCloudJobOutputFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %+v, want the dual upload collapsed to one row", files)
	}
	if files[0].RelPath != "output/train.pkl" {
		t.Fatalf("RelPath = %q, want plain spelling preferred", files[0].RelPath)
	}
}

// TestListCloudJobOutputFiles_SurfacesListError guards wb40: an R2 ListObjects
// failure must surface as a non-nil error, distinct from a clean-empty listing.
// Reporting the same empty result for both lets an unreachable R2 masquerade as
// "no cloud artifacts".
func TestListCloudJobOutputFiles_SurfacesListError(t *testing.T) {
	job := setupLaunchArtifactJob(t)

	// Clean-empty: no objects, no injected error → empty result, nil error.
	empty := &fakeCloudArtifactStore{objects: map[string][]byte{}}
	files, err := listCloudJobOutputFiles(empty, job)
	if err != nil {
		t.Fatalf("clean-empty listing should not error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("clean-empty listing should be empty, got %+v", files)
	}

	// List failure: every ListObjects errors → empty result but a non-nil error.
	failing := &fakeCloudArtifactStore{objects: map[string][]byte{}, listErr: fmt.Errorf("R2 timeout")}
	files, err = listCloudJobOutputFiles(failing, job)
	if err == nil {
		t.Fatalf("R2 list failure should surface an error, got files=%+v", files)
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

func TestDownloadCloudArtifact_MissingObjectWrapsSentinel(t *testing.T) {
	store := &fakeCloudArtifactStore{objects: map[string][]byte{}}

	_, _, err := downloadCloudArtifact(store, "jobs/42/runs/7/files/", "output/missing.json", filepath.Join(t.TempDir(), "missing.json"), time.Second)
	if err == nil {
		t.Fatal("err = nil, want missing artifact error")
	}
	if !errors.Is(err, r2resolve.ErrArtifactMissing) {
		t.Fatalf("errors.Is(err, ErrArtifactMissing) = false; err=%v", err)
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
	if err := db.UpdateStartTime(database, jobID, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("UpdateStartTime: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevCompletion := completionRecordFunc
	prevIsLocal := isLocalJobHostFunc
	t.Cleanup(func() {
		completionRecordFunc = prevCompletion
		isLocalJobHostFunc = prevIsLocal
	})
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord {
		return nil
	}
	// Local discovery is only legitimate when the job executed on this
	// machine (spec: Attribution).
	isLocalJobHostFunc = func(string) bool { return true }

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

// syncJobOutputsTestJob creates a queued inventory-host job whose working dir
// resolves locally, returning the refreshed job row.
func syncJobOutputsTestJob(t *testing.T, database *sql.DB, workDir string, startTime int64) *db.Job {
	t.Helper()
	jobID, err := db.RecordQueued(database, "studio", workDir, "python exp.py", "output sync")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if startTime != 0 {
		if err := db.UpdateStartTime(database, jobID, startTime); err != nil {
			t.Fatalf("UpdateStartTime: %v", err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	return job
}

// stubSyncJobOutputsRemote installs remote-behavior fakes for syncJobOutputs
// tests: the job's host is treated as remote, and unset hooks fail the test
// if reached.
func stubSyncJobOutputsRemote(t *testing.T) {
	t.Helper()
	prevCompletion := completionRecordFunc
	prevDiscover := discoverRemoteJobOutputFilesFunc
	prevIsLocal := isLocalJobHostFunc
	prevSyncBack := syncOutputFilesBackFunc
	prevWarnf := syncOutputWarnf
	t.Cleanup(func() {
		completionRecordFunc = prevCompletion
		discoverRemoteJobOutputFilesFunc = prevDiscover
		isLocalJobHostFunc = prevIsLocal
		syncOutputFilesBackFunc = prevSyncBack
		syncOutputWarnf = prevWarnf
	})
	isLocalJobHostFunc = func(string) bool { return false }
	discoverRemoteJobOutputFilesFunc = func(job *db.Job, lower, upper time.Time) ([]runner.OutputFile, error) {
		t.Fatal("remote discovery must not run in this scenario")
		return nil, nil
	}
	syncOutputFilesBackFunc = func(host, remoteDir, localDir string, files []string, totalSizeMB, maxMB int) error {
		t.Fatal("SyncOutputFilesBack must not run in this scenario")
		return nil
	}
	syncOutputWarnf = func(string, ...any) {}
}

// TestSyncJobOutputsSkipsDiscoveryWithoutStartTime: a job with no recorded
// start time has no attribution window; discovery must be skipped with a
// warning instead of matching every file in the tree (live incident:
// wj3871–3873 each swept 182 submitter-local files).
func TestSyncJobOutputsSkipsDiscoveryWithoutStartTime(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "output"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A submitter-local file that a zero-threshold discovery would have swept.
	if err := os.WriteFile(filepath.Join(workDir, "output", "historic.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	job := syncJobOutputsTestJob(t, database, workDir, 0)
	stubSyncJobOutputsRemote(t)
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord { return nil }
	var warning string
	syncOutputWarnf = func(format string, args ...any) { warning = fmt.Sprintf(format, args...) }

	result, err := syncJobOutputs(database, job)
	if err != nil {
		t.Fatalf("syncJobOutputs: %v", err)
	}
	if result.Added != 0 {
		t.Fatalf("Added = %d, want 0", result.Added)
	}
	if !strings.Contains(warning, "no recorded start time") {
		t.Fatalf("warning = %q, want start-time skip notice", warning)
	}
	entries, err := db.ListArtifactsByJob(database, job.ID)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("artifacts = %+v, want none", entries)
	}
}

// TestSyncJobOutputsNeverRegistersSubmitterLocalFiles: files present in the
// submitter's local working tree are not attributable to a remote job without
// remote corroboration (spec: Attribution), even when they fall inside the
// attempt's time window.
func TestSyncJobOutputsNeverRegistersSubmitterLocalFiles(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "output"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "output", "local-only.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	job := syncJobOutputsTestJob(t, database, workDir, time.Now().Add(-time.Hour).Unix())
	stubSyncJobOutputsRemote(t)
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord { return nil }
	// Remote discovery runs (there IS a window) but finds nothing on the host.
	discoverRemoteJobOutputFilesFunc = func(job *db.Job, lower, upper time.Time) ([]runner.OutputFile, error) {
		return nil, nil
	}

	result, err := syncJobOutputs(database, job)
	if err != nil {
		t.Fatalf("syncJobOutputs: %v", err)
	}
	if result.Added != 0 {
		t.Fatalf("Added = %d, want 0 — local files must not be attributed", result.Added)
	}
	entries, err := db.ListArtifactsByJob(database, job.ID)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("artifacts = %+v, want none", entries)
	}
}

// TestSyncJobOutputsSyncsCompletionRecordFiles: a completion record with an
// output_files listing drives the existing rsync-back + store path.
func TestSyncJobOutputsSyncsCompletionRecordFiles(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	job := syncJobOutputsTestJob(t, database, workDir, time.Now().Add(-time.Hour).Unix())
	stubSyncJobOutputsRemote(t)
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord {
		return &runner.CompletionRecord{
			ExitCode:    0,
			OutputFiles: []runner.OutputFile{{RelPath: "output/result.json", SizeBytes: 2}},
		}
	}
	var syncedPaths []string
	syncOutputFilesBackFunc = func(host, remoteDir, localDir string, files []string, totalSizeMB, maxMB int) error {
		syncedPaths = files
		path := filepath.Join(localDir, "output", "result.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("{}"), 0o644)
	}

	result, err := syncJobOutputs(database, job)
	if err != nil {
		t.Fatalf("syncJobOutputs: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("Added = %d, want 1", result.Added)
	}
	if len(syncedPaths) != 1 || syncedPaths[0] != "output/result.json" {
		t.Fatalf("synced paths = %v", syncedPaths)
	}
	entry, err := db.FindArtifactByNameOrPath(database, job.ID, "output/result.json")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
	if entry.Path != "output/result.json" {
		t.Fatalf("artifact path = %q", entry.Path)
	}
}

// TestSyncJobOutputsRemoteDiscoveryFallback: when the completion record lacks
// output_files, outputs are discovered on the execution host within
// [start − 1s, end + 2m] and fetched through the existing sync path.
func TestSyncJobOutputsRemoteDiscoveryFallback(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	start := time.Now().Add(-time.Hour).Unix()
	end := start + 600
	job := syncJobOutputsTestJob(t, database, workDir, start)
	stubSyncJobOutputsRemote(t)
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord {
		return &runner.CompletionRecord{ExitCode: 0, StartTime: start, EndTime: end}
	}
	var gotLower, gotUpper time.Time
	discoverRemoteJobOutputFilesFunc = func(job *db.Job, lower, upper time.Time) ([]runner.OutputFile, error) {
		gotLower, gotUpper = lower, upper
		return []runner.OutputFile{{RelPath: "output/found.json", SizeBytes: 2}}, nil
	}
	syncOutputFilesBackFunc = func(host, remoteDir, localDir string, files []string, totalSizeMB, maxMB int) error {
		path := filepath.Join(localDir, "output", "found.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("{}"), 0o644)
	}

	result, err := syncJobOutputs(database, job)
	if err != nil {
		t.Fatalf("syncJobOutputs: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("Added = %d, want 1", result.Added)
	}
	wantLower := time.Unix(start, 0).Add(-time.Second)
	wantUpper := time.Unix(end, 0).Add(2 * time.Minute)
	if !gotLower.Equal(wantLower) {
		t.Fatalf("discovery lower bound = %v, want %v (start − 1s)", gotLower, wantLower)
	}
	if !gotUpper.Equal(wantUpper) {
		t.Fatalf("discovery upper bound = %v, want %v (end + 2m)", gotUpper, wantUpper)
	}
	if _, err := db.FindArtifactByNameOrPath(database, job.ID, "output/found.json"); err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
}

// TestSyncJobOutputsBackfillsStartTimeFromCompletionRecord: a completed
// attempt missing start_time in the DB (a completion-sync bug, see
// internal/ops/sync.go) is repaired from the host's completion record, which
// also unblocks time-window discovery.
func TestSyncJobOutputsBackfillsStartTimeFromCompletionRecord(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	start := time.Now().Add(-time.Hour).Unix()
	job := syncJobOutputsTestJob(t, database, workDir, 0)
	stubSyncJobOutputsRemote(t)
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord {
		return &runner.CompletionRecord{ExitCode: 0, StartTime: start}
	}
	discoverCalled := false
	discoverRemoteJobOutputFilesFunc = func(job *db.Job, lower, upper time.Time) ([]runner.OutputFile, error) {
		discoverCalled = true
		return nil, nil
	}

	if _, err := syncJobOutputs(database, job); err != nil {
		t.Fatalf("syncJobOutputs: %v", err)
	}
	if !discoverCalled {
		t.Fatal("expected discovery to run once start_time was backfilled")
	}
	updated, err := db.GetJobByID(database, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if updated.StartTime != start {
		t.Fatalf("DB start_time = %d, want %d (backfilled)", updated.StartTime, start)
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
	if err := db.UpdateStartTime(database, jobID, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("UpdateStartTime: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevSync := syncLocalJobArtifacts
	prevCompletion := completionRecordFunc
	prevIsLocal := isLocalJobHostFunc
	t.Cleanup(func() {
		syncLocalJobArtifacts = prevSync
		completionRecordFunc = prevCompletion
		isLocalJobHostFunc = prevIsLocal
	})
	syncLocalJobArtifacts = func(*sql.DB, *db.Job, time.Duration) (artifacts.SyncResult, error) {
		return artifacts.SyncResult{}, errors.New("scp: output/artifact-smoke/: not a regular file")
	}
	completionRecordFunc = func(*db.Job) *runner.CompletionRecord {
		return nil
	}
	isLocalJobHostFunc = func(string) bool { return true }

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

func TestRunArtifactListRunningLaunchJobExplainsPendingUpload(t *testing.T) {
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
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	prevListSync := artifactListSync
	t.Cleanup(func() {
		artifactListSync = prevListSync
	})
	artifactListSync = false
	store := &fakeCloudArtifactStore{objects: map[string][]byte{}}
	oldBuild := buildArtifactR2Client
	buildArtifactR2Client = func() cloudOutputStore { return store }
	t.Cleanup(func() {
		buildArtifactR2Client = oldBuild
	})

	outBuf := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(outBuf)

	if err := runArtifactList(c, []string{strconv.FormatInt(jobID, 10)}); err != nil {
		t.Fatalf("runArtifactList: %v", err)
	}

	out := outBuf.String()
	for _, want := range []string{
		"No cached artifacts.",
		"status is running",
		"artifacts are not available yet",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
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

	got := artifactNotFoundMessage(job, "metrics.json", true, false)
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

// TestArtifactNotFoundMessage_R2ListFailedIsUnknownNotAbsent guards wb40: when
// the R2 listing failed, the not-found message must convey uncertainty ("status
// unknown", "could not be located") rather than a confident "not found", so an
// unreachable R2 is never reported as a confirmed absence.
func TestArtifactNotFoundMessage_R2ListFailedIsUnknownNotAbsent(t *testing.T) {
	job := &db.Job{ID: 7}

	confident := artifactNotFoundMessage(job, "metrics.json", true, false)
	if !strings.Contains(confident, "not found") || strings.Contains(confident, "status unknown") {
		t.Fatalf("confident message should read as not-found: %q", confident)
	}

	unknown := artifactNotFoundMessage(job, "metrics.json", true, true)
	if !strings.Contains(unknown, "status unknown") {
		t.Fatalf("R2-list-failed message should say status unknown: %q", unknown)
	}
	if !strings.Contains(unknown, "could not be located") {
		t.Fatalf("R2-list-failed message should avoid a confident 'not found': %q", unknown)
	}
}
