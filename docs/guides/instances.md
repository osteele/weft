# Cloud GPU Instances

Weft launches cloud GPU instances on Vast.ai (and RunPod) when local hosts
can't satisfy a job's GPU requirements. This guide covers everything about
launching, monitoring, and managing instances. For the batching concept that
groups multiple instances launched together, see [Campaigns](campaigns.md).

## Terminology

| Term | Meaning |
|------|---------|
| **Instance** | A single Vast.ai (or RunPod) deployment running one or more jobs |
| **Campaign** | A batch record grouping instances launched in one `weft start instance` invocation |
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

# 4. Launch instances
weft start instance
```

`weft start instance` (also `weft start instances` and `weft instance launch`)
is the primary command for moving unplaced jobs onto cloud GPUs — but see
[Coordinating with the autopilot](#coordinating-with-the-autopilot) first.
You usually do not need to launch by hand.

RunPod is also supported. Setup:

```bash
brew install runpodctl
runpodctl doctor
weft runpod doctor
weft runpod setup
```

RunPod launch readiness depends on the shared R2 bootstrap config under
`[vastai.r2]` plus a compatible `runpod.bootstrap_template_id`. Use
`weft runpod template print-bootstrap` to inspect the exact startup command
that the managed template must run.

## Coordinating with the autopilot

If the autopilot is running, you usually do **not** need to launch instances
by hand. The autopilot continuously watches for unplaced jobs and launches
instances for them on its own. Check first:

```bash
weft autopilot status            # text
weft autopilot status --json     # machine-readable
weft autopilot status --quiet    # exit 0=idle, 10=running, 11=stale, 12=paused
```

If `running`, just queue jobs and wait — the autopilot will pick them up. It
may take some time before an instance appears (a launch pass runs at most
once per autopilot cycle). **Launching manually is not faster than waiting
for the autopilot's next pass**; it just gives you control over:

- *Cost*: explicit `--max-spend`, `--strategy cheap|fast|fastest`, or
  `--min-survival` to override the autopilot's defaults.
- *Parallelism*: pick a subset of jobs (`--jobs`) or split a large batch
  across separate launches.
- *Offer selection*: review and pick offers in the TUI rather than letting
  the autopilot choose.

If you do need to launch manually while the autopilot is running, pause it
first to avoid double-launches:

```bash
weft autopilot pause --reason "manual launch"
weft start instance --yes --watch
weft autopilot resume
```

Pause is sticky across restarts; remember to resume.

## Launching instances

If you've decided manual launch is the right call (see
[Coordinating with the autopilot](#coordinating-with-the-autopilot)), the
following commands are how.

### Interactive launch

```bash
weft start instance
```

The TUI groups unplaced jobs by GPU class, searches for offers in parallel,
and presents an interactive selector:

1. **Job selection**: All eligible jobs are pre-selected. Use `Space` to
   toggle individual jobs, `a` to select all, `n` to deselect all.
2. **Cost review**: A cost table shows the best offer per GPU group with
   estimated setup time and total cost.
3. **Launch**: Press `Enter` to provision instances. All instances are
   launched **in parallel** for faster startup.
4. **Watch**: As soon as the rows are registered, the TUI switches into the
   watch-style view so you can follow provisioning, bootstrap, uptime, and
   job progress without waiting for the full launch to finish.

### Non-interactive launch

For scripted or agent-driven workflows, use `--yes` to skip the TUI:

```bash
weft start instance --yes --watch       # Launch all unplaced jobs, then watch
weft start instance --yes --jobs wj42,wj43  # Launch specific jobs only
weft start instance --yes --no-watch    # Launch and exit (print IDs)
```

This fetches offers, prints a cost summary, provisions instances in parallel,
and (unless `--no-watch`) transitions to watch mode.

### Project-scoped launch

To launch only jobs from a specific project:

```bash
weft project launch --yes --watch       # Launch unplaced jobs for cwd's project
weft start instance --project myproj    # Explicit project filter
```

When `weft project launch` transitions to watch mode it watches only
project-scoped instances and filters unplaced jobs to that project. Plain
`weft start instance` watches all active instances and shows all unplaced
jobs.

### Options

```bash
weft start instance --dry-run           # Preview plan without launching
weft start instance --watch             # Enter watch mode after launch
weft start instance --no-watch          # Launch and exit (print IDs only)
weft start instance --project myproj    # Filter to a specific project's jobs
weft start instance --max-spend '$10'   # Budget limit per instance
weft start instance --max-time 2h       # Time limit per instance
weft start instance --yes               # Skip TUI, launch all groups
weft start instance --jobs wj42,wj43    # Filter to specific job IDs
weft start instance --min-survival 0    # Disable survival floor (allow all offers)
weft start instance --min-survival 0.6  # Stricter survival floor
```

### Interruptible jobs

Mark jobs that can tolerate interruption with:

```bash
weft run --tag rental --tag interruptible --gpu a100 'python train.py'
```

or via script metadata:

```toml
[tool.weft]
interruptible = true
```

The tag and key `preemptible` are accepted as synonyms for backwards
compatibility.

Only jobs marked interruptible are eligible for interruptible offers. Weft
keeps non-interruptible jobs on normal offers and does not mix the two on the
same instance.

**Price.** The bid is the offer's asking `cost/hr` (`max_bid = ask`). There
is no separate bid knob in this first pass — if you want a stricter ceiling,
pick a cheaper offer via the usual GPU/memory filters. The launch plan table
annotates interruptible groups as `$X.YY/hr (int, bid $X.YY)` so the chosen
bid is visible in `--dry-run`.

**Not compatible with `benchmark`.** The submission path rejects jobs that
combine `benchmark` and `interruptible`: preemption pauses the container
mid-measurement (invalidating timing; the GPU is cold on resume), and a
stale-pause relaunch moves the job to a different physical machine, breaking
the "same hardware" control that benchmark analyses rely on.

**Historical accounting.** Every launch records its `instance_type`
(`on-demand` / `interruptible`), `max_bid_price_cents` (nil for on-demand),
and `on_demand_ref_cents` — the cheapest concurrent on-demand ask for the
same GPU class at launch time — so savings can be computed per-run rather
than inferred from tag history.

**Pause time and relaunch chains.** Provider status transitions (every move
into and out of `stopped`) are persisted in `provider_status_transitions`;
`db.LaunchPausedSeconds(launchID, refTime)` sums the stopped intervals to
give wall-clock-vs-runtime split. When a preempted launch is replaced, the
new attempt records its predecessor via
`job_attempts.predecessor_attempt_id`, and the old attempt's `cloud_outcome`
is set to `preempted` (distinct from generic `orphaned`). Together these let
runtime-estimation queries follow a job across the full relaunch chain and
account for pause time separately.

**Job-visible env vars.** Jobs get the following on every run:

| Var | Value |
|-----|-------|
| `WEFT_JOB_ID` | weft DB job id |
| `WEFT_TARGET_KIND` | `host` or `rental` |
| `WEFT_LAUNCH_ID` | weft DB launch id (rental only) |
| `WEFT_PROVIDER` | `vastai` / `runpod` (rental only) |
| `WEFT_INSTANCE_TYPE` | `on-demand` / `interruptible` (rental only) |
| `WEFT_RESUMED` | `1` if the agent's container has restarted on the same disk (typical Vast.ai pause/resume); unset on first boot |

User-supplied secret env vars are resolved before the job is sent to the
instance. Store `hf` with `weft secret set hf "$HF_TOKEN"` and use
`--hf-token` or declare an `hf:` / `hf-dataset:` input to attach
`HF_TOKEN=secret:hf` without writing the token into the job database.

Use `WEFT_RESUMED` to fork checkpoint-loading vs cold-start logic in jobs
that may run on interruptible instances.

To see how much interruptible currently saves vs on-demand for a given GPU
class, run:

```bash
weft cloud price-spread                       # default: rtx_3090 rtx_4090 a100 h100
weft cloud price-spread rtx_5090 h200         # specific classes
weft cloud price-spread --min-gpu-mem 40      # filter by per-GPU memory
weft cloud price-spread --json                # machine-readable output
```

The table reports min and median `$/hr` for on-demand vs interruptible in
each market, plus the savings ratio. In practice median savings are roughly
25-30% across most tiers; the larger gaps (50-75%) show up at the min on the
less-liquid ends of the market.

**Policy.** Interruptible instances are *pause-tolerant*: when the provider
marks them `stopped` (typically after losing a bid) weft leaves them in place
and waits for the provider to resume them. If the pause lasts longer than
~6 hours, weft gives up, fails the launch with
`termination_reason = preempted`, and the standard retry path relaunches the
jobs on a fresh offer. Jobs that already completed are not re-run. The
runaway breaker (see [Unattended runaway protection](#unattended-runaway-protection))
is the backstop against a pathological offer churning launches.

## Monitoring instances

### Watch mode

```bash
weft instance watch                     # Watch active instances and unplaced jobs
weft instance watch <instance-id>       # Watch a specific instance
weft campaign watch <campaign-id>       # Watch a specific batch
weft campaign watch --tui <campaign-id> # Interactive TUI
```

Watch mode shows per-instance details:
- Vast.ai instance ID and status
- SSH connection command
- Uptime and estimated cost
- Job progress (completed/total)

Press `Ctrl-C` to exit watch mode. Instances continue running in the
background.

### Listing and status

```bash
weft instance list                      # List all cloud instances
weft instance status <instance-id>      # Single instance details
weft instance info <instance-id>        # Alias for status
```

### Unattended runaway protection

When `--auto` is enabled, weft includes a runaway breaker to prevent
launch/die loops from running unattended for long periods.

The breaker watches relaunch behavior at the campaign/project scope and
trips when it detects no-progress churn (for example repeated orphaned
retries with no completed jobs), even when failures happen after long
runtimes.

Default thresholds:

- `auto_runaway_window = "24h"`
- `auto_runaway_chain_no_progress_limit = 3`
- `auto_runaway_orphan_churn_limit = 8`
- `auto_runaway_spend_no_progress_limit = 5.0`

Configuration (`~/.config/weft/config.toml`):

```toml
[campaign]
auto_runaway_enabled = true
auto_runaway_window = "24h"
auto_runaway_chain_no_progress_limit = 3
auto_runaway_orphan_churn_limit = 8
auto_runaway_spend_no_progress_limit = 5.0
```

If the breaker trips, auto-relaunch is blocked until you manually resume.
For a per-campaign trip:

```bash
weft campaign safety resume --campaign <campaign-id> [--project <project-name>]
```

For a global trip (or to inspect what's currently tripped):

```bash
weft autopilot blocked              # list tripped scopes + affected jobs + metrics
weft autopilot blocked --unblock    # list and reset in one shot
weft autopilot budget reset         # reset only
```

`weft info wj<N>` also surfaces the trip inline on any paused job. See the
[Autopilot guide](autopilot.md) for the full inspection and reset workflow.

## Managing instances

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

# All instances in a campaign batch (terminated in parallel)
weft campaign terminate <campaign-id>
```

