package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// The "Job:" footer line is rendered on a single visual row; any embedded
// newline in job.FailureReason (e.g. from a legacy/buggy agent that stuffed
// the multi-line prewarm log tail into failure_reason) would push the rest
// of the footer onto subsequent lines and scroll the top of the TUI off
// screen. appendJobStatusParts must collapse FailureReason via firstLine.
func TestAppendJobStatusParts_FailedJob_StripsMultilineReason(t *testing.T) {
	exit := 1
	job := &db.Job{
		ID:            1,
		Status:        db.StatusFailed,
		ExitCode:      &exit,
		FailureReason: "hf prewarm failed exit 124: setup command timed out after 1h0m0s\nprewarm log tail:\nlots of multi-line\nshell script content here\n",
	}
	parts := appendJobStatusParts(nil, job, selectedJobContext{}, time.Now())
	joined := strings.Join(parts, " · ")
	if strings.Contains(joined, "\n") {
		t.Errorf("footer line must be single-line; got embedded newline in: %q", joined)
	}
	if !strings.Contains(joined, "hf prewarm failed") {
		t.Errorf("first line of reason should still appear; got: %q", joined)
	}
	if strings.Contains(joined, "prewarm log tail") {
		t.Errorf("log-tail content must not leak into the footer; got: %q", joined)
	}
}
