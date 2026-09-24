package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/ssh"
)

type queueBatchStatus struct {
	State         queueState
	ExitCode      *int
	Mtime         int64
	RunID         int64
	GPUDevices    string
	FailureReason string
	FailureDetail string
	AgentVersion  string
	Source        *db.JobSourceExecutionMetadata
	FromR2        bool
}

const r2CompletionMarkerGracePeriod = 2 * time.Minute

var syncJobStatusFromR2ForBatch = syncJobStatusFromR2

// BatchSyncQueueRunnerJobs performs a batched sync for queue-runner jobs on one host/queue.
// This avoids per-job SSH calls by fetching queue state in a single command.
func BatchSyncQueueRunnerJobs(database *sql.DB, host string, jobs []*db.Job, timeout time.Duration) (int, error) {
	if len(jobs) == 0 {
		return 0, nil
	}
	jobIDs := make([]int64, 0, len(jobs))
	jobByID := make(map[int64]*db.Job, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		jobIDs = append(jobIDs, job.ID)
		jobByID[job.ID] = job
	}
	if len(jobIDs) == 0 {
		return 0, nil
	}

	statuses, err := fetchQueueBatchStatus(host, jobIDs, timeout)
	if err != nil {
		for _, job := range jobs {
			if clearErr := clearQueueOutcomeUnknown(database, job); clearErr != nil {
				slog.Warn("failed to reset queue absence observation after batch probe error", "component", "sync", "job_id", job.ID, "error", clearErr)
			}
		}
		if hostUsesR2Queue(host) {
			updated := syncMissingR2Completions(database, jobs, nil)
			updated += syncInventoryPublicationReports(database, jobs)
			if updated > 0 {
				return updated, nil
			}
		}
		return 0, err
	}

	updated, err := applyBatchStatuses(database, jobIDs, jobByID, statuses, timeout)
	// Independent completion records remain useful when another row fails.
	if hostUsesR2Queue(host) {
		updated += syncMissingR2Completions(database, jobs, statuses)
		updated += syncInventoryPublicationReports(database, jobs)
	}
	return updated, err
}

// syncMissingR2Completions closes jobs whose completion the runner's own state
// does not reflect.
//
// Two cases, and the second is why a job the runner still reports is checked
// rather than skipped. A terminal runner entry can age out of the daemon's
// 24-hour state window while the controller is offline. And a runner can go on
// reporting an attempt as running after its worker has exited and written a
// completion record — the worker's record is positive evidence that the attempt
// ended, while the runner's entry is only evidence of what the runner last
// noticed. Trusting the runner over the record is how completed jobs held their
// queue slots for hours, head-blocking the host behind them.
//
// Only presence is acted on. A missing completion record says nothing: an
// attempt that has not finished has none either, so absence is left alone.
//
// Only previously-dispatched attempts are eligible; a brand-new queued row
// should not perform a speculative completion lookup before its first inbox
// write.
func syncMissingR2Completions(database *sql.DB, jobs []*db.Job, statuses map[int64]queueBatchStatus) int {
	updated := 0
	for _, job := range jobs {
		if job == nil || job.LastSyncedStatus == "" {
			continue
		}
		if observed, ok := statuses[job.ID]; ok && observed.State != queueStateRunning && observed.State != queueStateUnresolvedCandidate {
			continue
		}
		result, err := syncJobStatusFromR2ForBatch(database, job)
		if err != nil {
			continue
		}
		if result.Updated {
			updated++
		}
	}
	return updated
}

// applyBatchStatuses processes pre-fetched batch statuses for a set of jobs.
func applyBatchStatuses(database *sql.DB, jobIDs []int64, jobByID map[int64]*db.Job, statuses map[int64]queueBatchStatus, timeout time.Duration) (int, error) {
	return applyBatchStatusesAt(database, jobIDs, jobByID, statuses, timeout, effectiveQueueUnknownAfter(0), time.Now())
}

