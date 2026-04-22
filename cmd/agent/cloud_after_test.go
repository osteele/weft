package main

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestCheckCloudAfter_NoRefs(t *testing.T) {
	skip, reason := checkCloudAfter(cloud.AgentJob{ID: 1}, []int64{2, 3})
	if skip {
		t.Fatalf("skip=%v reason=%q, want no skip", skip, reason)
	}
}

func TestCheckCloudAfter_ProducerNotFailed(t *testing.T) {
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5}}}
	skip, _ := checkCloudAfter(job, []int64{2, 3})
	if skip {
		t.Fatal("expected proceed; producer 5 not in failed list")
	}
}

func TestCheckCloudAfter_ProducerFailed(t *testing.T) {
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5}}}
	skip, reason := checkCloudAfter(job, []int64{5})
	if !skip {
		t.Fatal("expected skip when producer failed")
	}
	if !strings.Contains(reason, "cloud_after_failed") {
		t.Fatalf("reason=%q, expected it to mention cloud_after_failed", reason)
	}
}

func TestCheckCloudAfter_AllowFailureIgnored(t *testing.T) {
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 5, AllowFailure: true}}}
	skip, _ := checkCloudAfter(job, []int64{5})
	if skip {
		t.Fatal("expected proceed; AllowFailure should ignore producer failure")
	}
}

func TestCheckCloudAfter_UnknownProducerProceeds(t *testing.T) {
	// Producer not in failedJobs and not tracked: treat as satisfied (only
	// observed failures cause skip).
	job := cloud.AgentJob{ID: 10, CloudAfter: []cloud.CloudAfterRef{{JobID: 99}}}
	skip, _ := checkCloudAfter(job, nil)
	if skip {
		t.Fatal("expected proceed when nothing observed failed")
	}
}
