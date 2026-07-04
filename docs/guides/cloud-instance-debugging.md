# Cloud Instance Debugging

Troubleshooting guide for cloud GPU instance launches, whether via
`weft instance launch`, `weft start instance`, or the TUI cloud menu.

## Exit report

After a watch session ends, weft prints a summary report:

```
All rentals terminated.

  INSTANCE  GPU                    STATUS     UPTIME  COST   REASON
  ...                                                        (7 earlier attempts)
  217       RTX 4090 (4090 ≥20GB)  failed     12m3s   $0.02  infra_failure
  218       RTX 3090 (3090 ≥20GB)  failed     8m42s   $0.01  infra_failure
  220       RTX 3090 (3090 ≥20GB)  failed     21m8s   $0.04  infra_failure

  JOB  STATUS    INSTANCE  PROJECT              DESCRIPTION
  403  orphaned  220       head-type-ontology   EXP-018: Lambda Pareto sweep (requeue, needs >=20GB)
  404  orphaned  220       head-type-ontology   EXP-019: Multi-seed type regularization (requeue, ...)

  Total cost: $0.07
```

### Header

| Header | Meaning |
|--------|---------|
| **All rentals completed.** | Every instance finished successfully |
| **All rentals terminated.** | All instances reached a terminal state, but some failed |
| **Rental summary:** | Some instances are still running |

### Instance table

Shows all cloud instances that attempted the watched jobs, not just the
current session. Historical instances (from prior attempts) are included,
capped at the 5 most recent. If more exist, a summary line shows how many
were omitted.

| Column | Meaning |
|--------|---------|
| INSTANCE | Cloud instance ID (from local DB) |
| GPU | GPU spec (resolved model + search constraint) |
| STATUS | `running`, `completed`, `failed`, `canceled`, `grace` |
| UPTIME | Wall-clock time from launch to end |
| COST | Estimated spend for this instance |
| REASON | Termination reason (see below) |

### Termination reasons

| Reason | Meaning | Retryable? |
|--------|---------|------------|
| `completed` | All jobs finished successfully | No |
| `provider_failure` | Provider-side instance termination | Yes |
| `infra_failure` | Instance never became ready, bootstrap stalled, provider died, or provider delivered less disk than weft requested (phase `infra-failure:disk-cap`) | Yes |
| `job_failure` | A job exited with a non-zero code | No |
| `disk_full` | Ran out of disk space during execution | No |
| `canceled` | User canceled the instance | No |

### Job table

| Column | Meaning |
|--------|---------|
| JOB | Job ID |
| STATUS | `completed`, `failed`, `orphaned` (instance died before job finished) |
| INSTANCE | Which instance the job was last assigned to |
| PROJECT | Workspace/project name (derived from job directory) |
| DESCRIPTION | Job description, truncated to terminal width |

## Investigating a failed instance

### 1. Check instance status

```bash
weft instance status <id>
```

Shows instance metadata, timing, cost, and per-job outcomes including upload
times and failure messages.

### 2. Check the local operations log

```bash
# Find all log entries for the instance
grep '<instance_id>\|<provider_id>' ~/.cache/weft/operations.log
```

Key operations to look for:
- `cloud.instance.launch.requested` — instance was planned with specific jobs and disk budget
- `cloud.instance.launch.created` — Vast.ai accepted the creation request
- `cloud.instance.launch.readback` — SSH endpoint assigned
- `cloud.instance.launch.failed` — launch failed (with error details)

### 3. Check job logs

Job logs are stored locally after download:

```bash
ls ~/.cache/weft/logs/<job_id>.*
cat ~/.cache/weft/logs/<job_id>.log      # stdout/stderr
cat ~/.cache/weft/logs/<job_id>.log.meta # metadata (exit code, timing)
```

If no local log file exists for a job, first check the job state with
`weft status <job-id>` and retry `weft log <job-id>` after a sync. For a running
cloud job, missing local/R2 logs can mean the live log has not uploaded yet. For
a terminal job, absence of a log is evidence that the instance may have died
before the job started or before log upload completed.

### 4. Check the Vast.ai provider

```bash
# Human-readable (may crash for destroyed instances)
vastai show instance <provider_id>

# Raw JSON (safer for destroyed instances)
vastai show instances --raw | python3 -c "
import json, sys
for i in json.load(sys.stdin):
    if str(i['id']) == '<provider_id>':
        print(json.dumps(i, indent=2))
"
```

