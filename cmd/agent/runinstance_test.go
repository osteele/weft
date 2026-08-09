package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

func TestTerminalOutcomeForSequence(t *testing.T) {
	tests := []struct {
		name       string
		result     jobSequenceResult
		wantStatus string
		wantReason string
	}{
		{
			name:       "successful run",
			result:     jobSequenceResult{StartedJobCount: 1},
			wantStatus: db.LaunchStatusCompleted,
			wantReason: db.TerminationReasonCompleted,
		},
		{
			name:       "failed run",
			result:     jobSequenceResult{StartedJobCount: 1, AnyFailed: true},
			wantStatus: db.LaunchStatusFailed,
			wantReason: db.TerminationReasonJobFailure,
		},
		{
			name:       "infra failed run",
			result:     jobSequenceResult{StartedJobCount: 1, AnyFailed: true, AnyInfraFailed: true},
			wantStatus: db.LaunchStatusFailed,
			wantReason: db.TerminationReasonInfraFailure,
		},
		{
			name:       "canceled before any job started",
			result:     jobSequenceResult{AnyCanceled: true},
			wantStatus: db.LaunchStatusCancelled,
			wantReason: db.TerminationReasonCancelled,
		},
		{
			name:       "canceled after at least one job started",
			result:     jobSequenceResult{StartedJobCount: 1, AnyCanceled: true},
			wantStatus: db.LaunchStatusCompleted,
			wantReason: db.TerminationReasonCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotReason := terminalOutcomeForSequence(tt.result)
			if gotStatus != tt.wantStatus || gotReason != tt.wantReason {
				t.Fatalf("terminalOutcomeForSequence() = (%q, %q), want (%q, %q)",
					gotStatus, gotReason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

// rcloneTimeoutForBytes was the legacy size-derived timeout helper. It has
// been superseded by the stall-watchdog + ceiling drain gate in
// internal/r2upload; see TestComputeCeiling there for the equivalent
// regression coverage.

func TestUploadLiveFileSnapshotUsesRcatFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "telemetry.jsonl")
	want := "{\"event\":\"tick\"}\n"
	if err := os.WriteFile(sourcePath, []byte(want), 0o644); err != nil {
		t.Fatalf("write live file: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake rclone bin dir: %v", err)
	}
	capturePath := filepath.Join(dir, "capture")
	argsPath := filepath.Join(dir, "args")
	rcloneScript := `#!/bin/sh
printf '%s\n' "$*" > "$RCLONE_ARGS"
if [ "$1" != "rcat" ]; then
  echo "unexpected rclone command: $*" >&2
  exit 1
fi
if [ "$2" != "r2:test-bucket/live/key.jsonl" ]; then
  echo "unexpected rclone target: $2" >&2
  exit 1
fi
cat > "$RCLONE_CAPTURE"
`
	if err := os.WriteFile(filepath.Join(binDir, "rclone"), []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RCLONE_ARGS", argsPath)
	t.Setenv("RCLONE_CAPTURE", capturePath)

	uploadLiveFileSnapshot("test-bucket", 123, sourcePath, "live/key.jsonl", "live telemetry")

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured rclone stdin: %v", err)
	}
	if string(got) != want {
		t.Fatalf("uploaded stdin = %q, want %q", got, want)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured rclone args: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "rcat r2:test-bucket/live/key.jsonl" {
		t.Fatalf("rclone args = %q, want rcat target", got)
	}
}

func TestReadLogTail_SmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 8192)
	if got != content {
		t.Errorf("readLogTail() = %q, want %q", got, content)
	}
}

func TestReadLogTail_LargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write more than maxBytes
	var data []byte
	for i := 0; i < 200; i++ {
		data = append(data, []byte("this is a log line with some content\n")...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 100)
	if len(got) > 100 {
		t.Errorf("readLogTail() returned %d bytes, want <= 100", len(got))
	}
	// Should contain the end of the file
	if got[len(got)-1] != '\n' {
		t.Error("readLogTail() should end with newline")
	}
}

func TestReadLogTail_MissingFile(t *testing.T) {
	got := readLogTail("/nonexistent/path/file.log", 8192)
	if got != "" {
		t.Errorf("readLogTail() = %q, want empty string", got)
	}
}

func TestReadLogTail_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 8192)
	if got != "" {
		t.Errorf("readLogTail() = %q, want empty string", got)
	}
}

