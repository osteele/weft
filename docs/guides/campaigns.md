# Cloud GPU Campaigns

Weft's campaign system launches batches of jobs on Vast.ai cloud GPUs when local
hosts can't satisfy GPU requirements.

## Terminology

| Term | Meaning |
|------|---------|
| **Campaign** | A batch of cloud instances launched together |
| **Instance** | A single Vast.ai deployment running one or more jobs |
| **Unplaced job** | A queued job with no host assigned (needs a rental GPU unless tagged `inventory`) |

## Quick start

```bash
# 1. Install Vast.ai CLI
pip install vastai
vastai set api-key YOUR_API_KEY

# 2. Configure R2 for result upload (in ~/.config/weft/config.toml)
# [vastai.r2]
# bucket = "my-results-bucket"
# account_id = "..."
# access_key_id = "..."
# secret_access_key = "..."

# 3. Queue jobs that need rental GPUs
weft run --gpu hopper+ -m "Train on H100" 'python train.py'
# Job accepted (unplaced — needs rental GPU)

# 4. Launch a campaign
weft campaign launch
```

RunPod is also supported for cloud search and launch. The equivalent setup flow
is:

```bash
brew install runpodctl
runpodctl doctor
weft runpod doctor
weft runpod setup
```

RunPod launch readiness depends on the shared R2 bootstrap config under
`[vastai.r2]` plus a compatible `runpod.bootstrap_template_id`. Use
`weft runpod template print-bootstrap` to inspect the exact startup command that
the managed template must run.

## Launching campaigns

### Interactive launch

```bash
weft campaign launch
```

The TUI groups unplaced jobs by GPU class, searches for Vast.ai offers in
parallel, and presents an interactive selector:

1. **Job selection**: All eligible jobs are pre-selected. Use `Space` to
   toggle individual jobs, `a` to select all, `n` to deselect all.
2. **Cost review**: A cost table shows the best offer per GPU group with
   estimated setup time and total cost.
3. **Launch**: Press `Enter` to create a campaign and provision instances.
   All instances are launched **in parallel** for faster startup.
4. **Watch**: As soon as the instance rows are registered, the TUI switches
   into the watch-style view so you can follow provisioning, bootstrap,
   uptime, and job progress without waiting for the full launch to finish.

### Non-interactive launch

For scripted or agent-driven workflows, use `--yes` to skip the TUI:

```bash
weft campaign launch --yes              # Launch all unplaced jobs
weft campaign launch --yes --jobs 42,43 # Launch specific jobs only
weft campaign launch --yes --no-watch   # Launch and exit (print IDs)
```

This fetches offers, prints a cost summary, creates a campaign, launches
instances in parallel, and (unless `--no-watch`) transitions to watch mode.

### Options

```bash
weft campaign launch --dry-run          # Preview plan without launching
weft campaign launch --no-watch         # Launch and exit (print IDs only)
weft campaign launch --max-spend '$10'  # Budget limit per instance
weft campaign launch --max-time 2h      # Time limit per instance
weft campaign launch --yes              # Skip TUI, launch all groups
weft campaign launch --jobs 42,43       # Filter to specific job IDs
weft campaign launch --min-survival 0   # Disable survival floor (allow all offers)
weft campaign launch --min-survival 0.6 # Stricter survival floor
```

## Monitoring campaigns

### Watch mode

```bash
weft campaign watch <campaign-id>       # Line-oriented (good for agents/pipes)
weft campaign watch --tui <campaign-id> # Interactive TUI
weft campaign watch                     # Watch the most recent campaign
```

Watch mode shows per-instance details:
- Vast.ai instance ID and status
- SSH connection command
- Uptime and estimated cost
- Job progress (completed/total)

Press `Ctrl-C` to exit watch mode. Instances continue running in the background.

### Campaign overview

```bash
weft campaign list                      # List all campaigns with instance counts
weft campaign show <campaign-id>        # Detailed view with instances and jobs
weft campaign stats                     # Aggregate instance statistics
weft campaign survival                  # Survival model: posteriors and machine penalties
weft campaign survival --floor 0.6      # Highlight entries below a stricter floor
```

## Managing instances

### Instance status

```bash
weft instance list                      # List all cloud instances
weft instance status <instance-id>      # Single instance details
```

### SSH access

```bash
weft instance ssh <instance-id>         # SSH into running instance
```

If the instance is still provisioning, the command waits until the Vast.ai
instance ID is available, then connects.