func applyBatchStatusesAt(database *sql.DB, jobIDs []int64, jobByID map[int64]*db.Job, statuses map[int64]queueBatchStatus, timeout, unknownAfter time.Duration, now time.Time) (int, error) {
	var updated int
	var failures []error
	recordError := func(jobID int64, err error) {
		failures = append(failures, fmt.Errorf("sync job %d: %w", jobID, err))
	}
	for _, jobID := range jobIDs {
		job := jobByID[jobID]
		if job == nil {
			continue
		}
		status, ok := statuses[jobID]
		if !ok {
			if err := clearQueueOutcomeUnknown(database, job); err != nil {
				recordError(jobID, err)
				continue
			}
			continue
		}
		if status.RunID != 0 && (job.LatestRunID == nil || status.RunID != *job.LatestRunID) {
			slog.Debug("ignoring stale queue status", "component", "sync", "job_id", job.ID, "remote_run_id", status.RunID, "latest_run_id", job.LatestRunID)
			continue
		}
		if status.State == queueStateUnresolvedCandidate && status.RunID == 0 {
			slog.Debug("ignoring unfenced unresolved queue observation", "component", "sync", "job_id", job.ID)
			continue
		}
		syncSourceExecutionMetadata(database, job, status)

		switch status.State {
		case queueStateQueued:
			if err := clearQueueOutcomeUnknown(database, job); err != nil {
				recordError(jobID, err)
				continue
			}
			recordQueueDispatchOK(database, job.ID)
			if job.PendingStatus != nil && (*job.PendingStatus == db.StatusCanceled || *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead) {
				if err := removeFromQueueFile(job.Host, job.ID, timeout); err != nil {
					recordError(jobID, err)
					continue
				}
				finalStatus := db.StatusCanceled
				if *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead {
					finalStatus = db.StatusKilled
				}
				if err := db.ClearPendingAndUpdateStatus(database, job.ID, finalStatus); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
				continue
			}
			if job.Status == db.StatusRunning {
				if err := db.MarkQueuedByID(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			}
		case queueStateRunning:
			if err := clearQueueOutcomeUnknown(database, job); err != nil {
				recordError(jobID, err)
				continue
			}
			recordQueueDispatchOK(database, job.ID)
			if job.StartTime == 0 {
				if status.FromR2 && status.Mtime > 0 {
					if err := db.UpdateStartTime(database, job.ID, status.Mtime); err != nil {
						slog.Warn("failed to update start time from R2 runner state", "component", "sync", "job_id", job.ID, "error", err)
					} else {
						job.StartTime = status.Mtime
					}
				} else if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
					slog.Warn("failed to update start time", "component", "sync", "job_id", job.ID, "error", err)
				}
			}
			switch job.Status {
			case db.StatusQueued:
				if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			case db.StatusStarting:
				if err := db.MarkRunningByID(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			case db.StatusPaused:
				if err := db.MarkRunningFromPaused(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
				if err := db.MarkRunningFromTerminal(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			case db.StatusUnresolved:
				if err := db.MarkRunningFromUnresolved(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			}
			if status.GPUDevices != "" {
				syncGPUDevicesToMetadata(database, job, status.GPUDevices)
			}
		case queueStatePaused:
			if err := clearQueueOutcomeUnknown(database, job); err != nil {
				recordError(jobID, err)
				continue
			}
			recordQueueDispatchOK(database, job.ID)
			if job.StartTime == 0 {
				if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
					slog.Warn("failed to update start time", "component", "sync", "job_id", job.ID, "error", err)
				}
			}
			switch job.Status {
			case db.StatusQueued, db.StatusStarting, db.StatusRunning:
				if err := db.MarkPausedByID(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
				if err := db.MarkPausedFromTerminal(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			case db.StatusUnresolved:
				if err := db.MarkPausedFromUnresolved(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			}
			if status.GPUDevices != "" {
				syncGPUDevicesToMetadata(database, job, status.GPUDevices)
			}
		case queueStateUnresolvedCandidate:
			becameUnresolved, err := observeQueueOutcomeUnresolved(database, job, now, unknownAfter)
			if err != nil {
				recordError(jobID, err)
				continue
			}
			if becameUnresolved {
				updated++
			}
		case queueStateDead:
			if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning && job.Status == db.StatusQueued {
				if err := startQueuedJobNow(database, job, timeout); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
				continue
			}
			if isQueuedAndActive(job) {
				continue
			}
			if job.Status != db.StatusDead && job.Status != db.StatusFailed && job.Status != db.StatusKilled && job.Status != db.StatusCanceled {
				if err := db.MarkDeadByID(database, job.ID); err != nil {
					recordError(jobID, err)
					continue
				}
				updated++
			}
		case queueStatePreflightRejected:
			// Persist the reason while this is still the open attempt, then
			// close it at rejection time without a process start or exit code.
			// The dispatch event preserves the diagnostic for job inspection.
			if status.FailureReason != "" {
				if err := db.SetJobRemoteState(database, job.ID, "", status.FailureReason); err != nil {
					slog.Warn("failed to record failure reason", "component", "sync", "job_id", job.ID, "error", err)
				}
				detail := status.FailureDetail
				if detail == "" {
					detail = status.FailureReason
				}
				recordPreflightDispatchBlock(database, job.ID, detail)
			}
			if err := db.CloseAttempt(database, job.ID, db.StatusFailed, nil, status.Mtime); err != nil {
				recordError(jobID, err)
				continue
			}
			updated++
		default:
			if status.ExitCode != nil {
				recordHostReportedFinish(database, job, status)
				if status.FromR2 {
					result, err := syncJobStatusFromR2ForBatch(database, job)
					if err != nil {
						since := status.Mtime
						if since <= 0 && job.QueuedAt > 0 {
							since = job.QueuedAt
						}
						if since <= 0 && job.CreatedAt > 0 {
							since = job.CreatedAt
						}
						if since <= 0 || now.Sub(time.Unix(since, 0)) < r2CompletionMarkerGracePeriod {
							slog.Debug("R2 runner reported completion before result marker was readable", "component", "sync", "job_id", job.ID, "error", err)
							continue
						}
						attemptID, ambiguity := attributeRunnerCompletion(database, job, status.RunID, status.Mtime)
						if attemptID == 0 {
							// Settling here could close the wrong attempt, so
							// weft does not. Recording the refusal is what
							// keeps the job from reporting `running` forever
							// with no trace outside a debug log.
							recordUnattributableCompletion(database, job.ID, ambiguity)
							slog.Warn("R2 completion record unavailable and runner completion cannot be attributed to an attempt", "component", "sync", "job_id", job.ID, "reason", ambiguity, "error", err)
							continue
						}
						slog.Warn("R2 completion record unavailable; using attributed runner completion", "component", "sync", "job_id", job.ID, "run_id", attemptID, "exit_code", *status.ExitCode, "error", err)
						recordQueueDispatchOK(database, job.ID)
						endTime := status.Mtime
						if endTime <= 0 {
							endTime = now.Unix()
						}
						if recErr := RecordJobAttemptCompletion(database, job.ID, attemptID, *status.ExitCode, job.StartTime, endTime); recErr != nil {
							recordError(jobID, recErr)
							continue
						}
						updated++
						continue
					}
					if result.Updated {
						updated++
					}
					continue
				}
				if status.RunID != 0 && job.LatestRunID != nil && status.RunID != *job.LatestRunID {
					slog.Debug("ignoring stale completed queue status", "component", "sync", "job_id", job.ID, "remote_run_id", status.RunID, "latest_run_id", *job.LatestRunID)
					continue
				}
				recordQueueDispatchOK(database, job.ID)
				metaEndTime, _, metaErr := UpdateTimesFromMetadata(database, job, timeout)
				if metaErr != nil {
					recordError(jobID, metaErr)
					continue
				}
				// Second evidence source: a failed/empty metadata read here is
				// silent, and this may be the only tick that ever records this
				// completion — without a start_time the attempt would keep
				// end_time but NULL start_time forever (seen live: wj3871–73).
				BackfillStartTimeFromCompletionRecord(database, job, timeout)
				// Prefer end_time from metadata (system clock) over status file mtime (NFS clock)
				endTime := status.Mtime
				if metaEndTime > 0 {
					endTime = metaEndTime
				}
				if err := RecordJobCompletion(database, job.ID, *status.ExitCode, endTime); err != nil {
					recordError(jobID, err)
					continue
				}
				// Record failure reason if present, and surface as a dispatch
				// block so explain.ForJob shows the reason in `diagnose`.
				if status.FailureReason != "" {
					if err := db.SetJobRemoteState(database, job.ID, "", status.FailureReason); err != nil {
						slog.Warn("failed to record failure reason", "component", "sync", "job_id", job.ID, "error", err)
					}
					recordPreflightDispatchBlock(database, job.ID, status.FailureReason)
				}
				CacheCompletedJobLog(job, timeout)
				// Fetch resource usage data (best-effort). This writes only
				// telemetry/metadata (resource usage samples, phase timings) —
				// not job status — so a dropped write on lock contention is
				// harmless and is re-derived on the next sync pass. The
				// authoritative completion write above (RecordJobCompletion)
				// is the one that must not be lost.
				if _, err := updateJobResourceUsage(database, job, timeout); err != nil {
					slog.Warn("failed to update resource usage", "component", "sync", "job_id", job.ID, "error", err)
				}
				// Sync timeseries telemetry (best-effort)
				if err := syncJobTimeseries(database, job, timeout); err != nil {
					slog.Warn("failed to sync timeseries", "component", "sync", "job_id", job.ID, "error", err)
				}
				if err := syncJobTelemetry(database, job, timeout); err != nil {
					slog.Warn("failed to sync telemetry", "component", "sync", "job_id", job.ID, "error", err)
				}
				updated++
			}
		}
	}

	return updated, errors.Join(failures...)
}

// recordPreflightDispatchBlock inserts a dedup-windowed
// EventQueueDispatchFailed for a job whose runner reported a failure_reason.
// This is what `weft job diagnose` reads via
// explain.LatestInventoryDispatchBlock to surface the reason on a queued or
// just-failed job.
func recordPreflightDispatchBlock(database *sql.DB, jobID int64, detail string) {
	_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchFailed,
		JobID:     jobID,
		Detail:    truncateDispatchDetail(detail),
	}, dispatchEventDedupeWindow)
}

// attributeRunnerCompletion resolves which attempt a runner-reported terminal
// exit belongs to, for the fallback path where the R2 completion record could
// not be read and the runner's word is the only evidence available.
//
// A run id in the runner's entry is authoritative. Older agents publish a
// job-only entry that carries none; that is an absent observation, not a
// statement that the job has no attempt. Two things have to hold before such
// an entry can be placed:
//
//   - The job has exactly one live attempt. With several (a retried job) the
//     entry is genuinely ambiguous, and settling risks closing an attempt that
//     is still running on the host.
//   - The reported finish time falls inside that attempt's window. An entry
//     that predates the attempt's queueing describes some earlier run of the
//     same job id, so attributing it would overwrite a live retry with a
//     stale outcome. An entry with no finish time at all cannot be placed
//     either way, and unknown is not evidence.
//
// Returns (attemptID, "") when attributable, and (0, reason) when not.
func attributeRunnerCompletion(database *sql.DB, job *db.Job, runID, finishedAt int64) (int64, string) {
	if runID != 0 {
		return runID, ""
	}
	attempts, err := db.ListAttempts(database, job.ID)
	if err != nil {
		return 0, "attempt history unreadable: " + err.Error()
	}
	switch len(attempts) {
	case 0:
		return 0, "runner published no run id and the job has no recorded attempt"
	case 1:
	default:
		return 0, fmt.Sprintf("runner published no run id and the job has %d attempts", len(attempts))
	}
	attempt := attempts[0]
	if finishedAt <= 0 {
		return 0, "runner published no run id and no finish time"
	}
	if start := attemptWindowStart(attempt); start > 0 && finishedAt < start {
		return 0, fmt.Sprintf("runner published no run id and its finish time predates the job's only attempt by %s",
			time.Duration(start-finishedAt)*time.Second)
	}
	return attempt.ID, ""
}

// attemptWindowStart is the earliest moment an attempt could have produced a
// completion: its queueing, or its start when the queueing time is unrecorded.
func attemptWindowStart(attempt db.JobAttempt) int64 {
	if attempt.QueuedAt != nil && *attempt.QueuedAt > 0 {
		return *attempt.QueuedAt
	}
	if attempt.StartTime != nil && *attempt.StartTime > 0 {
		return *attempt.StartTime
	}
	return 0
}

// recordUnattributableCompletion surfaces a completion weft refuses to settle.
// Without it the refusal lived only in a slog.Debug, so a job in this state
// reported `running` indefinitely with nothing for `weft status`, `weft
// diagnose`, or `weft log --events` to show.
func recordUnattributableCompletion(database *sql.DB, jobID int64, reason string) {
	detail := "host reports this job finished; the completion cannot be attributed to an attempt"
	if reason != "" {
		detail += " (" + reason + ")"
	}
	_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
		EventKind: db.EventQueueCompletionUnattributable,
		JobID:     jobID,
		Detail:    truncateDispatchDetail(detail),
	}, dispatchEventDedupeWindow)
}

// recordHostReportedFinish records the first pass in which weft saw a runner
// report a terminal exit for a job whose row is not yet terminal, carrying the
// finish time the host reported.
//
// Nothing else in the pipeline records when the observation was made, so a
// settlement delay (wb181: 47 minutes between a host-side END and the recorded
// end_time) could not afterwards be split into "the host told us late" and
// "we were told and took this long". The gap between this event and end_time
// is that second half.
//
// A stale run id describes a superseded attempt, not this one, so it is not an
// observation of the current attempt finishing. An absent mtime means the
// host-reported finish time was not observed; it is recorded as unknown rather
// than filled in with the clock, which would silently pass off weft's own
// observation time as the host's.
//
// Deduped the same way as the dispatch events: the detail is stable for a
// given attempt, so repeated passes over an unsettled job collapse.
func recordHostReportedFinish(database *sql.DB, job *db.Job, status queueBatchStatus) {
	if job == nil || status.ExitCode == nil || db.IsTerminalStatus(job.Status) {
		return
	}
	if status.RunID != 0 && job.LatestRunID != nil && status.RunID != *job.LatestRunID {
		return
	}
	attempt := status.RunID
	if attempt == 0 && job.LatestRunID != nil {
		attempt = *job.LatestRunID
	}
	finish := "unknown"
	if status.Mtime > 0 {
		finish = time.Unix(status.Mtime, 0).UTC().Format(time.RFC3339)
	}
	_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
		EventKind: db.EventQueueCompletionObserved,
		JobID:     job.ID,
		Detail:    truncateDispatchDetail(fmt.Sprintf("attempt %d: host reported exit %d, host finish time %s", attempt, *status.ExitCode, finish)),
	}, dispatchEventDedupeWindow)
}

