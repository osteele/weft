# SkyPilot External Executor Integration Plan

Status: **Plan** — describes intended design, not current implementation.

## Decision

Use Weft as the local ledger and agent/human front door, and use SkyPilot as an
external executor. This is the shallow integration path: Weft records jobs,
project association, tags, processed bookkeeping, and query surfaces; SkyPilot
owns cloud resource selection, cluster lifecycle, retry/recovery, and raw task
execution.

This replaces the earlier "SkyPilot as provider facade" idea. SkyPilot is a
scheduler/control plane, not a low-level provider like Vast.ai or RunPod, so
deep provider integration would create lifecycle ownership conflicts.

See also: `docs/design/comparison-to-skypilot.md`.

## Goals

- Give agents one reliable habit: submit, inspect, watch, and mark processed
  through Weft.
- Include SkyPilot jobs in `weft jobs list --unprocessed`, grouped status
  views, and project-scoped views.
- Preserve local Weft job IDs (`wj...`) as the canonical address for imported
  and Weft-submitted SkyPilot jobs.
- Keep SkyPilot lifecycle ownership explicit instead of pretending SkyPilot
  jobs are Weft-managed rental instances.
- Make the first implementation useful without importing Weft telemetry, R2
  output streaming, grace periods, or placement scoring.

## Non-Goals

- Do not make SkyPilot a native `internal/campaign` provider in the first
  version.
- Do not create `Launch` / `CloudInstance` rows for SkyPilot jobs.
- Do not run the Weft Go agent inside SkyPilot clusters in the first version.
- Do not promise Weft R2 output collection, per-run telemetry, Weft grace
  periods, or survival-model placement for external jobs.
- Do not use SkyPilot labels or task state as the authoritative processed
  marker. Processed bookkeeping remains a Weft-local tag.

## User Model

The user or coding agent should be able to do this:

```bash
weft sky submit --gpu a100 'python train.py'
weft sky sync
weft jobs list --unprocessed
weft project watch
weft log wj123
weft mark-processed wj123
```

An existing SkyPilot job can be imported:

```bash
weft sky import --project my-project --command "python train.py" <sky-job-id>
```

After import, the job is addressed as `wj...` in Weft. The external SkyPilot ID
is detail, not the primary user-facing identity.

## Data Model

Add an external binding table:

| Field | Purpose |
| --- | --- |
| `job_id` | Local Weft job identity |
| `attempt_id` | Optional local attempt row for Weft-submitted external jobs |
| `executor` | `skypilot` |
| `external_job_id` | SkyPilot job/request identifier |
| `external_task_id` | Optional SkyPilot task identifier |
| `external_cluster_id` / `external_cluster_name` | Cluster context for logs/status/UI |
| `raw_status` / `raw_status_message` | Last observed SkyPilot status |
| `normalized_status` | Weft status bucket |
| `submitted_from_working_dir` / `submitted_from_project` | Project-scoping provenance |
| `dashboard_url` | Optional SkyPilot dashboard/API link |
| `cancel_requested_at` | Secondary pending-cancel state; never authoritative job status |
| `created_at` / `last_observed_at` | Local bookkeeping |

Idempotency key: `(executor, external_job_id, external_task_id)`. A unique
`job_id` constraint also guarantees that one Weft job has one live binding;
an initially unqualified task identity is refined in place.

Use `jobs.backend = "skypilot"` and `jobs.target_kind = "external-executor"` for
these rows. They are not `rental-instance` jobs.

## Status Mapping

The first adapter should normalize coarsely:

| SkyPilot state family | Weft status |
| --- | --- |
| pending, submitted, starting, provisioning, recovering | queued |
| running | running |
| succeeded, completed | completed |
| failed | failed |
| cancelling, canceling | running |
| cancelled, canceled | canceled |
| stopped, killed | killed |

Unknown non-terminal states should remain `queued` with the raw status visible in
`weft job info`. Unknown terminal/error states should become `failed`.

If SkyPilot cannot be reached during sync, leave the Weft status unchanged and
record a staleness warning. Do not infer failure from an unavailable SkyPilot
query.

## CLI Surface

Phase 1 commands:

- `weft sky submit [weft run-like flags] <command>`: create the Weft row first,
  submit to SkyPilot in detached-run mode, then store the external binding.