Note: `vastai show instance` crashes with `TypeError: 'NoneType'` for
destroyed instances where `start_date` is null. Use the `--raw` + JSON
parsing approach instead.

### 5. Check R2 for agent artifacts

The agent uploads status markers, logs, and per-sample telemetry to R2.
Per-job telemetry (CPU, RSS, GPU, **disk free/total**, GPU temp) is captured
on every sample tick and stored on R2; it is not imported into the local
DB. Fetch it from R2 when you need to investigate a specific run.

**Per-instance keys** (replace `<id>` with the launch ID):

```
instance/<id>/disk-failure.json     # ENOSPC report (df, top dirs, HF cache)
instance/<id>/agent-startup.json    # bootstrap context
instance/<id>/heartbeat             # last-seen timestamp
instance/<id>/maintenance.json      # provider maintenance signals
instance/<id>/opslog.jsonl          # agent operations log
instance/<id>/phase                 # current lifecycle phase
instance/<id>/termination-intent.json
```

**Per-job-run keys** (replace `<job>` and `<run>`):

```
jobs/<job>/runs/<run>/telemetry/timeseries.jsonl       # durable raw per-sample timeseries
jobs/<job>/runs/<run>/timeseries.jsonl                 # live checkpoint while running
jobs/<job>/runs/<run>/results/<job>.telemetry.jsonl    # finer-grained
jobs/<job>/runs/<run>/results/<job>.log                # stdout/stderr
jobs/<job>/runs/<run>/live-log/part-NNNNNN.log         # live tail
```

**Fetching:**

```bash
# Pretty-print the disk-failure report (when termination_reason=disk_full).
weft instance disk-report <id>

# Per-job timeseries summary (peak disk used, RSS, GPU mem/util, etc.).
weft job timeseries <job-id>            # high-water-marks summary
weft job timeseries <job-id> --raw      # full JSONL stream
weft job timeseries <job-id> --run <id> # specific attempt

# Stream raw telemetry for a specific job run via rclone if you prefer.
rclone cat <r2-remote>:weft-results/jobs/<job>/runs/<run>/telemetry/timeseries.jsonl
```

The `disk-failure.json` report is the most direct evidence for `disk_full`
investigations: it includes `df -h` output, top-level directory sizes
(`huggingface`, `uv_cache`, `workspace`), and a per-asset HF cache inventory.
Cross-check the recorded `df` total against `launches.disk_gb` to detect
silent provider-side disk caps.

**List grace-period control files:**

```bash
rclone ls <r2-remote>:weft-results/grace/<provider_id>/
```

## Common failure patterns

### Instance never became ready (`infra_failure`, `ready_at` is NULL)

The Vast.ai machine failed to boot or the Docker container didn't start.
The reconciler detected empty or stuck "created" provider status and
terminated the instance after a timeout.

**Timeouts:**
- Empty provider status: 1 minute (`maxEmptyStatusTime`)
- Stuck in pre-running provider status (`created`, `loading`, etc.): 5 minutes (`maxPreRunningStatusTime`)
- Bootstrap stalled (running but no progress): adaptive, default 15 minutes
- Setup phase stalled (`uv sync`, etc.): adaptive, default 15m warn / 25m terminate

**Action:** These are retried automatically. If retries are exhausted
(default: 3 attempts per job), try `weft instance launch` again or use
the `r` key in the watch TUI to force a manual retry with extra attempts.

### Jobs exceeded max retry attempts

After 3 cloud attempts per job (default), auto-retry stops. Retries can also
stop earlier when retry budget limits are reached (time/cost, whichever comes
first). The watch UI shows either "exceeded max cloud attempts" or
"exceeded retry budget".

**Action:** Press `r` in the watch TUI to add extra retry attempts (still
subject to retry budget), re-launch with `weft instance launch`, or adjust
`[campaign] retry_*_time_limit` / `retry_*_cost_limit` in
`~/.config/weft/config.toml`.

### Auto-relaunch blocked by runaway breaker

While the autopilot is enabled, Weft can halt relaunches when it detects repeated
no-progress churn (for example, repeated orphaned retries and spend growth with
no completions in the same campaign/project scope).

You will see status text like `auto-relaunch blocked` or `runaway breaker`.

**Action:** inspect recent lifecycle events, fix the underlying job/runtime
issue, then resume relaunch:

```bash
weft log --events --kind relaunch.runaway
weft campaign safety resume --campaign <campaign-id> [--project <project-name>]
```

### Disk full during execution

The instance ran out of disk. This usually means undeclared HF model
dependencies or runtime setup caches inflated actual disk usage beyond the
estimate.

**Action:** Ensure all HF models are declared with `--input hf:<model-id>`.
For setup-heavy jobs, add `--runtime-disk <gb>` for wheel/cache/build headroom
or `--disk <gb>` for a total rental disk floor. See
`docs/guides/workflow-guide.md` § "Declaring data dependencies".

Run `weft instance disk-report <id>` to see the agent's snapshot at failure
(df, top directories, HF cache breakdown). Cross-check the recorded
`recorded_disk` (what weft requested) against the `df` total — if they
disagree, the provider silently delivered a smaller disk than requested
and the failure is closer to `infra_failure` than over-provisioning. New
instances catch this proactively at boot via the disk-cap probe; pre-fix
runs need this manual check.

### Torch driver too old (`cuda_driver_too_old`)

Job fails seconds into the run with:

```
RuntimeError: The NVIDIA driver on your system is too old (found version NNNNN).
```

`NNNNN` is the driver's CUDA API version × 1000 (e.g. `12040` = driver
supports CUDA 12.4). The resolved torch wheel needs a newer CUDA runtime
than the rental's driver provides. Common cause: a dependency (extra,
upstream package, transitive pin) leaves torch unpinned, so `uv sync` on
the rental resolves to the latest torch — currently shipping `cu128`
wheels that require a driver supporting CUDA ≳12.8. Older Vast.ai hosts
still run drivers in the 12.2–12.4 range.

Weft already classifies this signature. The runner's live log scanner
(`internal/runner/single.go`) treats it as `fatalAtRuntime` and SIGTERMs
the job rather than letting it spin. The remediation path then
writes an `error_diagnosis` row with pattern `cuda_driver_too_old` and
the driver's inferred CUDA compatibility, which surfaces in:

- `weft status <id>` — "Diagnosis:" + "Solution:" lines
- `weft log <id>` — appended `--- diagnosis ---` footer
- `weft list` — `(diagnosed)` indicator on the failed row

**Diagnosis commands:**

```bash
weft status <job-id>                        # show diagnosis + solution
weft instance ssh <id> -- nvidia-smi | head -3
weft instance ssh <id> -- "cd /workspace && uv pip show torch | head -3"
```

Cross-reference the torch wheel tag (`cu118` / `cu121` / `cu124` / `cu128`)
against the driver's CUDA compatibility.

**Recovery (instance is in grace period, default 5m):**

1. **Pin torch and resubmit to the same instance** — reuses the rental,
   skips another launch + `uv sync` cycle:
   ```bash
   # edit pyproject.toml to pin a compatible torch (e.g. torch==2.4.1)
   weft instance extend <id> 15m
   weft instance submit <id> <new-job-id>
   ```
2. **Release and relaunch** — let Vast pick a different (often
   newer-driver) host:
   ```bash
   weft instance release <id>
   weft start instance --gpu nvidia>=24GB ...
   ```

**Prevention (before submit):**

- **Pin torch in your project.** Add `torch==<version>` to
  `[project.dependencies]` and run `uv lock`. Weft's `ScanTorchPin()`
  reads the lock and auto-derives both `gpu-arch-max` (compute capability
  cap) and `cuda-driver-min` (driver floor from the wheel's `cuXXX` tag).
  Vast.ai offers are then filtered by `cuda_vers>=X.Y` at search time.
- **If the project can't pin torch** (the script orchestrates an isolated
  venv that resolves torch on the rental — see workflow-guide.md
  § "Pinning torch inside an isolated venv"), declare the floor in the
  script's PEP 723 block:
  ```python
  # [tool.weft]
  # cuda-driver-min = "12.4"   # matches the cu124 wheel the venv installs
  ```
  This is the **permanent** property of the script and should ride with
  it. Use `--cuda-driver-min 12.4` on `weft run` only for ad-hoc
  overrides.
- **`gpu-arch-max` and `cuda-driver-min` are different axes.**
  `gpu-arch-max` catches "torch wheel was built for sm_90 but this GPU
  is sm_80". `cuda-driver-min` catches "torch needs CUDA runtime 12.8 but
  driver only supports 12.4". Both fire automatically from a project
  torch pin; both can be set explicitly when the auto-derivation can't
  see the relevant torch.