func fetchQueueBatchStatus(host string, jobIDs []int64, timeout time.Duration) (map[int64]queueBatchStatus, error) {
	if hostUsesR2Queue(host) {
		state, err := fetchR2RunnerState(host)
		if err != nil {
			return nil, err
		}
		results := make(map[int64]queueBatchStatus, len(jobIDs))
		wanted := make(map[int64]struct{}, len(jobIDs))
		for _, id := range jobIDs {
			wanted[id] = struct{}{}
		}
		for _, id := range state.Pending {
			if _, ok := wanted[id]; ok {
				results[id] = queueBatchStatus{State: queueStateQueued, FromR2: true}
			}
		}
		if state.Current != nil {
			if _, ok := wanted[*state.Current]; ok {
				results[*state.Current] = queueBatchStatus{State: queueStateRunning, FromR2: true}
			}
		}
		for idText, running := range state.Running {
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil {
				continue
			}
			if _, ok := wanted[id]; ok {
				observedState := queueStateRunning
				if running.StatusFile == opsqueue.ObservationAbsent && running.Process == opsqueue.ObservationAbsent {
					observedState = queueStateUnresolvedCandidate
				}
				results[id] = queueBatchStatus{
					State: observedState, RunID: running.RunID, Mtime: running.StartedAt,
					GPUDevices: strings.Join(running.GPUDevices, ","), FromR2: true,
				}
			}
		}
		for idText, finished := range state.Finished {
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil {
				continue
			}
			if _, ok := wanted[id]; ok {
				exitCode := finished.ExitCode
				results[id] = queueBatchStatus{RunID: finished.RunID, ExitCode: &exitCode, Mtime: finished.FinishedAt, FromR2: true}
			}
		}
		for idText, rejected := range state.Rejected {
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil {
				slog.Warn("invalid rejected job ID in runner state", "job_id", idText, "error", err)
				continue
			}
			if _, ok := wanted[id]; !ok {
				continue
			}
			if rejected.RunID <= 0 || rejected.RejectedAt <= 0 || !db.IsKnownFailureReason(rejected.FailureReason) {
				slog.Warn("invalid preflight rejection in runner state", "job_id", id, "run_id", rejected.RunID)
				continue
			}
			results[id] = queueBatchStatus{
				State: queueStatePreflightRejected, RunID: rejected.RunID,
				Mtime: rejected.RejectedAt, FailureReason: rejected.FailureReason,
				FailureDetail: rejected.Detail, AgentVersion: state.AgentVersion, FromR2: true,
			}
		}
		return results, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	idList := make([]string, 0, len(jobIDs))
	for _, id := range jobIDs {
		idList = append(idList, fmt.Sprintf("%d", id))
	}
	idsArg := strings.Join(idList, " ")

	agentPath := "~/.cache/weft/bin/weft-agent"
	cmd := fmt.Sprintf("%s batch-status %s", agentPath, idsArg)
	stdout, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		if s := strings.TrimSpace(stderr); s != "" {
			return nil, fmt.Errorf("%w: %s", err, s)
		}
		return nil, err
	}

	return parseQueueBatchStatusOutput(stdout, len(jobIDs)), nil
}

