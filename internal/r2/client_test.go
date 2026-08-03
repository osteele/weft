package r2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

func jobStartedKey(jobID int64) string {
	return "jobs/" + itoa(jobID) + "/.started"
}

func jobCompleteKey(jobID int64) string {
	return "jobs/" + itoa(jobID) + "/.complete"
}

func jobAttemptStartedKey(jobID, runID int64) string {
	return "jobs/" + itoa(jobID) + "/runs/" + itoa(runID) + "/.started"
}

func jobAttemptCompleteKey(jobID, runID int64) string {
	return "jobs/" + itoa(jobID) + "/runs/" + itoa(runID) + "/.complete"
}

func jobProcessedKey(jobID int64) string {
	return "jobs/" + itoa(jobID) + "/.processed"
}

func jobAttemptProcessedKey(jobID, runID int64) string {
	return "jobs/" + itoa(jobID) + "/runs/" + itoa(runID) + "/.processed"
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}

func TestJobMarkers_HasStartedMarkerMatchesExactCurrentRunKey(t *testing.T) {
	jobID := int64(222)
	currentRunID := int64(901)
	staleRunID := int64(700)

	markers := &JobMarkers{
		startedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptStartedKey(jobID, staleRunID):   {},
				jobAttemptStartedKey(jobID, currentRunID): {},
			},
		},
	}

	if !markers.HasStartedMarker(jobID, jobAttemptStartedKey(jobID, currentRunID)) {
		t.Fatalf("expected current run marker to match")
	}
	if markers.HasStartedMarker(jobID, jobAttemptStartedKey(jobID, currentRunID+1)) {
		t.Fatalf("unexpected match for missing run marker")
	}
	if markers.HasStartedMarker(jobID, jobStartedKey(jobID)) {
		t.Fatalf("unexpected match for legacy top-level marker")
	}
}

func TestJobMarkers_HasCompletedMarkerMatchesExactCurrentRunKey(t *testing.T) {
	jobID := int64(223)
	currentRunID := int64(902)
	staleRunID := int64(701)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, staleRunID):   {},
				jobAttemptCompleteKey(jobID, currentRunID): {},
			},
		},
	}

	if !markers.HasCompletedMarker(jobID, jobAttemptCompleteKey(jobID, currentRunID)) {
		t.Fatalf("expected current run completion marker to match")
	}
	if markers.HasCompletedMarker(jobID, jobAttemptCompleteKey(jobID, currentRunID+1)) {
		t.Fatalf("unexpected match for missing completion marker")
	}
	if markers.HasCompletedMarker(jobID, jobCompleteKey(jobID)) {
		t.Fatalf("unexpected match for legacy top-level completion marker")
	}
}

// Regression: an older attempt's .processed marker must not suppress
// reconciliation of a newer attempt's .complete marker on the same job.
//
// Pre-fix, the outer sync loop gated on a per-job IsProcessed check, so once
// any attempt of a job had been processed, every subsequent attempt's
// completion was silently skipped. This pinned re-queued cloud jobs in the
// "running" state for as long as the legacy job-scoped marker lived in R2.
func TestJobMarkers_HasUnprocessedComplete_NewAttemptAfterProcessedOlderAttempt(t *testing.T) {
	jobID := int64(2275)
	oldRunID := int64(17)
	newRunID := int64(19)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, oldRunID): {},
				jobAttemptCompleteKey(jobID, newRunID): {},
			},
		},
		processedKeys: map[int64]map[string]struct{}{
			jobID: {
				// Old attempt is processed; new attempt is not.
				jobAttemptProcessedKey(jobID, oldRunID): {},
				// Pre-fix marker: a job-scoped .processed key written by the
				// legacy ShouldMarkCloudJobProcessed path. Today's gate must
				// not treat this as suppressing the new attempt.
				jobProcessedKey(jobID): {},
			},
		},
	}

	if !markers.HasUnprocessedComplete(jobID) {
		t.Fatal("expected unprocessed completion for new attempt to be reported")
	}
}

func TestJobMarkers_HasUnprocessedComplete_AllAttemptsProcessed(t *testing.T) {
	jobID := int64(2280)
	runID := int64(3)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, runID): {},
			},
		},
		processedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptProcessedKey(jobID, runID): {},
			},
		},
	}

	if markers.HasUnprocessedComplete(jobID) {
		t.Fatal("expected fully-processed job to be reported as done")
	}
}