### Disk request implausibly large (`infra_failure`, "no instances available with enough disk space")

Symptom: every fresh launch fails almost instantly with
`offer unavailable: pod create --gpu-id: There are no longer any instances
available with enough disk space.` (RunPod phrasing — Vast surfaces the
same condition differently). Inspecting `weft instance list` shows
`launches.disk_gb` is wildly large (hundreds of TB / PB).

This is the disk estimator amplifying a single bogus telemetry sample
from a prior run. The agent-side `statfs` probe on overlay/fuse
container roots used to multiply by `stat.Bsize` instead of
`stat.Frsize`, over-reporting the filesystem by orders of magnitude. The
probe is fixed in current builds, but old `job_phase_timings` /
`job_timeseries` rows from before the fix can still poison
`EstimateGroupDisk` for jobs with matching command signatures.

**Action:** Run `weft job anomalies` — there's a "Disk telemetry
anomalies" section listing every prior job whose recorded disk reading
exceeds the 2 TB plausibility bound. If the current estimator is
seeing a bogus sample, it surfaces a one-line note prepended to
`placement_reasons` (visible in the TUI and `weft job diagnose`) such
as `skipped anomalous historical disk reading from wjN (231.1TB > 2.0TB
plausibility bound) — likely statfs Bsize-vs-Frsize bug; see weft job
anomalies`. The estimator then falls back to the input-based size, so
the next placement attempt will request a reasonable disk and the
provider's stockout error should go away.

There is no bulk-cleanup tool by design — natural decay of the
poisoned rows is preferred over a silent mass-delete that would also
remove the evidence trail.

### Provider delivered less disk than requested (`infra_failure:disk-cap`)

At agent startup, weft probes `df` against the disk it asked the provider
to allocate (recorded as `launches.disk_gb`). If the actual mounted disk
is below 85% of the request, the agent uploads
`instance/<id>/disk-cap-failure.json` to R2, marks the instance
`infra_failure`, and self-destructs immediately rather than running for
hours and hitting ENOSPC.

This catches Vast.ai (and similar) silently capping `--disk N` requests on
multi-tenant hosts where another container occupies most of the host disk.

**Action:** Check the disk-cap-failure report:

```bash
rclone cat <r2-remote>:weft-results/instance/<id>/disk-cap-failure.json
```

The report shows `requested_disk_gb` vs `actual_total_bytes` and `df -h`.
If a specific machine repeatedly under-delivers, blacklist it via the
provider's machine-id allowlist or constrain to offers with more
host-disk headroom (`--disk` filters offers by `disk_space`, but the
container slice is a separate negotiation).

### Bootstrap stalled

The instance started but the agent never began executing jobs. Usually
caused by slow Docker image pulls, network issues during `uv sync`, or
rclone configuration problems.

**Action:** Check if the agent binary was stale (`just build` rebuilds
agents). Check R2 for bootstrap script artifacts.

### OnStart died before bootstrap.sh ran (`infra_failure`, no R2 markers)

Symptom: launch hits `empty_status_timeout` after 25 minutes; R2 has only
`instance/<id>/agent-version` (written when the instance is created)
and **no** `bootstrap/<id>/stage` marker. The agent never wrote anything.

This means Vast's `--onstart-cmd` chain failed before reaching
`bash /tmp/bootstrap.sh`. The chain installs apt packages and downloads
uv + rclone via `curl`. Any one of those failing — DNS hiccup on
`archive.ubuntu.com`, `astral.sh`, or `rclone.org`, or a transient apt
mirror outage — used to abort the chain via `set -e`. Vast then destroyed
the rental and weft only noticed at the watchdog.

Mitigation in `internal/cloud/types.go:DefaultOnStartCmd`: each install
step is now skipped when the binary is already on `PATH`, and otherwise
retried up to three times with backoff. Pre-baked images
(`deploy/cloud-base.Dockerfile`) make every step a no-op once published.
Hard requirement: `rclone` must end up installed; if all retries fail
the command exits non-zero and the rental is reaped immediately rather
than wedging.

To diagnose a future occurrence:

```bash
# Was bootstrap.sh reached at all?
rclone cat <r2-remote>:weft-results/bootstrap/<id>/stage

# Empty/missing → OnStart failed before downloading bootstrap.sh.
# Anything else → bootstrap.sh started; the value names the last stage
# completed (e.g. agent_installed, sources_extracted, deps_installing).
```

