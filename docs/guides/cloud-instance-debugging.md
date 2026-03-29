# Cloud Instance Debugging

Troubleshooting guide for cloud GPU instance launches, whether via
`weft instance launch`, `weft campaign launch`, or the TUI cloud menu.

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
| `infra_failure` | Instance never became ready, bootstrap stalled, or provider died | Yes |
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

If no log file exists for a job, the instance died before the job started.

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

The agent uploads status markers and logs to R2:

```bash
# List grace-period control files
rclone ls <r2-remote>:weft-results/grace/<provider_id>/

# Check campaign manifest
rclone cat <r2-remote>:weft-results/campaigns/<instance_id>/manifest.json
```

## Common failure patterns

### Instance never became ready (`infra_failure`, `ready_at` is NULL)

The Vast.ai machine failed to boot or the Docker container didn't start.
The reconciler detected empty or stuck "created" provider status and
terminated the instance after a timeout.

**Timeouts:**
- Empty provider status: 1 minute (`maxEmptyStatusTime`)
- Stuck in "created" status: 5 minutes (`maxCreatedStatusTime`)
- Bootstrap stalled (running but no progress): adaptive, default 15 minutes
- Setup phase stalled (`uv sync`, etc.): adaptive, default 15m warn / 25m terminate

**Action:** These are retried automatically. If retries are exhausted
(default: 3 attempts per job), try `weft instance launch` again or use
the `r` key in the watch TUI to force a manual retry with extra attempts.

### Jobs exceeded max retry attempts

After 3 cloud attempts per job (default), auto-retry stops. The watch TUI
shows "N job(s) exceeded max cloud attempts, giving up".

**Action:** Press `r` in the watch TUI to add extra retry attempts, or
re-launch with `weft instance launch`.

### Disk full during execution

The instance ran out of disk. This usually means undeclared HF model
dependencies inflated actual disk usage beyond the estimate.

**Action:** Ensure all HF models are declared with `--input hf:<model-id>`.
See `docs/guides/workflow-guide.md` § "Declaring data dependencies".

### Bootstrap stalled

The instance started but the agent never began executing jobs. Usually
caused by slow Docker image pulls, network issues during `uv sync`, or
rclone configuration problems.

**Action:** Check if the agent binary was stale (`just build` rebuilds
agents). Check R2 for bootstrap script artifacts.

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

## Debugging with preserved working directories

By default, the agent deletes completed jobs' working directories in the
background to free disk space for subsequent jobs. To keep working
directories intact for debugging:

```bash
# Via campaign launch flag
weft campaign launch --skip-workdir-deletion

# Via instance launch
weft instance launch --skip-workdir-deletion
```

This preserves `.venv`, source files, and intermediate outputs on the
instance after each job completes, allowing SSH inspection of the
instance state. Uploads and job concurrency behavior are unaffected —
only the post-upload directory deletion is skipped.
