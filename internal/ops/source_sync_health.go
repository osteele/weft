package ops

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/retrypolicy"
)

// Source-sync wedge handling.
//
// A host whose working-dir filesystem is wedged (e.g. a stuck hard-NFS mount)
// makes every source rsync hang until the context deadline SIGKILLs it. No
// timeout value fixes that, and each retry spawns another unkillable rsync that
// piles up, so weft instead: (1) backs off retries per host as the timeout
// streak grows, and (2) once the streak crosses a threshold, auto-cordons the
// host and unplaces its queued inventory jobs so the autopilot re-places them
// on a healthy host. A wedge auto-cordon is lifted automatically once its
// retest cooldown elapses (or immediately on a clean recovery sync), so a host
// whose mount comes back rejoins rotation without manual intervention.

const (
	// sourceSyncWedgeThreshold is the number of consecutive source-sync
	// timeouts on a host before weft treats it as wedged and re-places its
	// queued work. With the ~2m source rsync timeout this is roughly 6 minutes
	// of sustained failure, matching the deliberateness of the 10m auto-replan.
	sourceSyncWedgeThreshold = 3

	// sourceSyncAutoCordonRetestAfter is how long a wedge auto-cordon stays in
	// effect before the recovery sweep lifts it to retest the host. After
	// cordon+unplace no job remains on the host to drive a recovery sync, so
	// recovery is cooldown-based: long enough not to thrash a genuinely wedged
	// mount, short enough that a recovered host returns without manual action.
	sourceSyncAutoCordonRetestAfter = 15 * time.Minute

	// sourceSyncAutoCordonReason prefixes cordons the wedge detector applied.
	// Only auto-applied cordons are auto-lifted; a manual `weft instance
	// cordon` (any other reason) is left untouched.
	sourceSyncAutoCordonReason = "source sync wedged (auto)"

	// simultaneousWedgeWindow bounds how recently another host's source-sync
	// streak must have failed to count as a correlated failure. When this
	// machine's own network drops, every host's dispatch times out within the
	// same short window; a streak whose last timeout is older than this reflects
	// an unrelated, already-resolved episode rather than the current outage.
	simultaneousWedgeWindow = 10 * time.Minute
)

// recordSourceSyncTimeout extends a host's source-sync timeout streak.
func recordSourceSyncTimeout(database *sql.DB, host string) {
	host = strings.TrimSpace(host)
	if database == nil || host == "" {
		return
	}
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventSourceSyncTimeout,
		Detail:    host,
	})
}

// recordSourceSyncOK resets a host's timeout streak and, when the host was
// auto-cordoned for a wedge, lifts that cordon immediately so a recovered host
// rejoins rotation without waiting for the retest cooldown.
func recordSourceSyncOK(database *sql.DB, host string, syncLog *slog.Logger) {
	host = strings.TrimSpace(host)
	if database == nil || host == "" {
		return
	}
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventSourceSyncOK,
		Detail:    host,
	})
	if lifted, err := liftSourceSyncAutoCordon(database, host); err != nil {
		if syncLog != nil {
			syncLog.Debug("source-sync auto-uncordon failed", "host", host, "error", err)
		}
	} else if lifted {
		oplog.Log("source_sync.auto_uncordon", oplog.WithHost(host), oplog.WithDetail("source sync recovered"))
	}
}

// sourceSyncBackoffRemaining returns how long source sync to host should be
// skipped given its current timeout streak, or 0 when an attempt is due.
func sourceSyncBackoffRemaining(database *sql.DB, host string, now time.Time) time.Duration {
	count, lastFailureAt, err := db.HostSourceSyncTimeoutStreak(database, host)
	if err != nil || count == 0 {
		return 0
	}
	return retrypolicy.BackoffRemaining(count, time.Unix(lastFailureAt, 0), now)
}