### First worker registration stalled

The campaign was created, but no worker launch row was registered
(`launches.created_at`) before the first-registration deadline.

This uses a survival model over historical
`time_to_first_registration = first_worker_launch.created_at - campaign.created_at`.
When no registration has happened yet, we condition on elapsed time (tail
truncation) to estimate:

- conditional success probability `P(register eventually | not yet by elapsed)`
- median remaining time from the truncated successful tail
- adaptive warn/terminate thresholds

Scope selection is:

1. provider + data center
2. provider
3. global

When a narrower scope has too little data, weft falls back automatically.

By default, this detection is enforced during launch: if elapsed reaches the
learned terminate threshold with zero worker registrations, the launch path
auto-fails the campaign.

**Action:** Inspect provider/API health and cloud offer availability; if this
repeats in one region, force a different region/provider and re-launch.

### Setup phase stalled

The agent started the setup phase (e.g., `uv sync`) for a job but
never transitioned to the running phase. The reconciler detects this
via the R2 instance phase marker and phase timing data.

Thresholds are **adaptive**: learned from historical setup durations
for the same command and workspace using survival analysis, with
fallback to all-jobs statistics when per-command data is insufficient
(< 20 samples). Default thresholds: warn at 15 minutes, terminate at
25 minutes.

**Action:** The instance is auto-terminated and jobs are reset to
queued for retry. If setup stalls recur for a specific project,
investigate the setup command (e.g., network issues during package
installation, pip/uv resolution hangs).

## When the wallet ran out

Vast.ai destroys running rentals and starts rejecting new launches with
"insufficient credit" once the account balance hits zero. Weft cannot
distinguish runtime credit-destroys from generic provider failures at
the moment they happen, so the affected launches land with
`termination_reason = provider_failure` or `infra_failure` — the same
labels used for genuine machine flakes. Leaving them mislabeled poisons
the bidding survival model with phantom "machine X is unreliable"
signals.

After topping up, reclassify the affected launches:

```bash
# 1. See what the auto-detector would label, without writing anything.
weft instance mark-credit-exhausted --auto --dry-run

# 2. If the proposed window and members look right, apply it.
weft instance mark-credit-exhausted --auto
```

The auto-detector finds the most recent **provider-scoped** burst of
failures followed by a silent gap (no same-provider instance reached
running) and a recovery launch. It will:

- **Refuse** to declare an incident if a same-provider instance reached
  running within the candidate silence window — that pattern is a
  regional outage, not a wallet event, and reclassifying would
  mislabel real machine failures.
- **Refuse** if the gap to the next same-provider recovery was shorter
  than `--auto-min-silence` (default 90s) — that's typically a
  transient cluster.
- **Wait** if the burst is still in progress (silence shorter than the
  floor with no recovery yet). Re-run after another minute.

A clean detection looks like:

```
Detected credit-exhaustion incident on vastai:
  Burst:    6 instances destroyed within 6m53s, peak 2026-06-01 08:24:04 CST
  Silence:  5m1s (no vastai instance reached running)
  Recovery: 2026-06-01 08:29:05 CST  (wi3419)
  Window:   2026-06-01 08:02:11 CST → 2026-06-01 08:29:05 CST
  ...
```

The detector intentionally includes failures whose detail strings have
no credit signature (e.g. `agent heartbeat stale`) when they fall
inside the burst window — those are typically running rentals Vast
killed at the moment the wallet hit zero, before the agent could
report the cause.

If the auto-detector misses an older incident (it only returns the
most recent qualifying burst), fall back to `--since DURATION` with
signature filtering, or name affected IDs explicitly. See
[`weft instance mark-credit-exhausted`](../reference/commands.md#weft-instance-mark-credit-exhausted)
for full flag reference.

## Debugging with preserved working directories

By default, the agent deletes completed jobs' working directories in the
background to free disk space for subsequent jobs. To keep working
directories intact for debugging:

```bash
# Via instance launch flag
weft start instance --skip-workdir-deletion

# Via instance launch
weft instance launch --skip-workdir-deletion
```

This preserves `.venv`, source files, and intermediate outputs on the
instance after each job completes, allowing SSH inspection of the
instance state. Uploads and job concurrency behavior are unaffected —
only the post-upload directory deletion is skipped.