func parseQueueBatchStatusOutput(stdout string, capacity int) map[int64]queueBatchStatus {
	results := make(map[int64]queueBatchStatus, capacity)
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "JOB|") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		state := parts[2]
		switch state {
		case "COMPLETED":
			if len(parts) < 5 {
				continue
			}
			exitCode, err := strconv.Atoi(parts[3])
			if err != nil {
				continue
			}
			var mtime int64
			if parts[4] != "" {
				mtime, _ = strconv.ParseInt(parts[4], 10, 64)
			}
			var runID int64
			failureReason := ""
			failureReasonTrusted := true
			if len(parts) >= 7 {
				if parts[5] != "" {
					runID, _ = strconv.ParseInt(parts[5], 10, 64)
				}
				if parts[6] != "" {
					rawReason := strings.TrimSpace(parts[6])
					failureReasonTrusted = db.IsKnownFailureReason(rawReason)
					failureReason = db.SanitizeFailureReason(rawReason)
				}
			} else if len(parts) >= 6 && parts[5] != "" {
				// TODO(remove after 2026-06-12): legacy agents emitted failure_reason
				// in field 5 before COMPLETED lines included run_id.
				rawReason := strings.TrimSpace(parts[5])
				failureReasonTrusted = db.IsKnownFailureReason(rawReason)
				failureReason = db.SanitizeFailureReason(rawReason)
			}
			status := queueBatchStatus{ExitCode: &exitCode, Mtime: mtime, RunID: runID, FailureReason: failureReason}
			if failureReasonTrusted {
				status.Source = parseBatchSourceExecution(parts, 7)
			}
			results[id] = status
		case "PREFLIGHT_REJECTED":
			// Format: JOB|<id>|PREFLIGHT_REJECTED|<ts>|<failure_reason>
			var mtime int64
			if len(parts) >= 4 && parts[3] != "" {
				mtime, _ = strconv.ParseInt(parts[3], 10, 64)
			}
			failureReason := ""
			failureReasonTrusted := true
			if len(parts) >= 5 && parts[4] != "" {
				rawReason := strings.TrimSpace(parts[4])
				failureReasonTrusted = db.IsKnownFailureReason(rawReason)
				failureReason = db.SanitizeFailureReason(rawReason)
			}
			agentVersion := ""
			if failureReasonTrusted && len(parts) >= 6 {
				agentVersion = strings.TrimSpace(parts[5])
			}
			var runID int64
			if len(parts) >= 7 {
				var err error
				runID, err = strconv.ParseInt(parts[6], 10, 64)
				if err != nil || runID <= 0 {
					slog.Warn("invalid preflight rejection run ID", "job_id", id, "run_id", parts[6])
					continue
				}
			}
			results[id] = queueBatchStatus{State: queueStatePreflightRejected, Mtime: mtime, FailureReason: failureReason, AgentVersion: agentVersion, RunID: runID}
		case "CURRENT", "RUNNING":
			gpuDevs := ""
			if len(parts) >= 4 {
				gpuDevs = parts[3]
			}
			results[id] = queueBatchStatus{State: queueStateRunning, GPUDevices: gpuDevs, Source: parseBatchSourceExecution(parts, 4)}
		case "UNRESOLVED_CANDIDATE":
			if len(parts) < 4 {
				continue
			}
			runID, err := strconv.ParseInt(parts[3], 10, 64)
			if err != nil || runID <= 0 {
				continue
			}
			results[id] = queueBatchStatus{State: queueStateUnresolvedCandidate, RunID: runID}
		case "PAUSED":
			gpuDevs := ""
			if len(parts) >= 4 {
				gpuDevs = parts[3]
			}
			results[id] = queueBatchStatus{State: queueStatePaused, GPUDevices: gpuDevs, Source: parseBatchSourceExecution(parts, 4)}
		case "QUEUED":
			results[id] = queueBatchStatus{State: queueStateQueued}
		case "DEAD":
			results[id] = queueBatchStatus{State: queueStateDead}
		}
	}

	return results
}

