package main

import (
	"testing"
	"time"
)

// TestExtendGraceDeadlineForJobsShiftsByElapsed is the regression test for
// the wj2297 incident (2026-05-31). The agent had been computing
// `deadline := time.Now().Add(cfg.Timeout)` once at grace-wait startup and
// then running resubmitted jobs against that same clock. A 21-minute
// resubmitted job inside a 15-minute grace window caused the agent to
// self-destruct the instant the job finished, even though the grace clock
// should only count idle wait time between failures. On the coordinator
// side this produced `grace_deadline < grace_started_at` in the launches
// table (deadline frozen from the original R2 payload; grace_started_at
// refreshed by the next reconcile pass).
func TestExtendGraceDeadlineForJobsShiftsByElapsed(t *testing.T) {
	jobStart := time.Date(2026, 5, 31, 23, 47, 0, 0, time.UTC)
	deadline := jobStart.Add(15 * time.Minute) // 00:02 — same as wj2297 incident
	jobEnd := jobStart.Add(21 * time.Minute)   // wj2297 ran ~21min before failing

	got := extendGraceDeadlineForJobs(deadline, jobStart, jobEnd)
	want := deadline.Add(21 * time.Minute) // shifted forward, still 15min ahead of jobEnd

	if !got.Equal(want) {
		t.Fatalf("deadline = %s, want %s (15min grace remaining after %s job)",
			got.Format(time.RFC3339), want.Format(time.RFC3339), jobEnd.Sub(jobStart))
	}

	// The post-shift deadline must be strictly after jobEnd — otherwise the
	// agent will self-destruct on the next ticker tick.
	if !got.After(jobEnd) {
		t.Fatalf("deadline %s is not after jobEnd %s — agent would self-destruct immediately",
			got.Format(time.RFC3339), jobEnd.Format(time.RFC3339))
	}
}

func TestExtendGraceDeadlineForJobsZeroDuration(t *testing.T) {
	now := time.Now()
	deadline := now.Add(15 * time.Minute)

	got := extendGraceDeadlineForJobs(deadline, now, now)
	if !got.Equal(deadline) {
		t.Fatalf("zero-duration run shifted deadline: got %s, want %s",
			got.Format(time.RFC3339), deadline.Format(time.RFC3339))
	}
}

func TestExtendGraceDeadlineForJobsClockSkew(t *testing.T) {
	// jobEnd before jobStart (negative duration from clock skew); helper
	// should not shift backwards, which would expire grace artificially.
	jobStart := time.Now()
	jobEnd := jobStart.Add(-5 * time.Second)
	deadline := jobStart.Add(15 * time.Minute)

	got := extendGraceDeadlineForJobs(deadline, jobStart, jobEnd)
	if !got.Equal(deadline) {
		t.Fatalf("negative elapsed shifted deadline: got %s, want %s",
			got.Format(time.RFC3339), deadline.Format(time.RFC3339))
	}
}

func TestExtendGraceDeadlineForJobsPreservesUserExtensions(t *testing.T) {
	// If the user extended grace before the job ran (e.g. `weft instance
	// extend <id> 30m`), the full pre-job idle window (base + extension)
	// should still be available after the job finishes. Elapsed job time
	// does not consume the wait clock.
	jobStart := time.Date(2026, 5, 31, 23, 47, 0, 0, time.UTC)
	deadline := jobStart.Add(15*time.Minute + 30*time.Minute) // 15m base + 30m extension
	jobEnd := jobStart.Add(10 * time.Minute)

	got := extendGraceDeadlineForJobs(deadline, jobStart, jobEnd)
	idleRemaining := got.Sub(jobEnd)
	want := 45 * time.Minute // full pre-job idle window preserved

	if idleRemaining != want {
		t.Fatalf("idle time remaining = %s, want %s", idleRemaining, want)
	}
}