`cancel` is an alias for `terminate` on both commands:

```bash
weft instance cancel <id>
weft campaign cancel <id>
```

Termination destroys the provider instance, resets associated jobs to
unplaced (queued with no host), and updates the instance status to
`canceled`.

### Cordoning (drain without terminating)

When you want an instance's current job to finish but no new jobs to land
on it — for example, the instance is running an outdated agent build, or
you want to take it out of rotation while you investigate — cordon it:

```bash
weft instance cordon <instance-id> --reason "stale agent"
weft instance uncordon <instance-id>
```

A cordoned instance keeps running, its active job continues to
completion, and grace-period behaviour is unchanged. The autopilot and
explicit reuse paths simply skip it when assigning new jobs. Cordon
state is shown in `weft instance list` (as `[cordoned]` next to the
status), in `weft instance info`, and in the watch TUI.

Use this instead of pausing the autopilot globally when you only need
to drain one instance. The flag clears immediately on `uncordon`.

## Grace period

Cloud instances have a default 15-minute grace period after job failures.
Instead of self-destructing immediately, the instance waits for you to fix
the issue and resubmit. This avoids repaying provisioning overhead (~100s
startup + model downloads).

### Launching with a grace period

```bash
# Default: 15-minute grace period
weft start instance

# Custom grace period
weft start instance --grace-period 30m

# No grace period (immediate self-destruct on failure)
weft start instance --grace-period 0
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

## Watch TUI single-job launch

For launching a single job without the batch flow, use the TUI:

1. Open `weft tui`
2. Navigate to an unplaced job (queued with no host)
3. Press `c` to open the rental GPU menu
4. Select a Vast.ai offer and confirm the cost

This launches a single instance for that job (recorded as a one-instance
campaign).

In the watch TUI, queued jobs that are already attached to a cloud instance
also have two move paths:

- `m`: deliberative move. Opens the picker with existing destinations and new
  cloud offers, showing cheap/fast/fastest choices and prices.
- `N`: atomic launch. Launches a new instance immediately with the fast
  strategy, without opening the picker or asking for confirmation.

## How it works

### Instance lifecycle

1. **Provisioning**: Weft creates a provider instance with the configured
   Docker image, disk, and SSH access.
2. **Setup**: Once the instance is running, weft deploys rclone
   configuration, the Go agent binary (`weft-agent`), and rsyncs project
   sources via SSH. Agent deployment and source sync run in parallel for
   faster setup.
3. **Execution**: The agent runs each assigned job sequentially with full
   telemetry: per-process CPU/RSS, GPU memory, timeseries sampling,
   structured completion records, and failure detection (OOM, segfault,
   signals).
   - **GPU warmup** (opt-in): The agent can run a lightweight CUDA warmup
     (context init + cuBLAS handle creation) before the first GPU benchmark
     job. Disabled by default. Enable in `~/.config/weft/config.toml`:
     ```toml
     [campaign]
     gpu_warmup = true
     ```
   - **Benchmark barrier**: Benchmark jobs (tagged `benchmark`) wait for all
     background uploads from prior jobs to complete before starting,
     preventing I/O interference with measurements.
4. **Result upload**: After each job, the wrapper uploads results (logs,
   completion record, timeseries, phases) to R2 under `jobs/<job-id>/`.
5. **Sweep**: The coordinator's sweep loop polls R2 for completed markers,
   downloads results, extracts phase timings and GPU stats into the
   `job_phase_timings` table, updates job statuses, and cleans up R2.
6. **Teardown**: The instance self-destructs after the wrapper completes.
   Time/budget limits also trigger automatic destruction.

### Parallel operations

- **Launch**: Instances launched in one batch are provisioned concurrently
  using goroutines. Each instance goes through create → wait-for-ready →
  deploy → start independently.
- **Terminate**: When terminating multiple instances (directly or via a
  batch), all provider destroy calls run in parallel.

### Failure handling

- If one instance fails to provision, others in the same batch continue.
- Jobs on failed instances are reset to unplaced (queued with no host) for
  re-launch.
- The sweep loop detects orphaned instances (exceeded time limits, exited
  unexpectedly) and marks associated jobs as failed.
- Exit code 137 and dmesg/nvidia-smi analysis detect OOM failures.

### Stall detection

The reconciler monitors instance progress and auto-terminates stuck
instances:

- **Bootstrap stall**: Instance is running but the agent never started a
  job. Uses adaptive thresholds learned from historical bootstrap durations
  per provider (survival analysis). Default: warn at 15m, terminate at 20m.
- **Setup phase stall**: Agent started the setup phase (e.g., `uv sync`)
  but never transitioned to running. Uses adaptive thresholds learned from
  historical setup durations for the same command and workspace, falling
  back to workspace-level or all-jobs data when per-command samples are
  insufficient (< 20). Default: warn at 15m, terminate at 25m.
- **Heartbeat stale**: Agent heartbeat is older than 3 minutes. Display-only
  warning; the reconciler's SSH probe logic handles actual termination.

Terminated instances have their jobs reset to queued for automatic retry.

## Configuration

In `~/.config/weft/config.toml`:

```toml
[vastai]
default_image = "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
max_runtime = "4h"

