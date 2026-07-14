package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

// withMockR2Get swaps graceR2Get for the duration of the test, restoring the
// original on cleanup.
func withMockR2Get(t *testing.T, get func(bucket, key string) (string, error)) {
	t.Helper()
	prev := graceR2Get
	graceR2Get = get
	t.Cleanup(func() { graceR2Get = prev })
}

func TestCheckCloudAfter_NoRefs(t *testing.T) {
	skip, reason := checkCloudAfter("bucket", cloud.AgentJob{ID: 1}, []int64{2, 3})
	if skip {
		t.Fatalf("skip=%v reason=%q, want no skip", skip, reason)
	}
}

func TestCheckCloudAfter_ProducerCompletedSuccessfully(t *testing.T) {
	withMockR2Get(t, func(bucket, key string) (string, error) {
		return "0", nil
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50}}}
	skip, _ := checkCloudAfter("bucket", job, nil)
	if skip {
		t.Fatal("expected proceed; producer completed with exit 0")
	}
}

func TestCheckCloudAfter_ProducerFailedInThisSession(t *testing.T) {
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50}}}
	skip, reason := checkCloudAfter("bucket", job, []int64{5})
	if !skip {
		t.Fatal("expected skip when producer failed")
	}
	if !strings.Contains(reason, "cloud_after_failed") {
		t.Fatalf("reason=%q, expected it to mention cloud_after_failed", reason)
	}
}

func TestCheckCloudAfter_ProducerFailedViaR2Marker(t *testing.T) {
	// Producer never observed to fail in *this* session (e.g. it ran and
	// failed in an earlier session before a grace-wake), but its R2
	// completion marker records a nonzero exit code.
	withMockR2Get(t, func(bucket, key string) (string, error) {
		return "1", nil
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50}}}
	skip, reason := checkCloudAfter("bucket", job, nil)
	if !skip {
		t.Fatal("expected skip when producer's completion marker records a failure")
	}
	if !strings.Contains(reason, "cloud_after_failed") {
		t.Fatalf("reason=%q, expected it to mention cloud_after_failed", reason)
	}
}

func TestCheckCloudAfter_ProducerNeverCompleted(t *testing.T) {
	// Regression for wj4663/wj4665: producer assigned to this same launch
	// but never dispatched (still queued in the DB) leaves no completion
	// marker in R2. It must not be treated as satisfied.
	withMockR2Get(t, func(bucket, key string) (string, error) {
		return "", nil // r2Get's documented "key doesn't exist" contract
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50}}}
	skip, reason := checkCloudAfter("bucket", job, nil)
	if !skip {
		t.Fatal("expected skip when producer has no completion marker")
	}
	if !strings.Contains(reason, "cloud_after_incomplete") {
		t.Fatalf("reason=%q, expected it to mention cloud_after_incomplete", reason)
	}
}

func TestCheckCloudAfter_ProducerCanceledMidBatch(t *testing.T) {
	// A producer canceled by the orchestrator mid-sequence (see
	// canceledAttempts in runJobSequence) also leaves no completion marker,
	// and must gate the consumer the same way a never-dispatched producer
	// does.
	withMockR2Get(t, func(bucket, key string) (string, error) {
		return "", nil
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50}}}
	skip, _ := checkCloudAfter("bucket", job, nil)
	if !skip {
		t.Fatal("expected skip for a canceled producer with no completion marker")
	}
}

func TestCheckCloudAfter_AllowFailureIgnoresFailure(t *testing.T) {
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50, AllowFailure: true}}}
	skip, _ := checkCloudAfter("bucket", job, []int64{5})
	if skip {
		t.Fatal("expected proceed; AllowFailure should ignore producer failure")
	}
}

func TestCheckCloudAfter_AllowFailureStillRequiresCompletion(t *testing.T) {
	// AllowFailure means "a failure is fine", not "an unknown state is
	// fine" -- an unmarked producer must still gate the consumer.
	withMockR2Get(t, func(bucket, key string) (string, error) {
		return "", nil
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50, AllowFailure: true}}}
	skip, reason := checkCloudAfter("bucket", job, nil)
	if !skip {
		t.Fatal("expected skip; AllowFailure does not exempt an incomplete producer")
	}
	if !strings.Contains(reason, "cloud_after_incomplete") {
		t.Fatalf("reason=%q, expected it to mention cloud_after_incomplete", reason)
	}
}

func TestCheckCloudAfter_R2ErrorFailsOpen(t *testing.T) {
	// A transient R2 error must not wedge the whole job sequence.
	withMockR2Get(t, func(bucket, key string) (string, error) {
		return "", fmt.Errorf("network unreachable")
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, RunID: 50}}}
	skip, _ := checkCloudAfter("bucket", job, nil)
	if skip {
		t.Fatal("expected proceed on R2 error; must fail open rather than wedge")
	}
}

func TestCheckCloudAfter_MultipleRefsAllMustBeSatisfied(t *testing.T) {
	withMockR2Get(t, func(bucket, key string) (string, error) {
		if strings.Contains(key, fmt.Sprintf("/%d/", 5)) {
			return "0", nil
		}
		return "", nil // producer 6 never completed
	})
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{
		{JobID: 5, RunID: 50},
		{JobID: 6, RunID: 60},
	}}
	skip, reason := checkCloudAfter("bucket", job, nil)
	if !skip {
		t.Fatal("expected skip; one of two producers is incomplete")
	}
	if !strings.Contains(reason, "producer job 6") {
		t.Fatalf("reason=%q, expected it to name producer job 6", reason)
	}
}
