package ops

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	srcsync "github.com/osteele/weft/internal/sync"
)

// recordWedgeStreak records n source-sync timeouts for host to drive the streak
// past a desired count without spawning real rsyncs.
func recordWedgeStreak(t *testing.T, database *sql.DB, host string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		recordSourceSyncTimeout(database, host)
	}
}

func TestHostSourceSyncStreakAndBackoff(t *testing.T) {
	database := db.SetupTestDB(t)
	host := "wedge-host"

	if d := sourceSyncBackoffRemaining(database, host, time.Now()); d != 0 {
		t.Fatalf("fresh host backoff = %s, want 0", d)
	}

	recordSourceSyncTimeout(database, host)
	count, _, err := db.HostSourceSyncTimeoutStreak(database, host)
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if count != 1 {
		t.Fatalf("streak after one timeout = %d, want 1", count)
	}
	if d := sourceSyncBackoffRemaining(database, host, time.Now()); d <= 0 {
		t.Fatalf("backoff after timeout = %s, want > 0", d)
	}

	// A clean sync resets the streak and clears the backoff.
	recordSourceSyncOK(database, host, slog.Default())
	count, _, err = db.HostSourceSyncTimeoutStreak(database, host)
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if count != 0 {
		t.Fatalf("streak after ok = %d, want 0", count)
	}
	if d := sourceSyncBackoffRemaining(database, host, time.Now()); d != 0 {
		t.Fatalf("backoff after ok = %s, want 0", d)
	}
}

// TestEnsureQueuedJobsOnRemote_TimeoutCountsAsWedge verifies the dispatch path
// classifies a SIGKILL'd (timeout) source sync as a wedge — extending the host
// streak — while a plain rsync error does not.
func TestEnsureQueuedJobsOnRemote_TimeoutCountsAsWedge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		syncErr   error
		wantCount int
	}{
		{"timeout is a wedge", fmt.Errorf("rsync to h:d timed out: %w", srcsync.ErrSourceSyncTimeout), 1},
		{"plain error is transient", fmt.Errorf("rsync protocol error"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := db.SetupTestDB(t)
			if _, err := db.RecordQueued(database, "wedge-host", "/tmp", "echo hi", "j"); err != nil {
				t.Fatalf("record queued: %v", err)
			}
			t.Cleanup(srcsync.SetSyncFunc(func(_, _, _ string, _ []string) error {
				return tc.syncErr
			}))
			mockSSHFunc(t, func(_, command string) (string, string, int) {
				if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
					return "__WEFT_NO_STATE_FILE__\n", "", 0
				}
				return "", "", 0
			})

			_, _, _ = ensureQueuedJobsOnRemote(database, "wedge-host", 5*time.Second, 5*time.Second, slog.Default())

			count, _, err := db.HostSourceSyncTimeoutStreak(database, "wedge-host")
			if err != nil {
				t.Fatalf("streak: %v", err)
			}
			if count != tc.wantCount {
				t.Fatalf("streak = %d, want %d", count, tc.wantCount)
			}
		})
	}
}

// TestEnsureQueuedJobsOnRemote_BacksOffWhileWedged verifies that once a host is
// in source-sync backoff, the dispatch path skips the rsync entirely instead of
// hammering the wedged host every tick.
func TestEnsureQueuedJobsOnRemote_BacksOffWhileWedged(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.RecordQueued(database, "wedge-host", "/tmp", "echo hi", "j"); err != nil {
		t.Fatalf("record queued: %v", err)
	}
	// Seed a fresh timeout so the host is within its backoff window.
	recordSourceSyncTimeout(database, "wedge-host")

	syncCalls := 0
	t.Cleanup(srcsync.SetSyncFunc(func(_, _, _ string, _ []string) error {
		syncCalls++
		return nil
	}))
	mockSSHFunc(t, func(_, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		}
		return "", "", 0
	})

	_, _, err := ensureQueuedJobsOnRemote(database, "wedge-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if syncCalls != 0 {
		t.Fatalf("source sync attempted %d time(s) during backoff, want 0", syncCalls)
	}
}