### Terminating

```bash
# Single instance
weft instance terminate <instance-id>

# Multiple instances (terminated in parallel)
weft instance terminate <id1> <id2> <id3>

# All instances in a campaign (terminated in parallel)
weft campaign terminate <campaign-id>
```

`cancel` is an alias for `terminate` on both commands:

```bash
weft instance cancel <id>
weft campaign cancel <id>
```

Termination destroys the Vast.ai instance, resets associated jobs to
unplaced (queued with no host), and updates the instance status to `canceled`.

## Grace period

Cloud instances have a default 15-minute grace period after job failures. Instead
of self-destructing immediately, the instance waits for you to fix the issue and
resubmit. This avoids repaying provisioning overhead (~100s startup + model
downloads).

### Launching with a grace period

```bash
# Default: 15-minute grace period
weft campaign launch

# Custom grace period
weft campaign launch --grace-period 30m

# No grace period (immediate self-destruct on failure)
weft campaign launch --grace-period 0
```

### Responding to failures

When a job fails on a grace-period instance, you can resubmit, extend, or
release:

```bash
# Resubmit the job with updated sources (re-syncs project files)
weft instance submit <instance-id> <job-id>

# Resubmit with a modified command
weft instance submit <instance-id> <job-id> --command 'python train.py --batch-size 16'

# Extend the grace period (default: +15m)
weft instance extend <instance-id> 15m

# Clean shutdown (self-destruct immediately)
weft instance release <instance-id>
```

### How it works

1. The agent wrapper captures exit codes from each job.
2. On failure, the wrapper invokes `weft-agent grace-wait` instead of
   self-destructing.
3. The agent polls R2 for control messages under `grace/<INSTANCE_ID>/`:
   - `jobs.json` — new job submission
   - `extend` — extend the deadline
   - `release` — clean shutdown
4. The CLI writes control messages to R2 when you run `instance submit`,
   `instance extend`, or `instance release`.
5. If the grace period expires with no action, the instance self-destructs.

The grace period is tracked in the database (`grace_period_seconds`,
`grace_started_at`, `grace_deadline`) and the instance status changes to
`grace` during the wait.

## TUI rental menu (single job)

For launching a single job without the full campaign flow, use the TUI:

1. Open `weft tui`
2. Navigate to an unplaced job (queued with no host)
3. Press `c` to open the rental GPU menu
4. Select a Vast.ai offer and confirm the cost

This creates a campaign with a single instance for that job.

## How it works

### Instance lifecycle

1. **Provisioning**: Weft creates a Vast.ai instance with the configured Docker
   image, disk, and SSH access.
2. **Setup**: Once the instance is running, weft deploys rclone configuration,
   the Go agent binary (`weft-agent`), and rsyncs project sources via SSH.
   Agent deployment and source sync run in parallel for faster setup.
3. **Execution**: The agent runs each assigned job sequentially with full
   telemetry: per-process CPU/RSS, GPU memory, timeseries sampling, structured
   completion records, and failure detection (OOM, segfault, signals).
   - **GPU warmup**: If the first GPU job on an instance is tagged `benchmark`,
     the agent runs a lightweight CUDA warmup (context init + cuBLAS handle
     creation) before starting it. This prevents cold-start overhead from
     inflating benchmark measurements. Non-benchmark GPU jobs warm the context
     implicitly, so the explicit warmup only triggers when no GPU job has run
     yet on the instance. Note: CUDA context is per-process, so each job's
     process still pays its own context init cost. The warmup primes
     system-level state: GPU driver, kernel JIT cache on disk, and cuBLAS/cuDNN
     library loading. This reduces — but does not fully eliminate — cold-start
     overhead for the first benchmark job.
4. **Result upload**: After each job, the wrapper uploads results (logs,
   completion record, timeseries, phases) to R2 under `jobs/<job-id>/`.
5. **Sweep**: The coordinator's sweep loop polls R2 for completed markers,
   downloads results, extracts phase timings and GPU stats into the
   `job_phase_timings` table, updates job statuses, and cleans up R2.
6. **Teardown**: The instance self-destructs after the wrapper completes.
   Time/budget limits also trigger automatic destruction.

### Parallel operations

- **Launch**: All instances in a campaign are provisioned concurrently using
  goroutines. Each instance goes through create → wait-for-ready → deploy →
  start independently.
- **Terminate**: When terminating multiple instances (directly or via campaign),
  all Vast.ai destroy calls run in parallel.