// Inventory jobs use job-scoped marker keys (runID=0 collapses to
// jobs/X/.complete and jobs/X/.processed via the key constructor fallback).
// The per-attempt gate must still apply correctly when both endpoints use
// the job-scoped form.
func TestJobMarkers_HasUnprocessedComplete_InventoryFormPair(t *testing.T) {
	jobID := int64(2290)

	t.Run("inventory complete without processed is unprocessed", func(t *testing.T) {
		markers := &JobMarkers{
			completedKeys: map[int64]map[string]struct{}{
				jobID: {jobCompleteKey(jobID): {}},
			},
		}
		if !markers.HasUnprocessedComplete(jobID) {
			t.Fatal("inventory .complete without .processed should be unprocessed")
		}
	})

	t.Run("inventory complete paired with job-scoped processed is done", func(t *testing.T) {
		markers := &JobMarkers{
			completedKeys: map[int64]map[string]struct{}{
				jobID: {jobCompleteKey(jobID): {}},
			},
			processedKeys: map[int64]map[string]struct{}{
				jobID: {jobProcessedKey(jobID): {}},
			},
		}
		if markers.HasUnprocessedComplete(jobID) {
			t.Fatal("inventory .complete paired with .processed should be done")
		}
	})
}

// Regression: when only the orphan (older) run's .complete is unpaired and
// the latest run's .complete is already processed, HasUnprocessedComplete
// still reports the job as needing work. Without the orphan-suppression
// write at the end of the sync handler, this would loop forever as the
// handler processes latest_run_id but never marks the orphan's .processed.
func TestJobMarkers_HasUnprocessedComplete_OrphanOlderRunOnly(t *testing.T) {
	jobID := int64(2295)
	orphanRunID := int64(5)
	latestRunID := int64(20)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, orphanRunID): {},
				jobAttemptCompleteKey(jobID, latestRunID): {},
			},
		},
		processedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptProcessedKey(jobID, latestRunID): {},
			},
		},
	}

	if !markers.HasUnprocessedComplete(jobID) {
		t.Fatal("orphan unpaired .complete should be reported as unprocessed")
	}

	// And once the sync handler suppresses the orphan, the gate flips.
	markers.processedKeys[jobID][jobAttemptProcessedKey(jobID, orphanRunID)] = struct{}{}
	if markers.HasUnprocessedComplete(jobID) {
		t.Fatal("after orphan suppression, job should be reported as done")
	}
}

func TestPairedProcessedKey(t *testing.T) {
	cases := []struct {
		name, completeKey, want string
	}{
		{"per-attempt", "jobs/123/runs/4/.complete", "jobs/123/runs/4/.processed"},
		{"job-scoped (inventory)", "jobs/123/.complete", "jobs/123/.processed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PairedProcessedKey(tc.completeKey); got != tc.want {
				t.Fatalf("PairedProcessedKey(%q) = %q, want %q", tc.completeKey, got, tc.want)
			}
		})
	}
}

func TestJobMarkers_AnyCompletedKeyReturnsSomeKey(t *testing.T) {
	jobID := int64(441)
	runID := int64(555)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, runID): {},
			},
		},
	}

	key, ok := markers.AnyCompletedKey(jobID)
	if !ok {
		t.Fatal("expected AnyCompletedKey to find a key")
	}
	if key != jobAttemptCompleteKey(jobID, runID) {
		t.Fatalf("AnyCompletedKey = %q, want %q", key, jobAttemptCompleteKey(jobID, runID))
	}

	_, ok = markers.AnyCompletedKey(999)
	if ok {
		t.Fatal("expected AnyCompletedKey to return false for unknown job")
	}
}

type delayedChunkReader struct {
	ctx    context.Context
	chunks [][]byte
	delay  time.Duration
	index  int
}

func (r *delayedChunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(r.delay):
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

type stallingReader struct {
	ctx   context.Context
	wrote bool
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
		p[0] = 'x'
		return 1, nil
	}
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(5 * time.Second):
		return 0, io.EOF
	}
}

func TestCopyWithIdleTimeout_AllowsSlowProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Two properties have to hold at once, and they pull in opposite
	// directions. To prove progress RESETS the timer, the transfer must
	// outlast the timeout (otherwise the copy finishes before the timer would
	// fire and the test passes even with the reset removed). To survive a
	// loaded machine, each per-chunk gap needs a wide margin under the
	// timeout. So: many small chunks, total well past the timeout, each gap a
	// small fraction of it.
	//
	// This is inherently wall-clock bound — copyWithIdleTimeout builds its own
	// time.Timer, so removing real time would take a timer seam in production
	// that exists only for the test. The margin here (100x per gap) is the
	// cheaper answer.
	const (
		chunkDelay  = 5 * time.Millisecond
		idleTimeout = 500 * time.Millisecond
		chunkCount  = 120 // 600ms total > 500ms timeout: the reset is load-bearing
	)
	chunks := make([][]byte, chunkCount)
	for i := range chunks {
		chunks[i] = []byte("a")
	}
	src := &delayedChunkReader{ctx: ctx, chunks: chunks, delay: chunkDelay}
	var out bytes.Buffer
	n, err := copyWithIdleTimeout(ctx, cancel, src, &out, idleTimeout)
	if err != nil {
		t.Fatalf("copyWithIdleTimeout returned error: %v", err)
	}
	if n != chunkCount {
		t.Fatalf("copied bytes = %d, want %d", n, chunkCount)
	}
	if out.String() != strings.Repeat("a", chunkCount) {
		t.Fatalf("output = %q, want %d a's", out.String(), chunkCount)
	}
}

