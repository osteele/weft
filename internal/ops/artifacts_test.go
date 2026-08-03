package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
	_ "modernc.org/sqlite"
)

func setupArtifactTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database := db.SetupTestDB(t)
	if err := dataloc.InitSchema(database); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestRecordJobOutputs_Success(t *testing.T) {
	database := setupArtifactTestDB(t)

	exitCode := 0
	job := &db.Job{
		ID:       42,
		Host:     "host-beta",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
		Outputs:  []string{"checkpoint:llama-ft-v1", "hf:my-org/fine-tuned-model"},
	}

	RecordJobOutputs(database, job)

	// Verify assets were recorded
	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 recorded assets, got %d", len(entries))
	}

	// Verify checkpoint
	found := false
	for _, e := range entries {
		if e.Asset.Kind == dataloc.AssetCheckpoint && e.Asset.ID == "llama-ft-v1" {
			found = true
		}
	}
	if !found {
		t.Errorf("checkpoint:llama-ft-v1 not found in recorded assets: %v", entries)
	}
}

func TestRecordJobOutputs_FailedJob(t *testing.T) {
	database := setupArtifactTestDB(t)

	exitCode := 1
	job := &db.Job{
		ID:       43,
		Host:     "host-beta",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
		Outputs:  []string{"checkpoint:should-not-record"},
	}

	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed job should not record outputs, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_NoOutputs(t *testing.T) {
	database := setupArtifactTestDB(t)

	exitCode := 0
	job := &db.Job{
		ID:       44,
		Host:     "host-beta",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
	}

	// Should not panic or error
	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("job with no outputs should record nothing, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_NilJob(t *testing.T) {
	database := setupArtifactTestDB(t)
	// Should not panic
	RecordJobOutputs(database, nil)

	entries, err := dataloc.ListAllAssets(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("nil job should record nothing, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_KilledJob(t *testing.T) {
	database := setupArtifactTestDB(t)

	job := &db.Job{
		ID:      45,
		Host:    "host-beta",
		Status:  db.StatusKilled,
		Outputs: []string{"checkpoint:should-not-record"},
	}

	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("killed job should not record outputs, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_LocalOutput(t *testing.T) {
	database := setupArtifactTestDB(t)
	prev := snapshotRemoteJobOutputFunc
	t.Cleanup(func() { snapshotRemoteJobOutputFunc = prev })
	snapshotRemoteJobOutputFunc = func(_ *sql.DB, job *db.Job, relPath string, _ time.Duration) (string, error) {
		if job.ID != 46 || relPath != "cache/representations" {
			t.Fatalf("snapshot args = job %d path %q", job.ID, relPath)
		}
		return "~/.cache/weft/artifacts/46/7/outputs/cache/representations", nil
	}

	exitCode := 0
	runID := int64(7)
	job := &db.Job{
		ID:          46,
		Host:        "host-beta",
		Status:      db.StatusCompleted,
		ExitCode:    &exitCode,
		Outputs:     []string{"local:cache/representations/"},
		LatestRunID: &runID,
	}

	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 recorded asset, got %d", len(entries))
	}
	if entries[0].Asset.Kind != dataloc.AssetJobOutput || entries[0].Asset.ID != "46/cache/representations" {
		t.Fatalf("unexpected asset = %+v", entries[0].Asset)
	}
	if entries[0].Path != "~/.cache/weft/artifacts/46/7/outputs/cache/representations" {
		t.Fatalf("path = %q, want snapshot path", entries[0].Path)
	}
}

func TestRecordJobOutputs_ProducesSnapshot(t *testing.T) {
	database := setupArtifactTestDB(t)
	prev := snapshotRemoteJobOutputFunc
	t.Cleanup(func() { snapshotRemoteJobOutputFunc = prev })
	var snapshotted []string
	snapshotRemoteJobOutputFunc = func(_ *sql.DB, job *db.Job, relPath string, _ time.Duration) (string, error) {
		snapshotted = append(snapshotted, relPath)
		return "~/.cache/weft/artifacts/48/3/outputs/" + relPath, nil
	}

	exitCode := 0
	runID := int64(3)
	job := &db.Job{
		ID:          48,
		Host:        "host-beta",
		Status:      db.StatusCompleted,
		ExitCode:    &exitCode,
		Produces:    []string{"output/model.pt", "cache/reps:48"},
		LatestRunID: &runID,
	}

	RecordJobOutputs(database, job)

	if len(snapshotted) != 2 {
		t.Fatalf("snapshotted = %v, want both produces paths", snapshotted)
	}

	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 recorded assets, got %d: %v", len(entries), entries)
	}
	byID := map[string]string{}
	for _, e := range entries {
		if e.Asset.Kind != dataloc.AssetJobOutput {
			t.Fatalf("asset kind = %q, want job-output", e.Asset.Kind)
		}
		byID[e.Asset.ID] = e.Path
	}
	if byID["48/output/model.pt"] != "~/.cache/weft/artifacts/48/3/outputs/output/model.pt" {
		t.Fatalf("model.pt path = %q, want snapshot path", byID["48/output/model.pt"])
	}
	if byID["48/cache/reps"] != "~/.cache/weft/artifacts/48/3/outputs/cache/reps" {
		t.Fatalf("cache/reps path = %q, want versioned spec stripped to path", byID["48/cache/reps"])
	}
}

func TestRecordJobOutputs_ProducesDeduplicatedAgainstLocalOutputs(t *testing.T) {
	database := setupArtifactTestDB(t)
	prev := snapshotRemoteJobOutputFunc
	t.Cleanup(func() { snapshotRemoteJobOutputFunc = prev })
	snapshotCalls := 0
	snapshotRemoteJobOutputFunc = func(_ *sql.DB, _ *db.Job, relPath string, _ time.Duration) (string, error) {
		snapshotCalls++
		return "~/.cache/weft/artifacts/49/1/outputs/" + relPath, nil
	}

	exitCode := 0
	job := &db.Job{
		ID:       49,
		Host:     "host-beta",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
		Outputs:  []string{"local:output/model.pt"},
		Produces: []string{"output/model.pt"},
	}

	RecordJobOutputs(database, job)

	if snapshotCalls != 1 {
		t.Fatalf("snapshot calls = %d, want the duplicate path snapshotted once", snapshotCalls)
	}
	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 recorded asset, got %d: %v", len(entries), entries)
	}
}

func TestRecordJobOutputs_LocalOutputSnapshotFailureSkipsMutablePath(t *testing.T) {
	database := setupArtifactTestDB(t)
	prev := snapshotRemoteJobOutputFunc
	t.Cleanup(func() { snapshotRemoteJobOutputFunc = prev })
	snapshotRemoteJobOutputFunc = func(*sql.DB, *db.Job, string, time.Duration) (string, error) {
		return "", errors.New("missing output")
	}

	exitCode := 0
	job := &db.Job{
		ID:         47,
		Host:       "host-beta",
		WorkingDir: "/tmp/project",
		Status:     db.StatusCompleted,
		ExitCode:   &exitCode,
		Outputs:    []string{"local:output/result.json"},
	}

	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "host-beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no mutable host_data entry after snapshot failure, got %d", len(entries))
	}
}

