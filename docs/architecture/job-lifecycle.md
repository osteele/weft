# Job Lifecycle Architecture

How job status is modeled, transitioned, and reconciled across the local
SQLite DB, remote queue runners, and cloud agents. Authoritative behavior
lives in `specs/job-lifecycle.allium` (states, transitions, user actions)
and `specs/status-sync.allium` (remote→local reconciliation).

## The transition graph is pinned

Stored statuses: `draft, queued, starting, running, paused, completed,
failed, dead, killed, canceled`. The spec's transition graph and the Go
table (`internal/status/status.go`) are kept identical edge-for-edge by
`internal/status/spec_transitions_test.go` — change one and the test forces
you to change the other. `ValidateTransition` enforces the graph; edges
marked `Authoritative` require an authoritative source (R2 completion).
`orphaned` is a **derived display status** (the `job_status` view reports it
when an attempt closed terminally because its backing launch died), never
stored and not in the graph.

The DB is the source of truth: display and query layers must not
reinterpret stored status with heuristics. The one sanctioned derivation is
`EffectiveStatus()` (an unplaced attempt cannot truly be running) and the
R2 phase refining the *active* job's status only.

## Three-way merge for user actions

User stop intents follow a requested/pending/synced model
(`internal/ops/kill.go` `StopJob`): set `requested_status`, attempt to apply
on the remote (PID kill, tmux kill, or R2 kill marker for cloud), and
reconcile. If the host is unreachable the intent stays pending and a later
sync applies it. Cancel and kill share mechanics but record distinct
terminal statuses — a user cancel lands as `canceled`, never silently as
`killed`.

For cloud jobs, the CLI stamps the terminal status immediately and writes a
kill signal to R2 (`instance/<id>/kill-job`); the agent's kill poller
escalates SIGTERM→SIGKILL with a grace window
(`runner.KillProcessGroupWithGrace`, `DefaultKillGrace`) so jobs can flush
checkpoints, and records `kill_reason=user_kill`.

## Authoritative R2 completions

The agent's `.complete` marker (exit code; full metadata in
`<job>.completion.json`) is ground truth about a cloud job's outcome.
Ingestion (`internal/syncorch/cloud_results.go`,
`internal/campaign/job_completion.go` → `db.RecordCloudJobCompletion`):

- `exit 0` → `completed`, overriding any guessed terminal state
  (`* → completed` authoritative edges).
- `exit != 0` → `failed`, overriding orphan recovery's `dead`/`killed`
  guesses (`dead/killed → failed` authoritative edges) — but **never**
  overriding `completed` (a stale failed marker must not clobber a later
  authoritative success).
- **User-kill carve-out**: a completion whose `kill_reason` is `user_kill`
  was produced by weft's own kill signal; it confirms the user's stop, so
  the attempt keeps its `killed`/`canceled` status and only backfills exit
  metadata.

Permanently rejected completions (transition-validation errors) get their
`.processed` marker written so they are not re-downloaded and re-rejected
every sync pass; transient errors stay unprocessed for retry. Marker-only
ingestion (results JSON not yet uploaded) leaves the row backfill-eligible
(`NeedsCloudCompletionBackfill`) so a later sync can fill authoritative
times.

## Failure detection and recovery

- **Queue runner** (`internal/runner`): layered exit-code capture — bash
  exit-capture trap → Go process waiter → zombie/orphan recovery in
  `refreshRunningJobs`. Watchdogs (stdout-silence, GPU-idle, max-time,
  fatal-log patterns) kill with grace and record a kill reason; a
  process-exit mutex prevents a late watchdog tick from mislabeling a clean
  exit. Setup failures close out fully (status, failure reason, completion
  record, PID/payload cleanup).
- **Kill reasons** (`<job>.kill_reason` file) discriminate user kills from
  watchdog and shutdown kills and flow into the completion record.
- **Orphan recovery** marks attempts `dead` when the process vanished; this
  is a guess that authoritative completions may later correct (above).

## Requeue and retry

`weft restart` / requeue is legal from any terminal status including
`completed`; it closes the prior attempt and opens a fresh queued one
(attempt numbers are monotonic; `latest_run_id` tracks the active attempt).
Cloud relaunch and reuse retries are bounded by `internal/retrypolicy`
(attempt budget + backoff) — see `docs/architecture/instance-lifecycle.md`.

## File map

| Package | Role |
|---|---|
| `internal/status` | Transition table, validation, spec-pinning test |
| `internal/db` | Jobs/attempts schema, status writes (`StampAttemptStatus`), completion recording, `EffectiveStatus`, `job_status` view |
| `internal/ops` | User actions (stop/cancel/requeue), three-way merge, reconciliation |
| `internal/orchestration` | `KillOrCancelJob` routing (cloud vs on-prem), autopilot |
| `internal/runner` | Queue-runner execution, watchdogs, orphan/zombie recovery |
| `internal/syncorch`, `internal/campaign/job_completion.go` | R2 completion ingestion (full pass and watch fast-path) |
| `cmd/agent` | Cloud agent job loop, kill poller, exit reporting |