func TestCopyWithIdleTimeout_FailsWhenStalled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &stallingReader{ctx: ctx}
	var out bytes.Buffer
	_, err := copyWithIdleTimeout(ctx, cancel, src, &out, 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected stall error, got nil")
	}
	if !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "no progress")
	}
	if out.String() != "x" {
		t.Fatalf("output = %q, want %q", out.String(), "x")
	}
}

func TestDownloadObjectToFileWithIdleTimeout_WritesFullContent(t *testing.T) {
	const key = "results/wj1/output.txt"
	oldContent := []byte("existing artifact")
	content := []byte("complete artifact contents")
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	client := newTestS3Client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/test-bucket/"+key {
			t.Errorf("path = %q, want %q", r.URL.Path, "/test-bucket/"+key)
		}
		close(requestStarted)
		<-releaseResponse
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if _, err := w.Write(content); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	localPath := filepath.Join(t.TempDir(), "nested", "output.txt")
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatalf("create destination dir: %v", err)
	}
	if err := os.WriteFile(localPath, oldContent, 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	type downloadResult struct {
		n   int64
		err error
	}
	resultCh := make(chan downloadResult, 1)
	go func() {
		n, err := client.DownloadObjectToFileWithIdleTimeout(context.Background(), key, localPath, time.Second)
		resultCh <- downloadResult{n: n, err: err}
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test S3 request")
	}
	gotBeforeRelease, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read destination during download: %v", err)
	}
	close(releaseResponse)

	var result downloadResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for download")
	}

	if !bytes.Equal(gotBeforeRelease, oldContent) {
		t.Fatalf("destination during download = %q, want %q", gotBeforeRelease, oldContent)
	}
	if result.err != nil {
		t.Fatalf("DownloadObjectToFileWithIdleTimeout returned error: %v", result.err)
	}
	if result.n != int64(len(content)) {
		t.Fatalf("downloaded bytes = %d, want %d", result.n, len(content))
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("local file = %q, want %q", got, content)
	}
}

func TestDownloadObjectToFileWithIdleTimeout_MidDownloadFailureLeavesExistingFile(t *testing.T) {
	const key = "results/wj1/output.txt"
	oldContent := []byte("known good artifact")
	fullContent := []byte("replacement artifact that never fully arrives")
	partialContent := fullContent[:12]
	client := newTestS3Client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/test-bucket/"+key {
			t.Errorf("path = %q, want %q", r.URL.Path, "/test-bucket/"+key)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(fullContent)))
		if _, err := w.Write(partialContent); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	dir := t.TempDir()
	localPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(localPath, oldContent, 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	_, err := client.DownloadObjectToFileWithIdleTimeout(context.Background(), key, localPath, time.Second)
	if err == nil {
		t.Fatal("expected mid-download failure, got nil")
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if !bytes.Equal(got, oldContent) {
		t.Fatalf("existing file = %q, want %q", got, oldContent)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read destination dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(localPath) {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("destination dir entries = %v, want only %q", names, filepath.Base(localPath))
	}
}

func newTestS3Client(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s3Client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("access", "secret", ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.UsePathStyle = true
	})
	return &Client{s3: s3Client, bucket: "test-bucket", endpoint: server.URL}
}

func TestIsPreconditionFailed(t *testing.T) {
	err := &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "condition failed"}
	if !IsPreconditionFailed(err) {
		t.Fatalf("IsPreconditionFailed = false, want true")
	}
	if !IsPreconditionFailed(ErrPreconditionFailed) {
		t.Fatalf("IsPreconditionFailed sentinel = false, want true")
	}
	if IsPreconditionFailed(fmt.Errorf("other")) {
		t.Fatalf("IsPreconditionFailed unrelated = true, want false")
	}
}

// Listing-derived rel paths must not escape the destination directory.
func TestRelPathForKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
		ok   bool
	}{
		{"results/wj1/out.json", "out.json", true},
		{"results/wj1/sub/out.json", "sub/out.json", true},
		{"results/wj1/", "", false},
		{"results/wj1/../other/secret", "", false},
		{"results/wj1//etc/passwd", "", false},
		{"results/wj1/sub/../../escape", "", false},
	} {
		got, ok := relPathForKey(tc.key, "results/wj1")
		if ok != tc.ok || got != tc.want {
			t.Errorf("relPathForKey(%q) = (%q, %v), want (%q, %v)", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}
