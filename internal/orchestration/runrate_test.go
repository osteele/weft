package orchestration

import "testing"

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
