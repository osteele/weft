package db

import (
	"testing"
	"time"
)

func TestLifecycleHookDeliveryIsLeasedAndFenced(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "test-host", t.TempDir(), "true", "hook test")
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := CloseAttempt(database, jobID, StatusCompleted, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	first, err := ClaimLifecycleHookDeliveries(database, "hook-a", jobID, 0, now, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("first claims = %d, want queued and terminal", len(first))
	}
	second, err := ClaimLifecycleHookDeliveries(database, "hook-a", 0, 0, now, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("concurrent claims = %d, want 0", len(second))
	}
	if won, err := FinishLifecycleHookDelivery(database, first[0].Event.EventID, "hook-a", "stale-token", "handled", "", now, 0); err != nil || won {
		t.Fatalf("stale finish = %v, %v", won, err)
	}
	for _, delivery := range first {
		if won, err := FinishLifecycleHookDelivery(database, delivery.Event.EventID, "hook-a", delivery.LeaseToken, "handled", "", now, 0); err != nil || !won {
			t.Fatalf("owner finish = %v, %v", won, err)
		}
	}
	again, err := ClaimLifecycleHookDeliveries(database, "hook-a", 0, 0, now.Add(time.Minute), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("redelivery after handled = %d, want 0", len(again))
	}
}

func TestLifecycleHookRetryCanBeReclaimedAfterBackoff(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "test-host", t.TempDir(), "true", "hook test")
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	if err := CloseAttempt(database, jobID, StatusFailed, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims, err := ClaimLifecycleHookDeliveries(database, "hook-a", jobID, 0, now, time.Minute, 10)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claim = %v, %v", claims, err)
	}
	if won, err := FinishLifecycleHookDelivery(database, claims[0].Event.EventID, "hook-a", claims[0].LeaseToken, "ignored", "", now, 0); err != nil || !won {
		t.Fatalf("finish nonterminal event = %v, %v", won, err)
	}
	if won, err := RecordLifecycleHookRetry(database, claims[1], "busy", now, 5*time.Second); err != nil || !won {
		t.Fatalf("retry = %v, %v", won, err)
	}
	before, err := ClaimLifecycleHookDeliveries(database, "hook-a", 0, 0, now.Add(4*time.Second), time.Minute, 10)
	if err != nil || len(before) != 0 {
		t.Fatalf("claim before backoff = %v, %v", before, err)
	}
	after, err := ClaimLifecycleHookDeliveries(database, "hook-a", 0, 0, now.Add(5*time.Second), time.Minute, 10)
	if err != nil || len(after) != 1 || after[0].Attempts != 2 {
		t.Fatalf("claim after backoff = %v, %v", after, err)
	}
}

func TestNonzeroExitCompletionEmitsOneFailedTerminalEvent(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "test-host", t.TempDir(), "false", "exit normalization")
	if err != nil {
		t.Fatal(err)
	}
	// The SSH sync completion path persists StatusCompleted with a nonzero
	// exit code in one row update; the job_status view reports failed. The
	// lifecycle event must match the view, and the notification-seam backfill
	// must not add a second terminal event for the same attempt.
	if _, err := RecordCompletionByIDWithTransition(database, jobID, 1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := EnsureJobTerminalEvent(database, jobID, StatusFailed, time.Now()); err != nil {
		t.Fatal(err)
	}
	var kinds, statuses []string
	rows, err := database.Query(`SELECT event_kind, status FROM job_lifecycle_events WHERE job_id = ? ORDER BY event_sequence`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, status string
		if err := rows.Scan(&kind, &status); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != "job.status_changed" || kinds[1] != "job.terminal" {
		t.Fatalf("event kinds = %v, want queued + one terminal", kinds)
	}
	if statuses[1] != StatusFailed {
		t.Fatalf("terminal event status = %q, want %q (exit-code normalization)", statuses[1], StatusFailed)
	}
}

func TestTerminalBackfillIsScopedToLatestAuthoritativeAttempt(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "test-host", t.TempDir(), "false", "retry history")
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	if err := CloseAttempt(database, jobID, StatusFailed, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// Retry: a second authoritative attempt exists before the terminal
	// notification seam runs its backfill.
	if _, err := CreateAttempt(database, jobID, "test-host", nil, StatusQueued); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if err := CloseAttempt(database, jobID, StatusCompleted, &zero, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// The backfill must select one attempt, not one row per attempt: an
	// unscoped insert reuses one event_id across rows and violates the
	// primary key, erroring before any notification is dispatched.
	if err := EnsureJobTerminalEvent(database, jobID, StatusCompleted, time.Now()); err != nil {
		t.Fatalf("EnsureJobTerminalEvent with retry history: %v", err)
	}
	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM job_lifecycle_events WHERE job_id = ? AND event_kind = 'job.terminal'`, jobID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("terminal events = %d, want one per terminal attempt", n)
	}
	var distinctAttempts int
	if err := database.QueryRow(
		`SELECT COUNT(DISTINCT attempt_id) FROM job_lifecycle_events WHERE job_id = ? AND event_kind = 'job.terminal'`, jobID,
	).Scan(&distinctAttempts); err != nil {
		t.Fatal(err)
	}
	if distinctAttempts != 2 {
		t.Fatalf("distinct terminal attempt ids = %d, want 2", distinctAttempts)
	}
}