func TestOutputUploadRcloneArgs(t *testing.T) {
	if got := outputUploadRcloneArgs(time.Time{}); len(got) != 1 || got[0] != "--update" {
		t.Errorf("outputUploadRcloneArgs(zero) = %v, want [--update]", got)
	}
	windowStart := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	got := outputUploadRcloneArgs(windowStart)
	want := []string{"--update", "--max-age", "2026-08-02T12:00:00Z"}
	if len(got) != len(want) {
		t.Fatalf("outputUploadRcloneArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outputUploadRcloneArgs() = %v, want %v", got, want)
		}
	}
}

// Regression: the upload walk must not count (or upload) files that predate
// the attempt — a prior same-workdir job's leftovers in output/ (spec:
// invariant Attribution mechanism (d) in specs/job-lifecycle.allium).
func TestMeasureUploadTreeSince_ExcludesPriorJobFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "prior-job.json")
	fresh := filepath.Join(dir, "this-job.json")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("new-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	files, bytes, ok := measureUploadTreeSince(dir, time.Now().Add(-time.Hour))
	if !ok {
		t.Fatal("measureUploadTreeSince() ok = false")
	}
	if files != 1 || bytes != int64(len("new-data")) {
		t.Errorf("measureUploadTreeSince() = (%d files, %d bytes), want (1, %d)", files, bytes, len("new-data"))
	}

	// Zero threshold counts everything.
	files, _, ok = measureUploadTreeSince(dir, time.Time{})
	if !ok || files != 2 {
		t.Errorf("measureUploadTreeSince(zero) = %d files, want 2", files)
	}
}

