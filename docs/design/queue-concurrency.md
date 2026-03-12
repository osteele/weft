# Queue Runner Concurrency (MVP Spec)

This document defines the MVP design for running multiple queued jobs per host
while keeping total CPU usage under a target cap. It adds preset-based CPU
allotments, adaptive tuning with hysteresis and decay, and warm-up gating.

## Goals

- Run multiple jobs per host while keeping total CPU utilization under a cap.
- Allow user intent via simple presets in the TUI (no CLI editing).
- Adapt local allotments based on observed usage (increase + decay).
- Avoid launching additional jobs while any running job is warming up.
- Preserve queued job order; do not skip ahead when a job cannot start.

## Terms

- **Requested allotment**: Percent of host CPU requested by the user (DB field).
- **Default allotment**: Preset used when a job has no requested allotment.
- **Local allotment**: Runner-side effective allotment for scheduling.
- **Observed utilization**: Average CPU percent over a rolling window.
- **Warm-up**: Initial window after job start where the runner does not start
  additional jobs.

## Constants (MVP Defaults)

- `HOST_UTILIZATION_TARGET = 80`
- `DEFAULT_ALLOTMENT_CORES = 7` (percent computed per host)
- `PRESETS = [20, 40, 60, 80]`
- `WARMUP_DURATION = 120s`
- `SAMPLE_INTERVAL = 15s`
- `SAMPLE_WINDOW = 60s` (4 samples)
- `HYSTERESIS_WINDOW = 5` (windows)
- `HYSTERESIS_THRESHOLD = 3` (windows)
- `INCREASE_STEP = 10`
- `DECAY_STEP = 10`
- `MIN_ALLOTMENT = 10`
- `MAX_ALLOTMENT = 100`

These should live in a single Go config module and be exported for the TUI
help text and the runner bootstrap.

## Data Model

### DB: jobs table

Add nullable `cpu_allotment` integer column (percent of host CPU).
- `NULL` means the user did not set a value; default applies.
- `0` is not used; use `NULL` to indicate "unset".

Job struct field:
- `CPUAllotment *int` (nil when unset).

### Queue command payload

Extend the queue command `job` payload to include:

- `cpu` (nullable int) — requested allotment from DB.

### Runner state

Extend queue state JSON to track concurrent jobs and tuning:

```
{
  "cursor": "...",
  "cursor_line": 123,
  "pending": [1,2,3],
  "running": {
    "42": {
      "started_at": 1700000000,
      "warmup_until": 1700000120,
      "local_allotment": 60,
      "samples": [55, 62, 64, 58],
      "over_count": 3,
      "under_count": 0
    }
  }
}
```

Use numeric job IDs as object keys for stable lookup.

## Scheduling Rules

### Start eligibility

A new job may start only if all are true:

1. No running job is in warm-up (`now < warmup_until` for any job).
2. `sum(local_allotments) + initial_allotment <= HOST_UTILIZATION_TARGET`.
3. The job at the head of the pending queue is ready (dependencies satisfied).

Do not skip ahead in the queue when the head cannot start.

### Initial allotment

When starting a job:

- If `cpu_allotment` is set in DB: use it as `local_allotment`.
- Else: use `DEFAULT_ALLOTMENT_CORES` converted to a host percent.

### Warm-up

When a job starts:

- Set `warmup_until = now + WARMUP_DURATION`.
- While any job is warming up, do not start additional jobs.

## Utilization Sampling

### Measurement

For each running job, compute CPU utilization from the job’s PID using
`ps -p <pid> -o %cpu=` and average over `SAMPLE_WINDOW`.

### Hysteresis for increases

A job’s `local_allotment` increases by `INCREASE_STEP` if:

- Not warming up, and
- `observed > local_allotment`, and
- This condition held for `HYSTERESIS_THRESHOLD` of the last
  `HYSTERESIS_WINDOW` windows.

### Decay for decreases

A job’s `local_allotment` decreases by `DECAY_STEP` if:

- Not warming up, and
- `observed < local_allotment - 10`, and
- This condition held for `HYSTERESIS_THRESHOLD` of the last
  `HYSTERESIS_WINDOW` windows.

### Capacity enforcement

