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

## Extend derived-status model to on-prem jobs

Cloud jobs now derive display status from attempt facts + instance lifecycle
(no mutable status mutations). On-prem jobs still use the three-way merge
(`status`/`pending_status`/`last_synced_status`).

### Remaining work

1. Derive on-prem job status from attempt facts the same way cloud jobs do
2. Replace the three-way merge with a simpler model: the remote queue runner
   writes `start_time`/`end_time`/`exit_code` directly, and the view derives
   the display status
3. Remove `pending_status` and `last_synced_status` once all paths use the
   derived model
4. Remove `cloud_outcome` column (no longer needed — the view derives
   orphaned/failed/completed from attempt facts + instance status)