func snapshotTestJob(endUnix int64) *db.Job {
	return &db.Job{
		ID:          50,
		Host:        "host-beta",
		WorkingDir:  "~/code/project",
		Status:      db.StatusCompleted,
		ExitCode:    intPtr(0),
		EndTime:     &endUnix,
		LatestRunID: int64Ptr(3),
	}
}

// Regression: the snapshot must refuse to copy a declared output whose mtime
// postdates the attempt's end bound (spec: CompletedOutputsSurviveWorkdirReuse).
func TestSnapshotRemoteJobOutput_RefusesPostAttemptOverwrite(t *testing.T) {
	t.Cleanup(ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", snapshotStaleMarker + " newest=1700009999", errors.New("exit status 4")
	}))

	_, err := snapshotRemoteJobOutput(nil, snapshotTestJob(1_700_000_000), "output/result.json", time.Second)
	if err == nil || !strings.Contains(err.Error(), "modified after the attempt ended") {
		t.Fatalf("snapshotRemoteJobOutput() error = %v, want freshness refusal", err)
	}
}

func TestSnapshotRemoteJobOutput_ProbeCarriesEndBound(t *testing.T) {
	var gotCmd string
	t.Cleanup(ssh.SetRunner(func(host, command string) (string, string, error) {
		gotCmd = command
		return "", "", nil
	}))

	end := int64(1_700_000_000)
	snapshotPath, err := snapshotRemoteJobOutput(nil, snapshotTestJob(end), "output/result.json", time.Second)
	if err != nil {
		t.Fatalf("snapshotRemoteJobOutput() error = %v", err)
	}
	upper := fmt.Sprintf("%d", end+int64(artifacts.AttemptOutputEndSlack/time.Second))
	if !strings.Contains(gotCmd, upper) {
		t.Fatalf("remote probe %q does not carry the end bound %s", gotCmd, upper)
	}
	if !strings.Contains(snapshotPath, "50/3/outputs/output/result.json") {
		t.Fatalf("snapshotPath = %q, want per-run snapshot path", snapshotPath)
	}
}

func TestSnapshotRemoteJobOutput_UnknownEndTimeRefuses(t *testing.T) {
	sshCalled := false
	t.Cleanup(ssh.SetRunner(func(host, command string) (string, string, error) {
		sshCalled = true
		return "", "", nil
	}))

	job := snapshotTestJob(1_700_000_000)
	job.EndTime = nil
	_, err := snapshotRemoteJobOutput(nil, job, "output/result.json", time.Second)
	if err == nil || !strings.Contains(err.Error(), "end time unknown") {
		t.Fatalf("snapshotRemoteJobOutput() error = %v, want unknown-end refusal", err)
	}
	if sshCalled {
		t.Fatal("snapshotRemoteJobOutput() probed the host without a verifiable window")
	}
}