If total local allotment exceeds the host target, do not preempt. Only block
additional starts until total is below the target.

## Propagating TUI Updates

When `cpu_allotment` is changed in the DB:

- The runner applies it immediately to `local_allotment`.
- Reset hysteresis counters and sample buffers for that job.

## TUI Behavior

- Add a CPU allotment preset picker in edit mode (queued jobs only).
- Display both percent of host and approx cores (`CPUs * percent / 100`).
- Store `NULL` when the user selects "Default".
- No CLI flag or command to set the allotment.

## Implementation Map (code references)

### DB + Job struct

- Add column and migration in `internal/db/db.go`.
- Update `Job` struct and `jobSelectColumns` in `internal/db/db.go`.
- Update read/write helpers in `internal/db/db.go` (record/update/list).

### Queue command payload

- Extend `CommandJob` in `internal/ops/commandqueue.go`.
- Populate `cpu` in `NewAddCommand` (from DB job).

### Queue runner

- Extend state format in `internal/ops/commandqueue.go` (RunnerState).
- Update Go agent (`internal/runner/`):
  - Track multiple running jobs and per-job state.
  - Add warm-up gating before starting a new job.
  - Periodically sample CPU and update local allotments (hysteresis + decay).
  - Start jobs in background while existing jobs continue to run.
  - Maintain `current` semantics for compatibility (use newest started job ID).

### TUI edit form

- Add CPU allotment control in `internal/tui/model.go` edit mode.
- Show presets and render % + cores using `Host.CPUs`.
- Read/write to DB using a new `db.SetJobCPUAllotment` helper.

### Sync and display

- Add read-only display fields in TUI job rows/details for requested and
  local allotment where available.
- Keep CLI read-only; no CLI setters.

## Migration Notes

- Existing jobs default to `NULL` allotment in DB; runner applies
  `DEFAULT_ALLOTMENT_CORES` (converted to percent per host) on start.
- No backfill required.

## GPU Concurrency Model

GPU jobs are scheduled **one per device**. Unlike CPU jobs, where multiple jobs
can share cores and the allotment system tracks utilization, GPU jobs get
exclusive access to their assigned device.

### Rationale

The constraint is **compute, not memory**. GPU workloads (training, inference,
benchmarks) typically saturate the device's SMs/warps/threads. Even if two jobs
fit in VRAM, they would contend for execution resources and both slow down.
There is no practical way to partition GPU compute across jobs the way OS
scheduling partitions CPU cores.

CPU jobs, by contrast, are often single-threaded or use a small number of cores
(a Python script, a data pipeline), so a multi-core host can run several
concurrently. The allotment system (above) manages this.

### Scheduling behavior

- `DeviceHasRunningJob` rejects any device that already has a running job.
- `PickBestGPUForClass` skips occupied devices when resolving `--gpu-class`.
- CPU-only jobs (no `gpu`, `gpu_class`, or `gpu_mem` fields) bypass the GPU
  gate entirely and are governed only by the CPU allotment rules.
- GPU jobs and CPU jobs can run concurrently on the same host, as long as they
  use different resources (GPU device vs CPU cores).

### Multi-tenancy and memory checks

On shared machines (e.g. atlas), other users may have jobs running on GPUs
outside our control. The one-job-per-device rule applies only to *our own* jobs
— we don't wait for a device to be completely vacant before starting, because:

- Other users don't coordinate with us and may start jobs at any time.
- They are just as likely to land on a GPU after we've started as before.
- Waiting for total vacancy on a shared machine would block indefinitely.

This is an intermediate cooperation stance: we avoid contending with our own
jobs (where we can control scheduling), but accept mutual slowdown with other
users' jobs on shared devices.

The memory tracking (`GPUMemGB`, `TotalGPUMemReserved`) supports this model —
it ensures we don't stack our own jobs' memory on a device, even though we
can't prevent external memory pressure.

## Testing Strategy (MVP)

- Unit tests for DB migration + CRUD (`internal/db/db_test.go`).
- Queue command serialization includes `cpu` (`internal/ops/commandqueue_test.go`).
- Script-level tests can be added to validate state transitions for multiple
  running jobs and warm-up gating.