// fakeRcloneOK puts a no-op rclone on PATH so upload helpers can run without
// touching R2.
func fakeRcloneOK(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\ncat > /dev/null 2>/dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeRawArtifactManifest writes raw manifest bytes for malformed or empty
// cases; well-formed manifests go through artifacts.WriteManifestFile.
func writeRawArtifactManifest(t *testing.T, jobID int64, content string) {
	t.Helper()
	manifestPath := runner.ExpandTilde(artifacts.RemoteManifestPath(jobID))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Regression: a declared artifact missing from disk must yield a failed
// per-entry outcome (spec: PerArtifactUploadOutcomeRecorded).
func TestUploadArtifactManifestEntries_MissingArtifactRecordedAsFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeRcloneOK(t)
	workDir := t.TempDir()
	manifestPath := runner.ExpandTilde(artifacts.RemoteManifestPath(61))
	err := artifacts.WriteManifestFile(manifestPath, artifacts.Manifest{
		JobID:     61,
		Artifacts: []artifacts.ArtifactSpec{{Path: "output/never-written.pt"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := uploadArtifactManifestEntries("bucket", 61, 1, workDir, nil)

	if result.Status != runner.UploadStatusFailed {
		t.Fatalf("Status = %q, want failed", result.Status)
	}
	if len(result.Dirs) != 1 || result.Dirs[0].Dir != "output/never-written.pt" ||
		result.Dirs[0].Status != runner.UploadStatusFailed || result.Dirs[0].Error == "" {
		t.Fatalf("Dirs = %+v, want one failed entry for the missing artifact", result.Dirs)
	}
}

// Regression: an unreadable manifest fails the upload rather than reporting a
// clean empty one (spec: PerArtifactUploadOutcomeRecorded).
func TestUploadArtifactManifestEntries_UnreadableManifestFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeRawArtifactManifest(t, 62, `{"job_id": 62, "artifacts": [`)

	result := uploadArtifactManifestEntries("bucket", 62, 1, t.TempDir(), nil)

	if result.Status != runner.UploadStatusFailed {
		t.Fatalf("Status = %q, want failed", result.Status)
	}
	if len(result.Dirs) != 1 || result.Dirs[0].Error == "" {
		t.Fatalf("Dirs = %+v, want one failed entry naming the manifest", result.Dirs)
	}
	if result.StartedAtUnix == 0 || result.CompletedAtUnix == 0 {
		t.Fatalf("result = %+v, want timing fields set on the failed result", result)
	}
}

func TestUploadArtifactManifestEntries_AbsentManifestIsOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	result := uploadArtifactManifestEntries("bucket", 63, 1, t.TempDir(), nil)

	if result.Status != runner.UploadStatusOK || len(result.Dirs) != 0 {
		t.Fatalf("result = %+v, want clean OK for a job that declared nothing", result)
	}
}

// Regression: a manifest the job touched but left empty declares nothing —
// that is the ErrManifestMissing sentinel, not an unreadable manifest.
func TestUploadArtifactManifestEntries_EmptyManifestIsOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeRawArtifactManifest(t, 64, "\n")

	result := uploadArtifactManifestEntries("bucket", 64, 1, t.TempDir(), nil)

	if result.Status != runner.UploadStatusOK || len(result.Dirs) != 0 {
		t.Fatalf("result = %+v, want clean OK for an empty manifest", result)
	}
}

type artifactUploadCall struct {
	src, dest, command, label string
	bytes                     int64
}

func captureArtifactUploads(t *testing.T) *[]artifactUploadCall {
	t.Helper()
	var calls []artifactUploadCall
	previous := uploadArtifactObject
	uploadArtifactObject = func(_ string, src, dest, command string, _, _ int64, label string, bytes int64) error {
		calls = append(calls, artifactUploadCall{src: src, dest: dest, command: command, label: label, bytes: bytes})
		return nil
	}
	t.Cleanup(func() { uploadArtifactObject = previous })
	return &calls
}

func TestUploadArtifactManifestEntries_ConventionOutputUsesCanonicalObject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeRcloneOK(t)
	workDir := t.TempDir()
	artifactPath := filepath.Join(workDir, "output", "model.pt")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The declared upload is intentionally not windowed by mtime. It must
	// publish even when the convention bulk walk would reject a stale file.
	stale := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(artifactPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.WriteManifestFile(runner.ExpandTilde(artifacts.RemoteManifestPath(65)), artifacts.Manifest{
		JobID: 65, Artifacts: []artifacts.ArtifactSpec{{Name: "model", Path: "./output/checkpoints/../model.pt"}},
	}); err != nil {
		t.Fatal(err)
	}
	calls := captureArtifactUploads(t)

	result := uploadArtifactManifestEntries("bucket", 65, 7, workDir, nil)

	if len(*calls) != 1 {
		t.Fatalf("upload calls = %+v, want one", *calls)
	}
	wantDest := r2keys.JobAttemptOutputsPrefix(65, 7) + "output/model.pt"
	if (*calls)[0].dest != wantDest || (*calls)[0].command != "copyto" {
		t.Fatalf("upload = %+v, want copyto destination %q", (*calls)[0], wantDest)
	}
	if result.Status != runner.UploadStatusOK || result.Bytes != int64(len("weights")) {
		t.Fatalf("result = %+v, want one successful payload", result)
	}
}

func TestUploadArtifactManifestEntries_CustomAndOutsidePathsKeepArtifactObjects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeRcloneOK(t)
	workDir := t.TempDir()
	customRoot := t.TempDir()
	absolutePath := filepath.Join(t.TempDir(), "absolute.pt")
	for _, p := range []string{filepath.Join(workDir, "checkpoint.pt"), absolutePath, filepath.Join(customRoot, "output", "custom.pt")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := captureArtifactUploads(t)

	if err := artifacts.WriteManifestFile(runner.ExpandTilde(artifacts.RemoteManifestPath(66)), artifacts.Manifest{
		JobID: 66, Artifacts: []artifacts.ArtifactSpec{{Path: "checkpoint.pt"}, {Path: absolutePath}},
	}); err != nil {
		t.Fatal(err)
	}
	uploadArtifactManifestEntries("bucket", 66, 3, workDir, nil)
	if err := artifacts.WriteManifestFile(runner.ExpandTilde(artifacts.RemoteManifestPath(67)), artifacts.Manifest{
		JobID: 67, ArtifactRoot: customRoot, Artifacts: []artifacts.ArtifactSpec{{Path: "output/custom.pt"}},
	}); err != nil {
		t.Fatal(err)
	}
	uploadArtifactManifestEntries("bucket", 67, 4, workDir, nil)

	wants := []string{
		r2keys.JobAttemptArtifactFilesPrefix(66, 3) + "checkpoint.pt",
		r2keys.JobAttemptArtifactFilesPrefix(66, 3) + "absolute.pt",
		r2keys.JobAttemptArtifactFilesPrefix(67, 4) + "output/custom.pt",
	}
	if len(*calls) != len(wants) {
		t.Fatalf("upload calls = %+v, want %d", *calls, len(wants))
	}
	for i, want := range wants {
		if (*calls)[i].dest != want {
			t.Errorf("call %d destination = %q, want %q", i, (*calls)[i].dest, want)
		}
	}
}

func TestUploadArtifactManifestEntries_DedupesLexicalAndAncestorOverlap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeRcloneOK(t)
	workDir := t.TempDir()
	dir := filepath.Join(workDir, "results", "bundle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.WriteManifestFile(runner.ExpandTilde(artifacts.RemoteManifestPath(68)), artifacts.Manifest{
		JobID: 68,
		Artifacts: []artifacts.ArtifactSpec{
			{Name: "child-first", Path: "results/bundle/a.bin"},
			{Name: "directory", Path: "./results/bundle"},
			{Name: "same-child", Path: "results/bundle/./a.bin"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	calls := captureArtifactUploads(t)

	result := uploadArtifactManifestEntries("bucket", 68, 9, workDir, []string{"results/"})

	if len(*calls) != 1 {
		t.Fatalf("upload calls = %+v, want one ancestor upload", *calls)
	}
	wantDest := r2keys.JobAttemptOutputsPrefix(68, 9) + "results/bundle/"
	if (*calls)[0].dest != wantDest || (*calls)[0].command != "copy" {
		t.Fatalf("upload = %+v, want directory copy to %q", (*calls)[0], wantDest)
	}
	if len(result.Dirs) != 3 {
		t.Fatalf("logical outcomes = %+v, want all three declarations", result.Dirs)
	}
	for _, outcome := range result.Dirs {
		if outcome.Status != runner.UploadStatusOK {
			t.Errorf("outcome = %+v, want ok", outcome)
		}
	}
	if result.FileCount != 1 || result.Bytes != int64(len("payload")) {
		t.Fatalf("result accounting = %d files, %d bytes; want one payload", result.FileCount, result.Bytes)
	}
}

// Regression: the post-job merge must carry per-entry artifact outcomes and
// re-derive the combined status, not just sum counters
// (spec: PerArtifactUploadOutcomeRecorded).
func TestMergeUploadResults_CarriesDirsAndCombinesStatus(t *testing.T) {
	dst := runner.OutputUploadResult{
		Status:          runner.UploadStatusOK,
		FileCount:       2,
		Bytes:           10,
		StartedAtUnix:   100,
		CompletedAtUnix: 110,
		Dirs:            []runner.OutputDirUpload{{Dir: "output", Status: runner.UploadStatusOK}},
	}
	src := runner.OutputUploadResult{
		Status:          runner.UploadStatusFailed,
		StartedAtUnix:   90,
		CompletedAtUnix: 120,
		Dirs:            []runner.OutputDirUpload{{Dir: "output/model.pt", Status: runner.UploadStatusFailed, Error: "stat"}},
	}

	mergeUploadResults(&dst, &src)

	if dst.Status != runner.UploadStatusPartial {
		t.Errorf("Status = %q, want partial", dst.Status)
	}
	if len(dst.Dirs) != 2 || dst.Dirs[1].Dir != "output/model.pt" {
		t.Errorf("Dirs = %+v, want both entries carried", dst.Dirs)
	}
	if dst.StartedAtUnix != 90 || dst.CompletedAtUnix != 120 {
		t.Errorf("timing = %d..%d, want 90..120", dst.StartedAtUnix, dst.CompletedAtUnix)
	}
}
