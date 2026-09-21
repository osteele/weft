package r2

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestListObjectsLimitedReportsWhetherPrefixWasExhausted(t *testing.T) {
	tests := []struct {
		name         string
		isTruncated  bool
		wantComplete bool
	}{
		{name: "complete", wantComplete: true},
		{name: "truncated", isTruncated: true, wantComplete: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := newTestS3Client(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				if got := r.URL.Query().Get("prefix"); got != "assets/a" {
					t.Errorf("prefix = %q, want assets/a", got)
				}
				if got := r.URL.Query().Get("max-keys"); got != "2" {
					t.Errorf("max-keys = %q, want 2", got)
				}
				truncated := "false"
				nextToken := ""
				if tc.isTruncated {
					truncated = "true"
					nextToken = "<NextContinuationToken>next</NextContinuationToken>"
				}
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>test-bucket</Name><Prefix>assets/a</Prefix><KeyCount>2</KeyCount><MaxKeys>2</MaxKeys>
  <IsTruncated>%s</IsTruncated>%s
  <Contents><Key>assets/a1</Key><Size>1</Size><ETag>"one"</ETag></Contents>
  <Contents><Key>assets/a2</Key><Size>2</Size><ETag>"two"</ETag></Contents>
</ListBucketResult>`, truncated, nextToken)
			}))

			objects, complete, err := client.ListObjectsLimited(context.Background(), "assets/a", 2)
			if err != nil {
				t.Fatalf("ListObjectsLimited: %v", err)
			}
			if complete != tc.wantComplete {
				t.Fatalf("complete = %t, want %t", complete, tc.wantComplete)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
			if len(objects) != 2 || objects[0].Key != "assets/a1" || objects[1].Key != "assets/a2" {
				t.Fatalf("objects = %+v", objects)
			}
		})
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

// multipartFake emulates the S3 multipart API over httptest and records how
// PutObject routed an upload.
type multipartFake struct {
	creates          int
	singlePuts       int
	completes        int
	aborts           int
	createKey        string
	createType       string
	partTries        map[int]int
	partBodies       map[int][]byte
	completeOrder    []int
	failFirstTryPart int
	// onUploadPartStart runs at UploadPart handler entry, onUploadPartBody
	// after the part body has been consumed; comparing the caller-reader
	// counts between the two proves the source is read during the transfer
	// rather than up front. Load/store only: handler goroutines call them.
	onUploadPartStart atomic.Pointer[func(pn int)]
	onUploadPartBody  atomic.Pointer[func(pn int)]
}

func (f *multipartFake) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			f.creates++
			f.createKey = strings.TrimPrefix(r.URL.Path, "/test-bucket/")
			f.createType = r.Header.Get("Content-Type")
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<CreateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Bucket>test-bucket</Bucket><Key>k</Key><UploadId>upload-1</UploadId>
</CreateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			pn, err := strconv.Atoi(q.Get("partNumber"))
			if err != nil {
				t.Errorf("partNumber = %q: %v", q.Get("partNumber"), err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.partTries[pn]++
			if hook := f.onUploadPartStart.Load(); hook != nil {
				(*hook)(pn)
			}
			if f.failFirstTryPart == pn && f.partTries[pn] == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `<Error><Code>InternalError</Code><Message>boom</Message></Error>`)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read part %d: %v", pn, err)
			}
			if hook := f.onUploadPartBody.Load(); hook != nil {
				(*hook)(pn)
			}
			f.partBodies[pn] = body
			w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", pn))
		case r.Method == http.MethodPost && q.Has("uploadId"):
			f.completes++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read complete body: %v", err)
			}
			var complete struct {
				Parts []struct {
					PartNumber int `xml:"PartNumber"`
				} `xml:"Part"`
			}
			if err := xml.Unmarshal(body, &complete); err != nil {
				t.Errorf("parse complete body: %v", err)
			}
			for _, p := range complete.Parts {
				f.completeOrder = append(f.completeOrder, p.PartNumber)
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Bucket>test-bucket</Bucket><Key>k</Key><ETag>"final"</ETag>
</CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete && q.Has("uploadId"):
			f.aborts++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut:
			f.singlePuts++
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read single-put body: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
}

func newMultipartTestClient(t *testing.T, fake *multipartFake) *Client {
	t.Helper()
	fake.partTries = map[int]int{}
	fake.partBodies = map[int][]byte{}
	client := newTestS3Client(t, fake.handler(t))
	client.multipartThresholdBytes = 64
	client.multipartPartSizeBytes = 16
	return client
}

func multipartPayload(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i)
	}
	return data
}

func (f *multipartFake) reassembled(t *testing.T) []byte {
	t.Helper()
	var out []byte
	for _, pn := range f.completeOrder {
		body, ok := f.partBodies[pn]
		if !ok {
			t.Fatalf("complete listed part %d but no part body was uploaded", pn)
		}
		out = append(out, body...)
	}
	return out
}

func TestPutObject_MultipartThresholdRouting(t *testing.T) {
	t.Run("below threshold uses single put", func(t *testing.T) {
		fake := &multipartFake{}
		client := newMultipartTestClient(t, fake)

		err := client.PutObject(context.Background(), "assets/small", strings.NewReader("0123456789"), "text/plain")
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		if fake.singlePuts != 1 || fake.creates != 0 || fake.completes != 0 {
			t.Fatalf("singlePuts=%d creates=%d completes=%d, want 1/0/0", fake.singlePuts, fake.creates, fake.completes)
		}
	})

	t.Run("above threshold uses multipart", func(t *testing.T) {
		fake := &multipartFake{}
		client := newMultipartTestClient(t, fake)
		payload := multipartPayload(100)

		err := client.PutObject(context.Background(), "agents/v1/linux-amd64", bytes.NewReader(payload), "application/octet-stream")
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		if fake.singlePuts != 0 || fake.creates != 1 || fake.completes != 1 {
			t.Fatalf("singlePuts=%d creates=%d completes=%d, want 0/1/1", fake.singlePuts, fake.creates, fake.completes)
		}
		if fake.createKey != "agents/v1/linux-amd64" {
			t.Fatalf("create key = %q", fake.createKey)
		}
		if fake.createType != "application/octet-stream" {
			t.Fatalf("create content type = %q", fake.createType)
		}
		if !bytes.Equal(fake.reassembled(t), payload) {
			t.Fatalf("reassembled parts do not match the original payload")
		}
	})

	t.Run("unknown-size reader above threshold uses multipart", func(t *testing.T) {
		fake := &multipartFake{}
		client := newMultipartTestClient(t, fake)
		payload := multipartPayload(100)
		body := struct{ io.Reader }{bytes.NewReader(payload)}

		err := client.PutObject(context.Background(), "assets/stream", body, "")
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		if fake.singlePuts != 0 || fake.creates != 1 || fake.completes != 1 {
			t.Fatalf("singlePuts=%d creates=%d completes=%d, want 0/1/1", fake.singlePuts, fake.creates, fake.completes)
		}
		if !bytes.Equal(fake.reassembled(t), payload) {
			t.Fatalf("reassembled parts do not match the original payload")
		}
	})
}

func TestPutObject_MultipartRetriesFailedPart(t *testing.T) {
	fake := &multipartFake{failFirstTryPart: 2}
	client := newMultipartTestClient(t, fake)
	payload := multipartPayload(80) // parts: 16 x 4, 8

	err := client.PutObject(context.Background(), "agents/v1/linux-amd64", bytes.NewReader(payload), "application/octet-stream")
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if fake.completes != 1 {
		t.Fatalf("completes = %d, want 1", fake.completes)
	}
	if fake.partTries[2] < 2 {
		t.Fatalf("part 2 attempts = %d, want at least 2", fake.partTries[2])
	}
	if !bytes.Equal(fake.reassembled(t), payload) {
		t.Fatalf("reassembled parts do not match the original payload")
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

// TestPutObject_MultipartFiresPartProgressCallback pins the callback
// contract the sync upload stall watchdog depends on: onPart fires once per
// part with the part's byte count, in order, regardless of how the SDK
// buffers or re-reads the body.
func TestPutObject_MultipartFiresPartProgressCallback(t *testing.T) {
	const bodySize = 100
	fake := &multipartFake{}
	client := newMultipartTestClient(t, fake)

	var mu sync.Mutex
	var reported []int64
	body := bytes.NewReader(make([]byte, bodySize))

	if err := client.PutObjectWithPartProgress(context.Background(), "streamed/key", body, "application/octet-stream", func(n int64) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, n)
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int64{16, 16, 16, 16, 16, 16, 4}
	if !reflect.DeepEqual(reported, want) {
		t.Fatalf("onPart = %v, want %v", reported, want)
	}
	if got := fake.reassembled(t); !bytes.Equal(got, make([]byte, bodySize)) {
		t.Fatalf("uploaded payload corrupted: got %d bytes", len(got))
	}
}

type countingReadSeeker struct {
	r     *bytes.Reader
	reads atomic.Int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.reads.Add(int64(n))
	return n, err
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}
