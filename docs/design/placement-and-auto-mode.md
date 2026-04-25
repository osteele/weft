# Placement and Auto-Pilot Mode

## Overview

Weft's placement system decides where jobs run — on-prem inventory hosts or
cloud rental instances. Auto-pilot mode automates this in the TUI, continuously
placing and launching without user intervention.

## Placement Decision Chain

When a job is submitted without an explicit `--host`, placement follows this
priority order:

1. **On-prem inventory hosts** — evaluated first using live metrics (GPU
   utilization, memory, queue depth). Jobs that fit an inventory host are
   assigned immediately.
2. **Existing cloud instances** — grace-period instances rank highest (free
   reuse), then running instances (shared cost). Ranked further by data
   locality (overlapping cached inputs).
3. **New cloud instances** — offers are fetched from providers, scored by the
   active strategy profile (cost-first, balanced, or time-first), and the best
   offer is selected.

### Tag-Based Routing

| Tag         | On-prem | Cloud reuse | Cloud launch |
|-------------|---------|-------------|--------------|
| (none)      | yes     | yes         | yes          |
| `rental`    | **no**  | yes         | yes          |
| `inventory` | yes     | **no**      | **no**       |

`rental` and `inventory` are mutually exclusive.

### Key Code Paths

- **On-prem evaluation**: `internal/placement/placement.go` — `Evaluate()`,
  `placeOnPremWithMetrics()` (rental tag blocks at line ~433)
- **Unified scoring**: `internal/placement/unified.go` — `Evaluate()` scores
  on-prem, cloud-reuse, and cloud-offer candidates
- **Reuse ranking**: `internal/campaign/reuse.go` — `FindReusableInstances()`,
  `RankForJob()`, `NewInstanceCapacity()`
- **Auto planner**: `internal/campaign/auto_planner.go` —
  `BuildAutoPlacementPlan()`, `AutoPlannerProfile()`

## Auto-Pilot Mode

Auto-pilot runs in two TUI contexts: the **grouped jobs list** (`jobs list
--group-by status`) and the **watch TUI** (`campaign watch`, `project watch`).
Toggle with `a` key. When enabled, it runs a pass after every job reload or
sync tick.

### What Auto-Pilot Does

Each pass runs `runAutoPilot()`, which fires two independent actions:

1. **Auto-place** — submits unplaced queued jobs to compatible active instances
   (grace or running) via `BuildAutoPlacementPlan()` reuse assignments.
2. **Auto-launch** — for remaining unplaced rental-eligible jobs, launches new
   cloud instances via `attemptRelaunchOrphanedJobs()`.

### Concurrency Control

Two layers, used together:

1. **Per-scope lease** — `auto_leases` table, 30s TTL. Prevents two TUIs
   sharing the same TUI title from running auto-pilot simultaneously. The
   lease scope is derived from the title (e.g.
   `list_grouped_status:Jobs • unprocessed`). Status shows "another runner
   is active" on contention.
2. **Singleton state row** — `autopilot_state` table (one row, id=1). Coarser
   layer that ensures *only one* autopilot pass is in flight across all weft
   processes regardless of scope, and exposes a sticky `paused` flag. Holders
   heartbeat every 5s; an aged-out claim (> 30s without a heartbeat) is
   reclaimed by the next runner. See `internal/db/autopilot_state.go` and
   `internal/orchestration/autopilot_runner.go`.

### External Pause / Status

The CLI surfaces the singleton state for users and external automation:

```
weft autopilot status            # text: idle | running | stale | paused | never
weft autopilot status --json     # machine-readable (stable contract)
weft autopilot status --quiet    # exit codes: 0=idle, 10=running, 11=stale, 12=paused
weft autopilot pause [--reason ...] [--by ...]
weft autopilot resume
```

While paused, every autopilot runner skips its pass — no auto-placement, no
auto-launch, no automatic relaunch of orphaned jobs. Pause is sticky across
restarts. Use it before manually launching instances or restarting jobs from
another terminal to avoid racing the autopilot.

Pause does *not* interrupt a pass already in flight. After pausing, observe
`weft autopilot status` until it leaves the `running` state before issuing
manual launch commands.

### Blocked Reasons

When auto-pilot cannot place or launch a job, it records a per-job blocked
reason. These are displayed as `blocked: <reason>` lines below unplaced jobs in
the grouped status view.

Sources of blocked reasons:
- **Planner**: "no compatible offers", "planner: ..."
- **Retry budget**: "first retry budget exceeded: elapsed X >= limit Y"
- **Max attempts**: "max cloud attempts reached" (default 3)
- **Runaway breaker**: Global circuit breaker for unattended loops
- **Fallback**: "no offers available" when nothing launched and no specific
  reason was recorded

### Retry Budget and Attempt Counting

Cloud launch attempts are counted per-job, scoped to the current campaign.
The count excludes:
- **Superseded** attempts (job moved to another instance)
- **Orphaned-before-start** attempts (instance failed during provisioning
  before the job started running — infrastructure failure, not job failure)

This means instance provisioning failures do not consume a job's retry budget.
Only attempts where the job actually started execution count.

Budget limits (configurable in `.weft.toml`):
- `retry.first_time_limit` — max elapsed time for first retry cycle
- `retry.first_cost_limit_cents` — max spend for first retry cycle
- `retry.next_time_limit` — subsequent retry cycles
- `retry.next_cost_limit_cents` — subsequent retry cycles

### Auto-Launch Backoff

When auto-launch fails or skips jobs, it backs off with exponential delays
before retrying. The backoff resets when the set of unplaced jobs changes
(new jobs queued or existing jobs placed).

Backoff delays: defined in `watch_tui_commands.go:autoLaunchBackoffDelays`.

### Instance Reuse Criteria

An existing instance is reusable if:
- Status is `running` or `grace`
- No active termination intent
- Grace period has >= 5 minutes remaining (`MinGraceRemaining`)
- GPU class/memory is compatible with the job
- Sufficient disk space for job inputs

Grace instances rank above running instances (free vs shared cost).

## Allium Spec Coverage

The Allium specs cover:
- **Job lifecycle** (`specs/job-lifecycle.allium`) — placement rules, status
  transitions, retry semantics, target kinds
- **Campaign lifecycle** (`specs/campaign-lifecycle.allium`) — instance launch,
  grace period, R2 control plane, reconciliation

Auto-pilot mode is **not** specified in Allium — it's a TUI-level automation
layer that orchestrates the placement and campaign primitives defined in the
specs. The specs explicitly exclude "TUI/dashboard rendering".

## Configuration

Auto-pilot objective (strategy profile) is configurable:
- `auto_objective = "cost_first"` (default) — minimize cost, time is secondary
- `auto_objective = "balanced"` — equal weight to cost and time
- `auto_objective = "time_first"` — minimize time, cost is secondary

Set in `.weft.toml` under `[campaign]`.