func parseBatchSourceExecution(parts []string, offset int) *db.JobSourceExecutionMetadata {
	if len(parts) < offset+8 || strings.TrimSpace(parts[offset]) == "" {
		return nil
	}
	verifiedAt, _ := strconv.ParseInt(parts[offset+5], 10, 64)
	rootCount, _ := strconv.Atoi(parts[offset+7])
	return &db.JobSourceExecutionMetadata{
		DispatchMode:     strings.TrimSpace(parts[offset]),
		IdentityKind:     strings.TrimSpace(parts[offset+1]),
		DispatchedSHA256: strings.TrimSpace(parts[offset+2]),
		VerifiedSHA256:   strings.TrimSpace(parts[offset+3]),
		Verification:     strings.TrimSpace(parts[offset+4]),
		VerifiedAt:       verifiedAt,
		AgentVersion:     strings.TrimSpace(parts[offset+6]),
		RootCount:        rootCount,
	}
}

func syncSourceExecutionMetadata(database *sql.DB, job *db.Job, status queueBatchStatus) {
	if job == nil {
		return
	}
	execution := status.Source
	if execution == nil && status.State == queueStatePreflightRejected && job.Metadata != nil && job.Metadata.Source != nil && job.Metadata.Source.Pin != nil {
		pin := job.Metadata.Source.Pin
		verification := db.SourceVerificationPending
		switch status.FailureReason {
		case db.FailureReasonSourceProvenanceMismatch:
			verification = db.SourceVerificationMismatch
		case db.FailureReasonPinnedSourceUnavailable,
			db.FailureReasonPinnedSourceFetchFailed,
			db.FailureReasonR2IsolatedSourceUnavailable,
			db.FailureReasonR2IsolatedSourceFetchFailed:
			verification = db.SourceVerificationObjectsUnavailable
		}
		execution = &db.JobSourceExecutionMetadata{
			DispatchMode:     "pinned_inventory_manifest",
			IdentityKind:     db.SourceIdentityManifestV2,
			DispatchedSHA256: pin.Hash,
			Verification:     verification,
			RootCount:        len(pin.Roots),
			AgentVersion:     status.AgentVersion,
		}
	}
	if execution == nil {
		return
	}
	copy := *execution
	if job.Metadata != nil && job.Metadata.Source != nil && job.Metadata.Source.Pin != nil {
		copy.SubmittedIdentityKind = db.SourceIdentityManifestV2
		copy.SubmittedSHA256 = job.Metadata.Source.Pin.Hash
		if copy.IdentityKind == copy.SubmittedIdentityKind {
			if copy.DispatchedSHA256 != copy.SubmittedSHA256 || (copy.VerifiedSHA256 != "" && copy.VerifiedSHA256 != copy.SubmittedSHA256) {
				copy.Verification = db.SourceVerificationMismatch
			}
		}
	}
	meta := job.Metadata
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if meta.Source == nil {
		meta.Source = &db.JobSourceMetadata{}
	}
	if reflect.DeepEqual(meta.Source.Execution, &copy) {
		return
	}
	meta.Source.Execution = &copy
	if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
		slog.Warn("failed to update source execution metadata", "component", "sync", "job_id", job.ID, "error", err)
		return
	}
	job.Metadata = meta
}

// syncGPUDevicesToMetadata persists the assigned GPU devices to the job's metadata
// if they differ from what's already stored.
func syncGPUDevicesToMetadata(database *sql.DB, job *db.Job, gpuDevices string) {
	meta := job.Metadata
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if meta.Resource == nil {
		meta.Resource = &db.ResourceUsage{}
	}
	if meta.Resource.GPUDevices != gpuDevices {
		meta.Resource.GPUDevices = gpuDevices
		if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
			slog.Warn("failed to update GPU devices metadata", "component", "sync", "job_id", job.ID, "error", err)
		}
		job.Metadata = meta
	}
}
