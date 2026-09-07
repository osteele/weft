package ops

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/opsqueue"
)

func TestCapabilityRemedyNamesTheCommand(t *testing.T) {
	want := "queue runner lacks job-payload-v1 capability; run `weft queue update host-alpha` to deploy a current agent"
	if got := capabilityRemedy("host-alpha", opsqueue.CapabilityJobPayloadV1); got != want {
		t.Fatalf("capabilityRemedy = %q, want %q", got, want)
	}
}

func TestTruncatedCapabilityRemedyRemainsRecognizable(t *testing.T) {
	detail := capabilityRemedy(strings.Repeat("h", 300), opsqueue.CapabilityJobPayloadV1)
	truncated := truncateDispatchDetail(detail)
	if truncated == detail || len(truncated) != 240 {
		t.Fatalf("truncateDispatchDetail produced %d bytes; want a truncated 240-byte detail", len(truncated))
	}
	if !opsqueue.IsMissingRunnerCapabilityBlock(truncated) {
		t.Fatalf("IsMissingRunnerCapabilityBlock(%q) = false after truncation", truncated)
	}
}
