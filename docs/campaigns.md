# Cloud GPU Campaigns

Weft's campaign system launches batches of jobs on Vast.ai cloud GPUs when local
hosts can't satisfy GPU requirements.

## Terminology

| Term | Meaning |
|------|---------|
| **Campaign** | A batch of cloud instances launched together |
| **Instance** | A single Vast.ai deployment running one or more jobs |
| **`needs_rental`** | Job status indicating no local host matches constraints |

## Quick start

```bash
# 1. Install Vast.ai CLI
pip install vastai
vastai set api-key YOUR_API_KEY

# 2. Configure R2 for result upload (in ~/.config/weft/config.yaml)
# vastai:
#   r2:
#     bucket: "my-results-bucket"
#     account_id: "..."
#     access_key_id: "..."
#     secret_access_key: "..."

# 3. Queue jobs that need cloud GPUs
weft run --gpu hopper+ -m "Train on H100" 'python train.py'
# Job accepted with needs_rental status

# 4. Launch a campaign
weft campaign launch
```

## Launching campaigns

### Interactive launch

```bash
weft campaign launch
```

The TUI groups `needs_rental` jobs by GPU class, searches for Vast.ai offers in
parallel, and presents an interactive selector:

1. **Job selection**: All eligible jobs are pre-selected. Use `Space` to
   toggle individual jobs, `a` to select all, `n` to deselect all.
2. **Cost review**: A cost table shows the best offer per GPU group with
   estimated setup time and total cost.
3. **Launch**: Press `Enter` to create a campaign and provision instances.
   All instances are launched **in parallel** for faster startup.
4. **Watch**: The TUI automatically transitions to watch mode showing
   instance status, SSH info, uptime, and job progress.

### Non-interactive launch

For scripted or agent-driven workflows, use `--yes` to skip the TUI:

```bash
weft campaign launch --yes              # Launch all needs_rental jobs
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
```

## Monitoring campaigns

### Watch mode

```bash
weft campaign watch <campaign-id>       # Line-oriented (good for agents/pipes)
weft campaign watch --tui <campaign-id> # Interactive TUI
weft campaign watch <id1> <id2>         # Watch multiple campaigns
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
`needs_rental`, and updates the instance status to `cancelled`.

## TUI cloud menu (single job)

For launching a single job without the full campaign flow, use the TUI:

1. Open `weft tui`
2. Navigate to a **queued** or **needs_rental** job
3. Press `c` to open the cloud GPU menu
4. Select a Vast.ai offer and confirm the cost

This creates a campaign with a single instance for that job.

## How it works

### Instance lifecycle

1. **Provisioning**: Weft creates a Vast.ai instance with the configured Docker
   image, disk, and SSH access.
2. **Setup**: Once the instance is running, weft deploys rclone configuration
   and a wrapper script via SSH.
3. **Execution**: The wrapper script runs each assigned job sequentially,
   capturing stdout/stderr, exit codes, phase timing, cache state, and GPU
   utilization.
4. **Result upload**: On completion, the wrapper uploads results (logs, exit
   code, phase timings, GPU monitor data, debug artifacts) to R2 under
   `jobs/<job-id>/`.
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
- Jobs on failed instances remain in `needs_rental` status for re-launch.
- The sweep loop detects orphaned instances (exceeded time limits, exited
  unexpectedly) and marks associated jobs as failed.
- Exit code 137 and dmesg/nvidia-smi analysis detect OOM failures.

## Configuration

In `~/.config/weft/config.yaml`:

```yaml
vastai:
  default_image: "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
  max_runtime: "4h"    # Max runtime before orphan cleanup
  r2:
    bucket: "my-results-bucket"
    account_id: "..."
    access_key_id: "..."
    secret_access_key: "..."
```

## Cost estimation

When launching a campaign, weft estimates the total cost per GPU group. If the
job-duration predictor is configured (`predictor.project_path` in config), each
job's predicted duration is summed with a 10-minute setup overhead. Otherwise a
default of 1 hour per job is used. The cost table shows:

- Resolved GPU name (e.g., "HOPPER+ → H200 NVL")
- Number of jobs, GPU memory, hourly rate
- Estimated duration and total cost

## Data collection

The wrapper scripts collect data for duration prediction and cost analysis:

### Phase timing

Each job records timestamps for wrapper start, setup start/end, run start/end,
and upload start/end. These are stored in the `job_phase_timings` table, allowing
the predictor to model setup, execution, and upload phases independently.

### Cache-state probes (campaign wrapper)

Before each job in a campaign, the wrapper records the size of `~/.cache/huggingface`
and `~/.cache/uv`. This lets the predictor distinguish cold-cache first jobs from
warm-cache subsequent jobs.

### GPU monitoring

During job execution, a background `nvidia-smi` sampling loop (5s interval)
records GPU utilization and memory usage. The sweep loop extracts peak GPU memory,
mean GPU utilization, and peak GPU utilization from the CSV and stores them in
`job_phase_timings`.

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
  `completed`, `failed`, `cancelled`)
- **`cloud_instances`**: Individual Vast.ai deployments linked to a campaign,
  tracking GPU spec, resolved GPU name, cost per hour, bandwidth, reliability,
  spend limits, Vast.ai instance ID, and lifecycle timestamps
  (`created_at`, `ready_at`, `launched_at`, `ended_at`)
- **`job_phase_timings`**: Per-job phase timestamps, cache state, upload sizes,
  and GPU monitoring summaries

Jobs link to cloud instances via `cloud_instance_id` and record their campaign
position via `campaign_job_index`.