// maybeHandleSourceSyncWedge drains a host whose source-sync timeout streak has
// crossed the wedge threshold: it unplaces the host's queued inventory jobs so
// the autopilot re-places them elsewhere, and auto-cordons the host if it isn't
// already cordoned. Below threshold it does nothing.
//
// The drain runs whether the cordon is ours or the operator's. A plain cordon
// only blocks *new* placement; it never moves a job already assigned to the
// host, so a job queued on a host that later wedges stays stranded — exactly
// the gap this closes. We leave an existing cordon's reason untouched (only
// auto-applied cordons are auto-lifted on recovery), so an operator's manual
// cordon and its rationale survive while its stranded jobs still drain.
func maybeHandleSourceSyncWedge(database *sql.DB, host string, syncLog *slog.Logger) {
	host = strings.TrimSpace(host)
	if database == nil || host == "" {
		return
	}
	count, _, err := db.HostSourceSyncTimeoutStreak(database, host)
	if err != nil || count < sourceSyncWedgeThreshold {
		return
	}

	// Suspect the observer first. Cordon + drain is a destructive action, so an
	// unknown cause is never sufficient (CLAUDE.md "Absence of Evidence").
	// When several independent hosts are timing out at once, the most likely
	// broken component is this machine's own network, not every host's
	// filesystem — draining them all would strand healthy work on paid
	// re-placements. If more than one inventory host has an active, recent
	// source-sync timeout streak, skip; the host re-cordons on its next failed
	// dispatch once the network recovers and streaks diverge.
	if active, lookupErr := db.CountActiveSourceSyncTimeoutHosts(database, time.Now().Add(-simultaneousWedgeWindow)); lookupErr != nil {
		// A local DB read failing is unrelated to network reachability and must
		// not by itself block remediation of a genuinely wedged single host;
		// the primary streak signal already crossed threshold. Log and proceed.
		syncLog.Debug("source-sync wedge: correlated-failure lookup failed", "host", host, "error", lookupErr)
	} else if active > 1 {
		syncLog.Warn("multiple hosts unreachable simultaneously; suspecting local network, not host wedge", "host", host, "active_timeout_hosts", active)
		oplog.Log("source_sync.wedge_suppressed", oplog.WithHost(host), oplog.WithDetailf("%d inventory hosts timing out simultaneously; suspecting local network", active))
		return
	}

	cordoned, _, err := db.IsInventoryExecutionTargetCordoned(database, host)
	if err != nil {
		syncLog.Debug("source-sync wedge: cordon lookup failed", "host", host, "error", err)
		return
	}
	if !cordoned {
		cordonReason := fmt.Sprintf("%s: %d source-sync timeouts; re-placing queued jobs", sourceSyncAutoCordonReason, count)
		if err := db.SetInventoryExecutionTargetCordoned(database, host, true, cordonReason); err != nil {
			syncLog.Debug("source-sync wedge: auto-cordon failed", "host", host, "error", err)
			return
		}
		syncLog.Info("auto-cordoned wedged host; re-placing its queued jobs", "host", host, "source_sync_timeouts", count)
		oplog.Log("source_sync.auto_cordon", oplog.WithHost(host), oplog.WithDetailf("%d source-sync timeouts", count))
	}

	if moved := unplaceQueuedInventoryJobsOnHost(database, host, syncLog); moved > 0 {
		oplog.Log("source_sync.wedge_replan", oplog.WithHost(host), oplog.WithDetailf("re-placed %d queued job(s)", moved))
	}
}

// unplaceQueuedInventoryJobsOnHost returns each queued inventory job on host to
// the unplaced pool so the autopilot re-places it; the host's cordon keeps the
// re-placement off the wedged host. Returns the number moved.
func unplaceQueuedInventoryJobsOnHost(database *sql.DB, host string, syncLog *slog.Logger) int {
	jobs, err := db.ListJobs(database, db.StatusQueued, host, 0, nil, "unprocessed")
	if err != nil {
		syncLog.Debug("source-sync wedge: list queued jobs failed", "host", host, "error", err)
		return 0
	}
	moved := 0
	for _, job := range jobs {
		if job == nil || job.Host != host || !job.HasInventoryHost() || job.UsesSlurm() {
			continue
		}
		if job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		reason := fmt.Sprintf("re-placed from %s: source sync wedged (host filesystem unresponsive)", host)
		opts := OptionsForMode(TimeoutFast)
		opts.UnplaceReason = reason
		if _, err := UnplaceQueuedJob(database, job, opts); err != nil {
			syncLog.Debug("source-sync wedge: unplace failed", "job_id", job.ID, "host", host, "error", err)
			continue
		}
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventQueueDispatchAutoReplanned,
			JobID:     job.ID,
			Detail:    reason,
		})
		oplog.LogJob("source_sync.wedge_replan", job.ID, host, oplog.WithDetail(reason))
		moved++
	}
	return moved
}

// AutoUncordonRecoveredSourceSyncHosts lifts wedge auto-cordons whose retest
// cooldown has elapsed, letting the autopilot place work on the host again. A
// host still wedged re-cordons on its next failed dispatch; a recovered host
// stays eligible and records source_sync.ok on its next clean sync. Manual
// cordons are never touched. Returns the number of hosts uncordoned.
func AutoUncordonRecoveredSourceSyncHosts(database *sql.DB, now time.Time) (int, error) {
	if database == nil {
		return 0, nil
	}
	targets, err := db.ListExecutionTargets(database)
	if err != nil {
		return 0, err
	}
	cutoff := int64(sourceSyncAutoCordonRetestAfter / time.Second)
	lifted := 0
	for _, t := range targets {
		if t == nil || t.Kind != db.ExecutionTargetInventoryHost || !t.Cordoned {
			continue
		}
		if !strings.HasPrefix(t.CordonReason, sourceSyncAutoCordonReason) {
			continue
		}
		if t.CordonedAt == nil || now.Unix()-*t.CordonedAt < cutoff {
			continue
		}
		if err := db.SetInventoryExecutionTargetCordoned(database, t.Host, false, ""); err != nil {
			return lifted, fmt.Errorf("auto-uncordon %s: %w", t.Host, err)
		}
		oplog.Log("source_sync.auto_uncordon", oplog.WithHost(t.Host), oplog.WithDetail("retest cooldown elapsed"))
		lifted++
	}
	return lifted, nil
}

// liftSourceSyncAutoCordon clears a host's cordon only when it is a wedge
// auto-cordon. Returns true when a cordon was lifted.
func liftSourceSyncAutoCordon(database *sql.DB, host string) (bool, error) {
	cordoned, reason, err := db.IsInventoryExecutionTargetCordoned(database, host)
	if err != nil || !cordoned || !strings.HasPrefix(reason, sourceSyncAutoCordonReason) {
		return false, err
	}
	if err := db.SetInventoryExecutionTargetCordoned(database, host, false, ""); err != nil {
		return false, err
	}
	return true, nil
}
