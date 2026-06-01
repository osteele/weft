package r2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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
	src := &delayedChunkReader{
		ctx:    ctx,
		chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c")},
		delay:  25 * time.Millisecond,
	}
	var out bytes.Buffer
	n, err := copyWithIdleTimeout(ctx, cancel, src, &out, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("copyWithIdleTimeout returned error: %v", err)
	}
	if n != 3 {
		t.Fatalf("copied bytes = %d, want 3", n)
	}
	if out.String() != "abc" {
		t.Fatalf("output = %q, want %q", out.String(), "abc")
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