### Failure handling

- If an instance fails to provision, other instances in the campaign continue.
- Jobs on failed instances are reset to unplaced (queued with no host) for re-launch.
- The sweep loop detects orphaned instances (exceeded time limits, exited
  unexpectedly) and marks associated jobs as failed.
- Exit code 137 and dmesg/nvidia-smi analysis detect OOM failures.

## Configuration

In `~/.config/weft/config.toml`:

```toml
[vastai]
default_image = "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
max_runtime = "4h"

[vastai.r2]
bucket = "my-results-bucket"
account_id = "..."
access_key_id = "..."
secret_access_key = "..."
```

### Source sync

When launching with the agent (`--use-agent`), project sources are rsynced to
the cloud instance. The same exclude patterns as persistent hosts apply
(`.git`, `.venv`, `__pycache__`, etc.), plus app-level excludes from
`~/.config/weft/config.toml` and project-specific excludes and output dirs from
`.weft.toml`.

Current app-level defaults also exclude `runs`, `wand`, and `wandb`.

Example app config:

```toml
[sync]
exclude_dirs = ["lab-notebook"]
```

Example project config:

```toml
[sync]
exclude_dirs = ["data"]
```

### Per-project Docker image

By default, cloud instances use the image from `vastai.default_image` in the
global config. Projects that need a different base image (e.g., `devel` instead
of `runtime` for JIT kernel compilation) can override this in `.weft.toml`:

```toml
[cloud]
image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"
```

When a campaign contains jobs from multiple projects with different images, weft
automatically splits instance groups so each instance uses the correct image.
Jobs with no `[cloud] image` setting share the global default.

## Cost estimation

When launching a campaign, weft estimates the total cost per GPU group. If the
job-duration predictor is configured (`predictor.project_path` in config), the
estimate combines startup, SSH setup, provisioning, job setup, predicted run
time, and upload time. Otherwise the runtime falls back to a default of 1 hour
per job and the other phases use static defaults. The cost table shows:

- Resolved GPU name (e.g., "HOPPER+ → H200 NVL")
- Number of jobs, GPU memory, hourly rate
- Estimated duration and total cost

See [Estimation and Modeling](../reference/estimation.md) for the full
estimation pipeline, including the statistical models and telemetry sources.

## Data collection

Cloud jobs run the same Go agent as persistent-host jobs, producing identical
telemetry. The agent collects data for duration prediction and cost analysis:

### Phase timing

Each job records structured phase timing as `{jobID}.phases.json`: wrapper start,
setup start/end, run start/end, and cache probes. These are stored in the
`job_phase_timings` table, allowing the predictor to model setup, execution,
and upload phases independently.

### Cache-state probes

Before and after each job, the agent records the size of `~/.cache/huggingface`
and `~/.cache/uv` in the phases file. This lets the predictor distinguish
cold-cache first jobs from warm-cache subsequent jobs.

### Telemetry (timeseries)

During job execution, the agent samples at 15-second intervals and writes
`{jobID}.timeseries.jsonl`:
- Per-process CPU usage, RSS, and peak RSS (VmHWM)
- Per-process GPU memory (via nvidia-smi query-compute-apps)
- Host memory and memory pressure (3 levels)
- GPU utilization

### Completion records

On job completion, the agent writes `{jobID}.completion.json` with exit code,
peak metrics, failure reason (OOM, segfault, signal), and discovered outputs.

### Upload sizes

The wrapper records the byte sizes of the results directory and workspace before
uploading, so upload duration can be correlated with transfer size.

### Campaign job position

Jobs in a campaign are assigned a 0-based `campaign_job_index` indicating their
position in the execution sequence. This lets the predictor use position as a
feature (index 0 = cold caches, index 1+ = warm caches).

## DB schema

Campaigns use three tables:

- **`campaigns`**: Batch record with status (`planned`, `launching`, `running`,
  `completed`, `failed`, `canceled`)
- **`cloud_instances`**: Individual Vast.ai deployments linked to a campaign,
  tracking GPU spec, resolved GPU name, cost per hour, bandwidth, reliability,
  spend limits, Vast.ai instance ID, and lifecycle timestamps
  (`created_at`, `ready_at`, `launched_at`, `ended_at`)
- **`job_phase_timings`**: Per-job phase timestamps, cache state, upload sizes,
  and GPU monitoring summaries

Jobs link to cloud instances via `cloud_instance_id` and record their campaign
position via `campaign_job_index`.
