package orchestration

import (
	"strings"
	"testing"

	dbpkg "github.com/osteele/weft/internal/db"
)

func TestAutoPilotBlockSummaryWithCountIgnoresWaitingAndPausedReasons(t *testing.T) {
	reasons := map[int64]string{
		3342: `job wj3342 declares input "checkpoint:role-enc/exp036-klm3-s42", which weft cannot resolve`,
		3344: "autopilot placing jobs",
		3345: "placement pending",
		3346: "paused: repeated infrastructure failures without progress",
	}

	summary, count := AutoPilotBlockSummaryWithCount(reasons)

	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	want := `job wj3342 declares input "checkpoint:role-enc/exp036-klm3-s42", which weft cannot resolve`
	if summary != want {
		t.Fatalf("summary = %q, want %q", summary, want)
	}
}

func TestAutoPilotBlockSummaryWithCountReturnsEmptyForWaitOnlyReasons(t *testing.T) {
	reasons := map[int64]string{
		3344: "autopilot placing jobs",
		3345: "placement pending",
	}

	summary, count := AutoPilotBlockSummaryWithCount(reasons)

	if summary != "" || count != 0 {
		t.Fatalf("summary/count = %q/%d, want empty/0", summary, count)
	}
}

func TestFormatRunRateTargetExceededReasonSingleJobNamesRequest(t *testing.T) {
	reason := FormatRunRateTargetExceededReason(400, 69, 457, 526, 331, 457, []int64{3362})

	if strings.Contains(reason, "no subset fits") {
		t.Fatalf("single-job reason must not mention subsets: %q", reason)
	}
	if !strings.Contains(reason, "requested $4.57/hr") || !strings.Contains(reason, "job needs $4.57/hr") {
		t.Fatalf("reason = %q, want requested and job-needed amounts", reason)
	}
	if need, ok := ParseRunRateBlockedNeedCents(reason); !ok || need != 457 {
		t.Fatalf("ParseRunRateBlockedNeedCents(%q) = %d/%v, want 457/true", reason, need, ok)
	}
}

func TestFormatRunRateTargetExceededReasonMultiJobKeepsSubsetMarker(t *testing.T) {
	reason := FormatRunRateTargetExceededReason(400, 69, 457, 526, 331, 220, []int64{3362, 3363})

	if !strings.Contains(reason, "no subset fits") {
		t.Fatalf("multi-job reason = %q, want no-subset marker", reason)
	}
	if !strings.Contains(reason, "requested $4.57/hr") || !strings.Contains(reason, "cheapest group $2.20/hr") {
		t.Fatalf("reason = %q, want requested and cheapest-group amounts", reason)
	}
}

func TestCheckRunRateProjectionNamesRequestedAmount(t *testing.T) {
	database := dbpkg.SetupTestDB(t)

	reason, blocked := CheckRunRateProjection(database, 100, 150)

	if !blocked {
		t.Fatal("CheckRunRateProjection blocked = false, want true")
	}
	if !strings.Contains(reason, "requested $1.50/hr") {
		t.Fatalf("reason = %q, want requested amount", reason)
	}
	if strings.Contains(reason, "planned") {
		t.Fatalf("reason = %q, must not use planned wording", reason)
	}
}
