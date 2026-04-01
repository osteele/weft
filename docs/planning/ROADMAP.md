# Campaign System Roadmap

Gaps between weft's campaign system and llm-performance-models' Vast.ai
management. Filling these would let llm-performance-models run on top of weft
instead of maintaining its own provisioning infrastructure.

## Status Key

- **Done**: Implemented in weft
- **Gap**: Not yet implemented, needed for llm-performance-models integration

## Current State (Done)

| Feature | Notes |
|---------|-------|
| Instance provisioning | Create Vast.ai instance, wait for ready, deploy via SSH |
| Parallel instance launch | All instances in a campaign launch concurrently |
| Parallel terminate | Multiple instances destroyed concurrently |
| Result collection via R2 | Wrapper uploads logs/exit codes to R2, sweep loop processes them |
| DB-backed state | `campaigns` + `cloud_instances` tables with status tracking |
| Watch mode | TUI and plain-text instance monitors |
| Non-interactive launch | `--yes` flag for scripted/agent-driven launches |
| Job filtering | `--jobs` flag to select specific job IDs |
| SSH access | `instance ssh <id>` and `instance ssh --print <id>` |
| Orphan detection | Sweep loop detects timed-out and exited instances |
| OOM detection | Exit code 137 + dmesg/nvidia-smi analysis |

## Tier 1: Must Have

| Gap | Priority | Description | llm-perf-models ref |
|-----|----------|-------------|---------------------|
| Persistent volumes | High | Vast.ai volumes (~60GB) for uv + HuggingFace caches, reused across instances. Saves 10-30min downloads per instance. | `vastai_provision.sh`: volume create/find/attach |
| Phone-tree copy | High | Queue-based fan-out: seed instance downloads everything, then copies fan out to targets (seed→1, seed+1→2, 4→4, etc.). Load-balanced donor selection. | `run_campaign.py`: copy loop with seeded/unprovisioned sets |
| Copy failure fallback | High | Retry copies N times, then fall back to direct download + install. | `run_campaign.py`: `--copy-retries` flag |

## Tier 2: High Value

| Gap | Priority | Description | llm-perf-models ref |
|-----|----------|-------------|---------------------|
| Seed vs target distinction | Medium | Separate seed instance (downloads everything first) from target instances (receive copies). Seed can be a different GPU type. | `--seed-gpu`, `--seed-instance` flags |
| External donor support | Medium | Reuse a pre-seeded instance as copy source without destroying it. | `--donor-instance ID` flag |
| Phase tracking in DB | Medium | Per-instance sub-statuses: provisioning → copying → seeded → running → done. Currently only: planned → launching → running → completed. **Partial**: `job_phase_timings` table now stores per-job phase timestamps (setup, run, upload), cache state, and GPU stats. Instance-level sub-statuses still use the original set. | State file with per-GPU status tracking |
| Per-GPU cost tracking | Medium | Track actual spend per instance (hourly rate × uptime), not just budget limits. Report total campaign cost at end. **Partial**: `cloud_instances` now stores `cost_per_hour_cents` and lifecycle timestamps (`ready_at`, `launched_at`, `ended_at`). Cost estimation uses predictor-based durations when available. `actual_spend_cents` not yet computed. | Cost summary with per-GPU breakdown |
| Resume interrupted campaigns | Medium | `--resume` flag to replay from DB state, skipping completed phases. | `--resume` flag, JSON state file |
| Seed-first validation | Medium | `--seed-first`: calibrate on seed instance before fanning out copies, to validate scripts work before provisioning N instances. | `--seed-first` flag |

## Tier 3: Nice to Have

| Gap | Priority | Description | llm-perf-models ref |
|-----|----------|-------------|---------------------|
| Volume pooling | Low | Reuse existing Vast.ai volumes across campaigns. Try existing volumes before creating new ones. | Volume find/reuse in provisioning |
| Multi-phase workflows | Low | Run multiple phases per instance (e.g., power calibration, then timing calibration). Currently: single wrapper script. | Two-phase calibration per GPU |
| vLLM-only mode | Low | Run subset of calibration (~20min vs 2+ hours). Would need weft to understand calibration phases. | `--only-vllm` flag |

## Integration Strategy