[campaign]
reliability = 0.95             # provider-offer reliability floor (0 disables)
retry_first_time_limit = "45m" # first retry tier
retry_first_cost_limit = 1.0   # USD
retry_next_time_limit = "45m"  # second+ retry tiers
retry_next_cost_limit = 0.25   # USD

[vastai.r2]
bucket = "my-results-bucket"
account_id = "..."
access_key_id = "..."
secret_access_key = "..."
```

Retry limits apply to both automatic relaunch and manual `r` retries in
watch mode. Each retry tier stops when either its time limit or cost limit
is reached, whichever happens first.

`campaign.reliability` is used during provider offer search. The default is
`0.95`; set it to `0` to allow offers regardless of provider reliability.

### Source sync

When launching with the agent (`--use-agent`), project sources are rsynced
to the cloud instance. The same exclude patterns as persistent hosts apply
(`.git`, `.venv`, `__pycache__`, etc.), plus app-level excludes from
`~/.config/weft/config.toml` and project-specific excludes and output dirs
from `.weft.toml`.

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

By default, cloud instances use the image from `vastai.default_image` in
the global config. Projects that need a different base image (e.g., `devel`
instead of `runtime` for JIT kernel compilation) can override this in
`.weft.toml`:

```toml
[cloud]
image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"
```

When a launch contains jobs from multiple projects with different images,
weft automatically splits instances so each one uses the correct image.
Jobs with no `[cloud] image` setting share the global default.

Custom images can also declare explicit driver/CUDA requirements and private
registry credentials:

```toml
[cloud]
image = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"
min_driver = "535"
min_cuda = "12.9"
image_pull_secret = "ghcr.io"
```

Weft also attempts to read `NVIDIA_REQUIRE_CUDA` from the image's OCI config
and derive the required NVIDIA driver and CUDA floors automatically. Explicit
`min_driver` and `min_cuda` values are useful for private images, custom images
that do not publish this metadata, or when you want a stricter floor.

On Vast.ai, `min_driver` becomes a `driver_version>=...` offer filter and
private registry credentials are passed with the provider login flag. On
RunPod, `min_cuda` is passed as the pod CUDA compatibility floor and configured
registry credentials are translated into a RunPod registry auth record.

Private registry credentials live in `~/.config/weft/config.toml`:

```toml
[registry."ghcr.io"]
username = "osteele"
password_env = "WEFT_GHCR_TOKEN"
runpod_auth_name = "weft-ghcr" # optional
```

Use `password_command = "gh auth token"` instead of `password_env` if the token
should be fetched dynamically. Configure only one password source per registry.

### CUDA toolkit compatibility

Weft automatically filters out GPU offers that require a newer CUDA toolkit
than the Docker image provides. Each GPU generation has a minimum CUDA
version needed for kernel compilation:

| Generation | GPUs | Min CUDA Toolkit |
|---|---|---|
| Turing | RTX 2080 Ti, T4 | 10.0 |
| Ampere | RTX 3090, A100, A10 | 11.0 |
| Ada Lovelace | RTX 4090, L40S, L4 | 11.8 |
| Hopper | H100, H200 | 12.0 |
| Blackwell | B200, RTX 5090 | 12.8 |

With the default image (`nvidia/cuda:12.4.1-runtime`), Blackwell GPUs (RTX
5090, B200) are automatically excluded since they need CUDA 12.8+. To use
newer GPUs, set a compatible image in `.weft.toml`:

```toml
[cloud]
image = "nvidia/cuda:12.8.0-runtime-ubuntu22.04"
```

## Cost estimation

When launching, weft estimates the total cost per GPU group. If the
job-duration predictor is configured (`predictor.project_path` in config),
the estimate combines startup, SSH setup, provisioning, job setup,
predicted run time, and upload time. Otherwise the runtime falls back to a
default of 1 hour per job and the other phases use static defaults. The
cost table shows:

- Resolved GPU name (e.g., "HOPPER+ → H200 NVL")
- Number of jobs, GPU memory, hourly rate
- Estimated duration and total cost

See [Estimation and Modeling](../reference/estimation.md) for the full
estimation pipeline, including the statistical models and telemetry
sources.

## Data collection

Cloud jobs run the same Go agent as persistent-host jobs, producing
identical telemetry. The agent collects data for duration prediction and
cost analysis:

### Phase timing

Each job records structured phase timing as `{jobID}.phases.json`: wrapper
start, setup start/end, run start/end, and cache probes. These are stored
in the `job_phase_timings` table, allowing the predictor to model setup,
execution, and upload phases independently.

### Cache-state probes

Before and after each job, the agent records the size of
`~/.cache/huggingface` and `~/.cache/uv` in the phases file. This lets the
predictor distinguish cold-cache first jobs from warm-cache subsequent
jobs.

### Telemetry (timeseries)

During job execution, the agent samples at 15-second intervals and writes
`{jobID}.timeseries.jsonl`:
- Per-process CPU usage, RSS, and peak RSS (VmHWM)
- Per-process GPU memory (via nvidia-smi query-compute-apps)
- Host memory and memory pressure (3 levels)
- GPU utilization

### Completion records

On job completion, the agent writes `{jobID}.completion.json` with exit
code, peak metrics, failure reason (OOM, segfault, signal), and discovered
outputs.

### Upload sizes

The wrapper records the byte sizes of the results directory and workspace
before uploading, so upload duration can be correlated with transfer size.

### Position within a batch

Jobs launched together are assigned a 0-based `campaign_job_index`
indicating their position in the execution sequence. This lets the
predictor use position as a feature (index 0 = cold caches, index 1+ =
warm caches).

## DB schema

Cloud instances span three tables:

- **`cloud_instances`**: Individual provider deployments, tracking GPU
  spec, resolved GPU name, cost per hour, bandwidth, reliability, spend
  limits, provider instance ID, and lifecycle timestamps (`created_at`,
  `ready_at`, `launched_at`, `ended_at`)
- **`campaigns`**: Batch record grouping instances launched together, with
  status (`planned`, `launching`, `running`, `completed`, `failed`,
  `canceled`)
- **`job_phase_timings`**: Per-job phase timestamps, cache state, upload
  sizes, and GPU monitoring summaries

Jobs link to instances via `cloud_instance_id` and record their position
within the launching batch via `campaign_job_index`.
