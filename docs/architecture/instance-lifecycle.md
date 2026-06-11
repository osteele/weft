# Cloud Instance Lifecycle Architecture

How rental instances are launched, supervised, reused, and torn down.
Authoritative behavior lives in `specs/campaign-lifecycle.allium`. User-facing
behavior: `docs/guides/instances.md` and
`docs/guides/cloud-instance-debugging.md`.

## States and stickiness

Launch statuses: `planned → launching → running ⇄ paused / grace →
completed | failed | cancelled`. Terminal statuses are **sticky**:
non-terminal writes (`UpdateLaunchStatus` running/default branches,
`SetLaunchGraceStarted`) are guarded with `status NOT IN (terminal)` and
return `ErrLaunchTerminal` when skipped, so a late "running" write from a
launch that the user terminated mid-flight cannot resurrect the row (and
`LaunchInstance` aborts instead of re-failing it). DB triggers enforce that
terminal rows carry `ended_at` and a termination reason.

## Launch

`campaign.LaunchInstance` creates a placeholder DB row, creates the provider
instance, records the provider ID, uploads the manifest and bootstrap script
to R2, then marks the row running. Every post-create failure path runs the
same ritual (`failLaunchInfra`): destroy the leaked provider instance, mark
the launch failed with an infra reason, and release the claimed jobs
(`resetLaunchJobsForFailure`) so they relaunch promptly. Backstops
(`ResetJobsOnTerminalLaunches`, orphan sweeps, `SweepStalePlannedLaunches`)
repair anything that slips through.

Launch gates: dependency readiness, provider credit, price authorization,
cloud-provisionable inputs, and the source-size validation shared with the
reuse path.

## Supervision: reconcile + instance check

Two periodic passes compare provider state, R2 phase markers, and the DB:

- **Reconcile** (`internal/campaign/reconcile.go`,
  `internal/cloudreconcile`): ingests phase/heartbeat markers, detects
  provider-dead instances (with hysteresis and "presume alive on API error"
  defaults), pauses/resumes interruptible instances, enters grace only when
  the agent's grace marker says `waiting` (a `running` marker carries a
  stale pre-extension deadline), and handles disk-full and idle timeouts.
- **Instance check** (`instance_check.go`): phase-stall and
  launching-phase timeouts, dud-Vast detection (multi-signal), hedge-cohort
  culls, grace-deadline enforcement, and runaway breakers.

Provider death deliberately does **not** fail grace-period instances (a
transient API blip must not kill a recoverable session); grace instances
reach terminal state via expiry or release.

## Grace protocol

After a job failure the instance idles in **grace** (default 5m) while the
agent polls R2 control keys (`grace/<id>/{status,jobs.json,extend,release}`):

- `status` carries `state` (`waiting` / `running` / `completed` — constants
  in `internal/controlplane`) and the deadline.
- **Extension is absolute**: `extend` sets deadline = now + duration (not
  additive), and resubmitted-job runtime does not consume grace (the agent
  shifts the deadline forward by the job's runtime).
- `weft instance submit/extend/release` write these keys; the reuse pass
  uses the same channel to hand new jobs to a live agent.

## Reuse and relaunch

- **Reuse** (`internal/campaign/reuse.go`): running/grace instances accept
  compatible queued jobs. Sources are validated **before** any claim
  (deterministic rejections leave zero attempt rows); the autopilot backs
  off after consecutive submit failures (`reuse.submit_failed` lifecycle
  streak + `retrypolicy.BackoffDelayClamped`) and records per-instance
  rejection detail into the persisted blocked reason.
- **Relaunch** (`relaunch.go`): retryable terminations
  (`IsRetryableTermination`: provider/infra failures, bootstrap timeouts,
  stalls, preemption, provider timeout, upload stall, credit exhaustion)
  re-place orphaned jobs on fresh offers, with an attempt budget,
  attempt-row-derived backoff, runaway breakers, and probe launches.

## Termination

Paths to terminal state: job completion (agent exit report), user
terminate (`weft instance terminate` — direct provider destroy, stamps
`termination_requested_at`, resets jobs), grace expiry, reconcile/check
actions (each with a specific `termination_reason`), and agent-side
self-destruct (upload-stall breaker writes the failure marker first, then
destroys). `weft instance diagnose` reconstructs the timeline; `weft cost
instances` attributes spend.

## File map

| Package | Role |
|---|---|
| `internal/campaign` | Launch execution, reconcile, instance check, reuse, relaunch, grace guard, orphan sweep |
| `internal/db/cloud_instances.go` | Launch rows, sticky-terminal writes, grace fields, attempt associations |
| `internal/cloudreconcile` | Two-phase reconcile scheduling and leases |
| `internal/instanceintent` | Termination-intent markers |
| `internal/orchestration/instance_lifecycle.go` | User-initiated terminate |
| `internal/vastai`, `internal/runpod`, `internal/cloudproviders` | Provider clients |
| `cmd/agent` | Instance agent: job loop, grace wait, kill poller, self-destruct |
| `cmd/instance*.go`, `cmd/campaign*.go` | CLI surfaces (launch, watch, diagnose, extend, release) |