Even without filling all gaps, llm-performance-models could use weft for the
provisioning/lifecycle layer today:

1. `weft campaign launch --yes --jobs <ids>` provisions instances
2. `weft instance ssh --print <id>` gets SSH connection details
3. llm-performance-models runs its own calibration scripts via SSH
4. `weft campaign terminate <id>` cleans up when done

The main blockers for deeper integration are persistent volumes (Tier 1) and
phone-tree copy orchestration (Tier 1).

## Implementation Notes

### Persistent Volumes

Add to `internal/vastai/`:
- `CreateVolume(sizeGB int) (volumeID string, err error)`
- `FindVolume(label string) (volumeID string, err error)`
- `AttachVolume(instanceID int, volumeID string) error`

Add `volume_id` column to `cloud_instances` table.

### Phone-tree Copy

Add to `internal/campaign/`:
- `PhoneTreeCopier` type with seeded/unprovisioned instance sets
- `CopyLoop()` method: picks donor with fewest active copies, starts SSH copy
- `CopyPaths`: configurable list of paths to copy (uv cache, HF cache, project)

Add `copy_source_id`, `copy_status`, `copy_duration_s` columns to
`cloud_instances` table.

## Future: Rename status/pending_status to outcome/intent

The `status` column conflates two concerns: what happened on the last attempt
(outcome) and whether the job should be placed (intent). `pending_status` already
serves as an intent override, but the column names don't reflect these roles.

### Column mapping

| Current | Proposed | Meaning |
|---------|----------|---------|
| `status` | `outcome` | What happened on the last attempt (`pending`, `running`, `starting`, `paused`, `completed`, `failed`, `dead`, `killed`) |
| `pending_status` | `intent` | What the user wants next (`run`, `cancel`, `pause`, `draft`, `NULL` = accept current outcome) |
| `last_synced_status` | `last_synced_outcome` | Three-way merge base |

### New value sets

**outcome**: Drop `queued`, `canceled`, `draft` (those are intents, not outcomes).
Add `pending` for jobs that have never been attempted.

**intent**: `run` (place/retry me), `cancel`, `pause`, `draft`, `NULL` (no action).

### Placement query

```sql
WHERE intent = 'run' AND host = ''
```

### Display status (derived)

| intent | outcome | Display |
|--------|---------|---------|
| `cancel` | any | "canceled" |
| `draft` | any | "draft" |
| `run` | `pending` | "queued" |
| `run` | `failed` | "retry-pending" |
| `NULL` | any | show outcome directly |

### Migration sketch

1. Add `outcome` and `intent` columns
2. Backfill from existing data
3. Audit and update all queries
4. Drop old columns

### Stronger variant: derive status entirely from attempts + intent

Instead of renaming columns, make `job_status` a pure view with no mutable
status column. The view computes display status from:

1. **User intent** (`requested_status`): `queued`, `canceled`, `killed`, `draft`
2. **Latest attempt** (if any): `start_time`, `end_time`, `exit_code`, `launch_id`
3. **Instance status** (via `launches.status`): whether the instance is still alive

```
IF requested_status IN ('canceled','killed','draft') → show that
IF no attempts → 'queued'
IF latest attempt has end_time:
  exit_code = 0 → 'completed'
  exit_code != 0 → 'failed'
  exit_code IS NULL AND launch terminal → 'orphaned'
IF latest attempt has start_time but no end_time:
  launch terminal → 'orphaned'
  ELSE → 'running'
IF latest attempt has no start_time → 'queued'
```

**Key design choice**: attempts only exist once a host/instance is assigned.
No phantom "queued" attempts. Retry = set `requested_status = 'queued'` (a
new attempt is created only when the scheduler places the job). This makes
`attempt_number` count real execution attempts.

This eliminates `ResetLaunchJobs`, `CloseLaunchAttempts`, `cloud_outcome`,
and the many code paths that mutate job status during instance lifecycle
events. The instance lifecycle only writes to `launches`; the job view
automatically reflects the correct state.

**Motivation**: repeated bugs where grace expiry, bootstrap timeout, and
resubmit code paths each need to set the right combination of attempt
status, cloud_outcome, and job status. Each new edge case is whack-a-mole.