- `weft sky import [--project NAME] [--cwd DIR] [--command COMMAND]
  <external-id>`: mirror an existing SkyPilot job into Weft. Current SkyPilot
  queue JSON omits the task command, so a new import requires `--command`;
  `--job` recovery retains the command Weft recorded before submission.
- `weft sky sync [--project NAME]`: refresh external bindings.
- `weft log <wj-id>`: for `backend=skypilot`, call SkyPilot logs through the
  complete job/task binding; snapshot mode is explicitly non-following and
  ordinary tail/follow views push the line bound to SkyPilot.
- `weft kill|cancel <wj-id>`: for `backend=skypilot`, call SkyPilot cancel and
  wait for a later sync to confirm terminal status. Since cancellation is
  managed-job-wide, a task-qualified request requires a fresh observation
  proving there are no sibling tasks.

Existing surfaces should include these jobs without separate commands:

- `weft jobs list --unprocessed`
- `weft jobs list --project NAME`
- `weft project watch [NAME]`
- grouped status TUI
- legacy dashboard TUI (ambient refresh, logs, and cancellation use SkyPilot;
  host SSH/R2 monitoring is skipped)
- `weft job info <wj-id>`

## TUI / Display

SkyPilot jobs appear in the normal grouped status buckets after normalization.
They must not appear as Unplaced just because Weft has no Host or Launch row.

Selected-job detail should render an external-executor host line such as:

```text
Host: SkyPilot - cluster <name> - raw <status>
```

The display must not imply a Weft-managed instance, Weft cost meter, grace
period, R2 phase, or agent telemetry unless a future adapter actually imports
those signals.

## Implementation Phases

1. **Schema and DB access**
   - Add `skypilot` backend and `external-executor` target kind.
   - Add `external_job_bindings`.
   - Add idempotent upsert/list/get helpers.

2. **Read/import path**
   - Implement `weft sky import`.
   - Implement `weft sky sync`.
   - Add status normalization tests.
   - Include external jobs in project and unprocessed queries.

3. **Display path**
   - Teach grouped/flat job lists to render external executor targets.
   - Add `weft job info` external binding details.
   - Add stale-sync warnings.

4. **Submit path**
   - Implement `weft sky submit`.
   - Generate a SkyPilot task from a conservative subset of Weft run flags:
     command, working directory/source mount, GPU class/count, env, and
     description.
   - Store the Weft job before invoking SkyPilot.

5. **Operations**
   - Route `weft log` through SkyPilot for external jobs.
   - Route `weft cancel` through SkyPilot for external jobs.
   - Add watch/project-watch refresh hooks for external sync.

6. **Hardening**
   - Add duplicate-import protection.
   - Add read-only fallback behavior when sync cannot write the DB.
   - Add fixture-based tests for representative
     `sky jobs queue --all --verbose --output json` payloads.
   - Document unsupported features clearly in `weft job info`.

The implemented boundary also rejects Weft-local retry/requeue, status edit,
move/unplace, launch-new, draft, pause, resume, and artifact collection for
external jobs. These operations would otherwise create false local state or
duplicate execution without changing SkyPilot.

## Open Questions

- Which SkyPilot interface should the first adapter use: CLI JSON output, API
  server, or a thin Python helper?
- How stable are SkyPilot job IDs across managed-job recovery and cluster
  replacement?
- What source packaging model is safest for `weft sky submit`: SkyPilot
  `file_mounts`, working-tree archive, or a minimal command wrapper?
- Should Weft import SkyPilot cost estimates when available, and how should
  those be distinguished from Weft's direct rental cost meter?
- Should imported jobs default to the current directory's project, or require
  `--project` unless SkyPilot metadata already records one?

## Later Deepening

If SkyPilot-backed execution becomes a high-volume path, reconsider deeper
integration only after the shallow ledger path is stable. Candidate later work:

- Import SkyPilot cost and dashboard links.
- Import logs into Weft's local log cache.
- Add optional artifact import from SkyPilot storage mounts.
- Run a Weft agent inside SkyPilot tasks for telemetry/R2 output collection.
- Compare Weft direct providers and SkyPilot offers in placement. This should be
  a separate design, because it reopens lifecycle ownership and cost-model
  questions.