// TestMaybeHandleSourceSyncWedge_CordonsAndReplaces verifies that crossing the
// wedge threshold auto-cordons the host and unplaces its queued inventory jobs.
func TestMaybeHandleSourceSyncWedge_CordonsAndReplaces(t *testing.T) {
	database := db.SetupTestDB(t)
	host := "wedge-host"
	jobID, err := db.RecordQueued(database, host, "/tmp", "echo hi", "wedged job")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	mockSSHFunc(t, func(_, _ string) (string, string, int) { return "", "", 0 })

	recordWedgeStreak(t, database, host, sourceSyncWedgeThreshold)
	maybeHandleSourceSyncWedge(database, host, slog.Default())

	cordoned, reason, err := db.IsInventoryExecutionTargetCordoned(database, host)
	if err != nil {
		t.Fatalf("cordon lookup: %v", err)
	}
	if !cordoned {
		t.Fatal("expected host auto-cordoned after wedge threshold")
	}
	if !strings.HasPrefix(reason, sourceSyncAutoCordonReason) {
		t.Fatalf("cordon reason = %q, want auto-cordon prefix", reason)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.HasInventoryHost() {
		t.Fatalf("expected job unplaced (off %s), still on host %q", host, job.Host)
	}
}

// TestMaybeHandleSourceSyncWedge_DrainsManualCordonButKeepsReason verifies that
// a job stranded on a manually-cordoned host that has wedged still drains (a
// plain cordon never moves already-queued work), while the operator's cordon
// reason is preserved.
func TestMaybeHandleSourceSyncWedge_DrainsManualCordonButKeepsReason(t *testing.T) {
	database := db.SetupTestDB(t)
	host := "wedge-host"
	jobID, err := db.RecordQueued(database, host, "/tmp", "echo hi", "manual job")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := db.SetInventoryExecutionTargetCordoned(database, host, true, "operator maintenance"); err != nil {
		t.Fatalf("manual cordon: %v", err)
	}

	recordWedgeStreak(t, database, host, sourceSyncWedgeThreshold)
	maybeHandleSourceSyncWedge(database, host, slog.Default())

	cordoned, reason, err := db.IsInventoryExecutionTargetCordoned(database, host)
	if err != nil {
		t.Fatalf("cordon lookup: %v", err)
	}
	if !cordoned || reason != "operator maintenance" {
		t.Fatalf("manual cordon reason changed to %q (cordoned=%v)", reason, cordoned)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.HasInventoryHost() {
		t.Fatal("stranded job on wedged manually-cordoned host should be drained")
	}
}

// TestMaybeHandleSourceSyncWedge_DoesNotRentalTagAndRecordsWedgeReason verifies
// the wedge drain returns an on-prem job to the unplaced pool WITHOUT promoting
// it to rental (which would route it to a paid instance) and records the honest
// wedge reason in placement_reasons rather than "manually moved to unplaced
// queue". Regression for the 2026-07-02 VPN-flap incident.
func TestMaybeHandleSourceSyncWedge_DoesNotRentalTagAndRecordsWedgeReason(t *testing.T) {
	database := db.SetupTestDB(t)
	host := "wedge-host"
	jobID, err := db.RecordQueued(database, host, "/tmp", "echo hi", "on-prem job")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	mockSSHFunc(t, func(_, _ string) (string, string, int) { return "", "", 0 })

	recordWedgeStreak(t, database, host, sourceSyncWedgeThreshold)
	maybeHandleSourceSyncWedge(database, host, slog.Default())

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.HasInventoryHost() {
		t.Fatalf("expected job drained off %s, still on host %q", host, job.Host)
	}
	if job.HasTag(db.TagRental) {
		t.Fatalf("wedge-drained on-prem job must not be promoted to rental, tags=%v", job.Tags)
	}
	wantReason := "re-placed from " + host + ": source sync wedged (host filesystem unresponsive)"
	if got := strings.Join(job.PlacementReasons, "\n"); got != wantReason {
		t.Fatalf("placement reasons = %v, want %q", job.PlacementReasons, wantReason)
	}
}

// TestMaybeHandleSourceSyncWedge_SuppressedWhenMultipleHostsWedged verifies the
// observer-blindness guard: when several inventory hosts are timing out at once
// (the signature of a local network outage), a wedge on one of them does NOT
// cordon or drain it. Regression for treating correlated failures as host death.
func TestMaybeHandleSourceSyncWedge_SuppressedWhenMultipleHostsWedged(t *testing.T) {
	database := db.SetupTestDB(t)
	hostA, hostB := "wedge-host-a", "wedge-host-b"
	jobA, err := db.RecordQueued(database, hostA, "/tmp", "echo hi", "job a")
	if err != nil {
		t.Fatalf("record queued a: %v", err)
	}
	mockSSHFunc(t, func(_, _ string) (string, string, int) { return "", "", 0 })

	// Both hosts cross the wedge threshold at once — the observer (this machine)
	// is the likely culprit, not either host's filesystem.
	recordWedgeStreak(t, database, hostA, sourceSyncWedgeThreshold)
	recordWedgeStreak(t, database, hostB, sourceSyncWedgeThreshold)

	maybeHandleSourceSyncWedge(database, hostA, slog.Default())

	if cordoned, _, err := db.IsInventoryExecutionTargetCordoned(database, hostA); err != nil {
		t.Fatalf("cordon lookup: %v", err)
	} else if cordoned {
		t.Fatal("host should not be auto-cordoned while multiple hosts are timing out simultaneously")
	}
	job, err := db.GetJobByID(database, jobA)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !job.HasInventoryHost() {
		t.Fatalf("job should not be drained during a suspected local-network outage; host=%q", job.Host)
	}
}

// TestAutoUncordonRecoveredSourceSyncHosts verifies the cooldown sweep lifts
// only expired wedge auto-cordons, leaving manual and still-cooling cordons.
func TestAutoUncordonRecoveredSourceSyncHosts(t *testing.T) {
	database := db.SetupTestDB(t)
	autoHost, manualHost, freshHost := "auto-host", "manual-host", "fresh-host"

	if err := db.SetInventoryExecutionTargetCordoned(database, autoHost, true, sourceSyncAutoCordonReason+": 3 source-sync timeouts"); err != nil {
		t.Fatalf("auto cordon: %v", err)
	}
	if err := db.SetInventoryExecutionTargetCordoned(database, manualHost, true, "operator maintenance"); err != nil {
		t.Fatalf("manual cordon: %v", err)
	}
	if err := db.SetInventoryExecutionTargetCordoned(database, freshHost, true, sourceSyncAutoCordonReason+": 3 source-sync timeouts"); err != nil {
		t.Fatalf("fresh auto cordon: %v", err)
	}

	// Advance "now" past the retest cooldown so autoHost/freshHost are both
	// expired; cordoned_at is ~now, so a now far in the future expires them.
	future := time.Now().Add(sourceSyncAutoCordonRetestAfter + time.Minute)
	lifted, err := AutoUncordonRecoveredSourceSyncHosts(database, future)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if lifted != 2 {
		t.Fatalf("lifted = %d, want 2 (both auto-cordons)", lifted)
	}
	if c, _, _ := db.IsInventoryExecutionTargetCordoned(database, autoHost); c {
		t.Error("auto-cordoned host should be uncordoned after cooldown")
	}
	if c, reason, _ := db.IsInventoryExecutionTargetCordoned(database, manualHost); !c || reason != "operator maintenance" {
		t.Error("manual cordon should survive the sweep")
	}

	// With now == real now, a freshly auto-cordoned host is within cooldown.
	if err := db.SetInventoryExecutionTargetCordoned(database, freshHost, true, sourceSyncAutoCordonReason); err != nil {
		t.Fatalf("re-cordon fresh: %v", err)
	}
	lifted, err = AutoUncordonRecoveredSourceSyncHosts(database, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if lifted != 0 {
		t.Fatalf("within-cooldown lift = %d, want 0", lifted)
	}
}

// TestRecordSourceSyncOKLiftsAutoCordon verifies a clean recovery sync lifts a
// wedge auto-cordon immediately, but leaves a manual cordon intact.
func TestRecordSourceSyncOKLiftsAutoCordon(t *testing.T) {
	database := db.SetupTestDB(t)

	if err := db.SetInventoryExecutionTargetCordoned(database, "auto-host", true, sourceSyncAutoCordonReason); err != nil {
		t.Fatalf("auto cordon: %v", err)
	}
	recordSourceSyncOK(database, "auto-host", slog.Default())
	if c, _, _ := db.IsInventoryExecutionTargetCordoned(database, "auto-host"); c {
		t.Error("auto-cordon should be lifted on recovery sync")
	}

	if err := db.SetInventoryExecutionTargetCordoned(database, "manual-host", true, "operator maintenance"); err != nil {
		t.Fatalf("manual cordon: %v", err)
	}
	recordSourceSyncOK(database, "manual-host", slog.Default())
	if c, _, _ := db.IsInventoryExecutionTargetCordoned(database, "manual-host"); !c {
		t.Error("manual cordon should survive a recovery sync")
	}
}
