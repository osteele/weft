# Weft

[![Go Reference](https://pkg.go.dev/badge/github.com/osteele/weft.svg)](https://pkg.go.dev/github.com/osteele/weft)
[![Go Report Card](https://goreportcard.com/badge/github.com/osteele/weft)](https://goreportcard.com/report/github.com/osteele/weft)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A coordinator-based workload scheduler for GPU compute clusters, with resource
inventory, data locality awareness, automatic job placement, and cloud instance
lifecycle management across commodity commercial GPU providers.

## Overview

Weft manages jobs across on-prem and cloud GPU capacity. Submit a job from your
laptop without specifying a host, and the coordinator can either place it on
the best available machine in your cluster or provision, monitor, and tear down
instances on cheap commercial GPU providers when local capacity is insufficient.
Placement decisions consider GPU capabilities, data locality, current
utilization, and queue depth. Jobs run in tmux sessions that survive SSH
disconnections, laptop sleep, and network outages.

```
Laptop (CLI / TUI)
   │
   ├──intent──> Coordinator (studio, always-on)
   │              ├── Scores hosts via placement engine
   │              ├── Pre-stages missing data (rsync between hosts)
   │              └── Dispatches to best host's queue
   │
   └──fallback──> Direct SSH (when coordinator unreachable)
                    └── Local placement scoring, queue directly

Remote Hosts (titan, atlas)
   └── Go agent (autonomous)
        ├── Discovers and executes queued jobs
        ├── Manages GPU allocation and exclusive jobs
        ├── Logs output, captures exit codes
        └── Processes next job when current completes
```

### Key features

- **Automatic placement**: Omit the host and let the coordinator pick the best
  one based on GPU class, memory, data locality, and current load
- **Data locality**: Declare `--input hf:model-name` and the coordinator prefers
  hosts with the data cached, pre-stages it via rsync, or downloads missing HF
  assets before the job starts
- **Occasionally connected**: Jobs are recorded locally first and synced when
  hosts are reachable — your laptop can sleep, travel, or disconnect
- **Graceful degradation**: When the coordinator is unreachable, the CLI falls
  back to local placement scoring and direct SSH dispatch
- **Cloud GPU bursting**: When local GPUs are busy or no host matches,
  `weft campaign launch` batch-provisions Vast.ai instances, runs jobs, and
  tears down on completion. Failed jobs enter a grace period for resubmission
- **Campaign management**: Launch, watch, and terminate batches of cloud
  instances from the CLI or TUI. `weft campaign watch` streams live status,
  cost, and per-job progress until all instances finish
- **System watch**: `weft watch` shows all active cloud instances, on-prem jobs,
  and unplaced jobs in a simplified TUI or periodic plain-text summary
- **Web dashboard**: Browser UI at `localhost:8127/cluster` with host cards,
  live GPU utilization bars, coordinator status, and recent placement decisions

#### Campaign Planner

`weft campaign launch` opens an interactive planner:

```
Cloud GPU jobs (12 jobs, 3 GPU groups)

  [x] A100 (3 jobs)
  [x]   42  Llama-3 70B fine-tune on RedPajama (LoRA, 3 epochs)
  [x]   43  Mixtral 8x7B inference benchmark (batch=64)
> [ ]   44  GPT-NeoX 20B ablation: attention heads vs throughput
  [-] RTX 4090 (5 jobs)
  ...

── Cost Estimate ──────────────────────────────────────────────────
A100 → A100 PCIE    2/3 jobs  80GB  $0.52/hr  ~2h (1h–4h)   ~$1.04±0.52
RTX 4090             5/5 jobs  24GB  $0.24/hr  ~5h (2h–10h)  ~$1.20±0.96
                                                   Total: ~$2.24

↑/↓ navigate  space toggle  a all  n none  d details  enter launch  q quit
```

#### Campaign Watch

Monitor running instances with `weft campaign watch`:

```
Campaign 7 — 3 instances

Instance 12  RTX 4090 24GB   running    ⏱ 42m   $0.18
  Job 108  Fine-tune Llama-3 8B (LoRA)              ● running
  Job 109  Eval on MMLU + HellaSwag                  ○ queued

Instance 13  A100 80GB       running    ⏱ 38m   $0.33
  Job 110  Mixtral 8x7B inference latency sweep      ● running

Instance 14  RTX 3060 12GB   grace      ⏱ 51m   $0.10
  Job 111  Phi-3 mini quantization benchmark         ✗ exit 1 (OOM)
  Grace period: 12m remaining
  → weft instance submit 14 111 --command '...'  # resubmit with fix
  → weft instance release 14                     # clean shutdown
```

When an instance enters the grace period after a failure, the watch output
shows remaining time and suggested commands for resubmission or release.

`weft campaign watch` watches a single campaign. If you omit the campaign ID, it
watches the most recent campaign. Use `--tui` or `--plain` to override the
default terminal-based mode selection.

#### System Watch

Monitor the full active system with `weft watch`:

```bash
weft watch           # Interactive TUI in a terminal
weft watch --plain   # Periodic plain-text summary
weft watch --follow  # Keep printing summaries even when idle
```

The system watch shows:

- Active cloud instances across all campaigns
- On-prem running and queued jobs grouped by host
- Unplaced jobs that are waiting for placement or cloud launch
- Directory tails in job listings, consistent with `weft job list`

Press `l` in the watch TUI to jump into the cloud launch planner, then return
to the watch view when the planner exits.

#### Campaign Commands

Current campaign subcommands are:

```bash
weft campaign launch [--dry-run|--yes|--plain|--tui]
weft campaign watch [campaign-id] [--plain|--tui]
weft campaign list [--plain|--tui]
weft campaign show <campaign-id>
weft campaign terminate <campaign-id>
```

### Designed for unreliable networks

Laptops move between Wi-Fi networks, VPNs flap, and SSH servers occasionally
drop. Weft treats those scenarios as normal operations:

- Jobs always start locally first, so connection failures never lose metadata.
- Failed SSH attempts automatically defer work to the host queue (or the local
  pending list) and are replayed by `weft sync` when the host returns.
- Blocking commands such as `status --wait` and `plan submit --wait` keep
  polling while the host is down and announce when the connection comes back.

See [Network Resilience](docs/guides/network-resilience.md) for the full story
on how the CLI keeps itself useful while you roam across networks.

### Agents-first ergonomics

Weft was designed for workflows where an automated agent drives the CLI while a
human keeps an eye on the TUI. Most commands print suggested follow-up commands
(“next steps”) directly in their output so agents can keep the relevant context
in their prompt without hunting through reference docs or skills files. For
example, `weft run` prints the `status`, `log`, and `start` commands that make
sense for the job it just created, which agents can copy verbatim. The TUI then
becomes the dashboard where humans monitor progress, adjust queues, or apply
manual fixes when needed.

### Architecture

Weft has three components:

1. **CLI / TUI** (laptop) — submits jobs, monitors status, provides interactive
   dashboard
2. **Coordinator** (studio, always-on) — watches for intent files, runs
   placement scoring, pre-stages data, dispatches to host queues
3. **Go agent** (each remote host) — autonomous queue runner managing job
   execution, GPU allocation, and resource monitoring

The coordinator is optional. Without it, the CLI places jobs directly using the
same scoring logic. See [Comparison to SLURM](docs/design/comparison-to-slurm.md)
for how this differs from centralized job managers.

## Installation

```bash
go install github.com/osteele/weft@latest
```

Or build from source:
```bash
jj git clone https://github.com/osteele/weft
cd weft
go install .
```

## Documentation Map

For task-oriented docs and deeper design notes, start with
[docs/README.md](docs/README.md).

- **User guides**: [Workflow Guide](docs/guides/workflow-guide.md),
  [Campaigns](docs/guides/campaigns.md),
  [Network Resilience](docs/guides/network-resilience.md),
  [Debugging](docs/guides/debugging.md)
- **Reference**: [Job Plans](docs/reference/job-plans.md),
  [Command Reference](docs/reference/commands.md),
  [Logging and Progress](docs/reference/logging-and-progress.md),
  [Estimation and Modeling](docs/reference/estimation.md)
- **Architecture and design**: [Architecture](docs/design/architecture.md),
  [Coordinator Architecture](docs/design/coordinator-architecture.md),
  [Comparison to SLURM](docs/design/comparison-to-slurm.md)
- **Development and planning**: [Agent Deployment](docs/development/agent-deployment.md),
  [Manual Testing Runbook](docs/development/manual-testing-runbook.md),
  [Campaign Roadmap](docs/planning/ROADMAP.md),
  [Future Ideas](docs/planning/IDEAS.md)

## Commands

The full CLI reference lives in [Command Reference](docs/reference/commands.md).

Most workflows start with:

```bash
weft run --gpu-class a100 -m "Train" 'uv run python train.py'
weft data where hf:meta-llama/Llama-3-8B
weft data fetch hf:meta-llama/Llama-3-8B --host cool100
weft log 42 -f
weft job list --running
weft plan submit plan.yaml
```

For shell syntax, queue operations, job control, artifact commands, and advanced flags, use the dedicated reference above.

## Terminal UI

Launch the interactive terminal UI with `weft tui` (or `weft tui --mouse` for
clickable rows).

```bash
weft tui
weft tui --mouse   # enable mouse clicks (disables terminal selection)
```

The TUI has two views: **Jobs** and **Hosts**.
Press `f` at any time to cycle the Jobs view between showing all jobs, only queued/running jobs, completed successes, or completed failures.

### Jobs View (default)

Split-screen with:
- **Top panel**: Job list with status indicators (colored by status)
- **Bottom panel**: Job details or logs

Jobs are sorted by the newest job IDs so your latest or actively queued entries stay near the top of the list.

Press `d` on a highlighted job to flip it into draft mode. Drafting a queued job
removes it from the remote queue (or defers the removal if the host is offline),
while drafting a running job kills it and marks the record so future syncs don't
try to restart it.

When editing a queued job (`e`), you can set a CPU allotment preset (20/40/60/80%).
If unset, the runner treats it as the default (60%) and keeps total host
utilization under its cap while adjusting local allotments based on observed
usage.

```
╭──────────────────────────────────────────────────────────────────────────────╮
│ ID   HOST         STATUS       STARTED      COMMAND / DESCRIPTION            │
│ 52   deepthought  ● running    2h ago       python train.py --lr 0.001       │
│ 51   deepthought  ✗ exit 1     3h ago       python test.py                   │
│ 50   skynet       ✓ done       yesterday    make build                       │
╰──────────────────────────────────────────────────────────────────────────────╯
╭──────────────────────────────────────────────────────────────────────────────╮
│ Details                                                                      │
│ Job 52 on deepthought                                                        │
│ Cmd:     python train.py --lr 0.001                                          │
│ Dir:     ~/code/ml-project                                                   │
│ Started: 2025-12-13 10:15:32 (2h ago)                                        │
│ Elapsed: 2h 14m 23s (running)                                                │
│                                                                              │
│ Process Stats:                                                               │
│   CPU:     45% (1h23m user, 5m sys)                                          │
│   Memory:  2.1 GB (12%)                                                      │
│   Threads: 24                                                                │
│   GPU 0:   85% util, 12.5GiB                                                 │
╰──────────────────────────────────────────────────────────────────────────────╯
 ↑/↓:nav l:logs s:sync n:new r:restart k:kill d:draft p:prune h:hosts q:quit
```

Press `l` to view logs:

```
╭──────────────────────────────────────────────────────────────────────────────╮
│ ID   HOST         STATUS       STARTED      COMMAND / DESCRIPTION            │
│ 52   deepthought  ● running    2h ago       python train.py --lr 0.001       │
│ 51   deepthought  ✗ exit 1     3h ago       python test.py                   │
│ 50   skynet       ✓ done       yesterday    make build                       │
╰──────────────────────────────────────────────────────────────────────────────╯
╭──────────────────────────────────────────────────────────────────────────────╮
│ Logs: Job 52 on deepthought                                                  │
│ Epoch 45/100: loss=0.0234, acc=0.9812                                        │
│ Epoch 46/100: loss=0.0229, acc=0.9818                                        │
│ Epoch 47/100: loss=0.0221, acc=0.9825                                        │
│ Epoch 48/100: loss=0.0218, acc=0.9831                                        │
│ ...                                                                          │
╰──────────────────────────────────────────────────────────────────────────────╯
 ↑/↓:nav l:logs s:sync n:new r:restart k:kill p:prune h:hosts q:quit
```

Press `?` for the full keyboard shortcut help overlay.

Mouse support is off by default so you can select/copy text with your terminal. Pass `--mouse` (or set `enable_mouse = true` in `~/.config/weft/config.toml`) if you prefer clickable rows instead.

**Log caching:** When a host goes offline, the TUI shows the last successfully fetched log content with a "(cached - host offline)" indicator.

### Hosts View

Shows all hosts that have had jobs, with system info, queue status, and resource utilization.

- **Top panel**: Host list with status, queue runner, architecture, CPU/RAM usage
- **Bottom panel**: Detailed host info including per-GPU stats

```
╭──────────────────────────────────────────────────────────────────────────────╮
│ HOST         STATUS     QUEUE    ARCH             CPU     RAM                │
│ deepthought  ● online   ▶ 3      Linux x86_64     45%     62%                │
│ skynet       ● online   ○        Linux x86_64     12%     28%                │
│ tardis       ○ offline  -        Linux x86_64     -       -                  │
╰──────────────────────────────────────────────────────────────────────────────╯
╭──────────────────────────────────────────────────────────────────────────────╮
│ Host Details                                                                 │
│ Host: deepthought                                                            │
│ Status: online                                                               │
│ ───────────────────────────────────────────────────────────────              │
│ Architecture: Linux x86_64                                                   │
│ OS Version:   5.15.0-generic                                                 │
│ CPUs:         32                                                             │
│ Memory:       45Gi used / 128Gi total                                        │
│ Load:         2.31 (1m), 1.89 (5m), 1.45 (15m)  [7% utilized]                │
│ GPUs:         6× NVIDIA GeForce RTX 3090                                     │
│                                                                              │
│ ID    TEMP    UTIL   MEM USED / TOTAL                                        │
│  0    52°C      0%   456MiB / 24.0GiB (2%)                                   │
│  1    48°C     95%   22.1GiB / 24.0GiB (92%)                                 │
│  2    45°C      0%   456MiB / 24.0GiB (2%)                                   │
│ ...                                                                          │
│ Updated: 5s ago                                                              │
╰──────────────────────────────────────────────────────────────────────────────╯
 ↑/↓:nav j:jobs tab:switch q:quit
```

**Queue status icons:**
- `▶ N`: Queue runner active with N jobs queued
- `■ N`: Queue runner stopping, N jobs queued
- `○`: No queue runner active
- `-`: Status unknown (host offline or checking)

**Host details include:**
- Architecture and OS version
- CPU count and memory usage
- Load average with CPU utilization percentage
- GPU table with temperature, utilization, and memory usage
- Queue runner status and job count

**Offline hosts:** Host details are cached and persist when a host goes offline. The "Updated" timestamp shows when the host was last successfully contacted (not the last failed attempt).

The TUI automatically syncs job statuses every 15 seconds, refreshes logs for running jobs every 3 seconds, and refreshes host info every 30 seconds (configurable).

## Coordinator

The coordinator is an always-on daemon (running on studio) that centralizes job
placement decisions. When active, `weft run` writes intent files instead of
directly dispatching to hosts — the coordinator picks the best host and
dispatches the job.

```bash
# Start the coordinator daemon
weft coordinator start

# Check coordinator status
weft coordinator status

# Stop the coordinator
weft coordinator stop

# Install/uninstall as a launchd service (macOS)
weft coordinator install
weft coordinator uninstall
```

### Placement Scoring

When a job has no explicit host, the coordinator scores all eligible hosts:

1. **Hard constraints** (pass/fail): GPU class match, GPU memory fit, compute
   backend compatibility (CUDA vs MPS)
2. **Data locality** (+2 per local input): prefer hosts that already have
   required data cached
3. **Transfer cost** (up to -5): penalize hosts that would need large data
   transfers, estimated from asset sizes and host network bandwidth
4. **Utilization** (up to -4): penalize hosts with high GPU/CPU utilization or
   deep job queues
5. **Explicit host** (+10): strong preference when user specifies a host

### Pre-staging and HF downloads

Before dispatching a job, the coordinator pre-stages missing input data via
rsync. If a job needs `hf:meta-llama/Llama-3-8B` and it exists on titan but
not atlas, the coordinator transfers it before dispatch. Pre-staging is
best-effort.

If no host already has a declared `hf:` or `hf-dataset:` input, the
coordinator downloads it onto the target on-prem host before queueing the job.
Downloads use `huggingface-cli download` and first check that the target HF
cache volume has enough free space.

### Data Locality

The system tracks what data exists on which hosts:

- **HuggingFace models/datasets**: Scanned from `~/.cache/huggingface` during
  host sync (`weft sync`)
- **Job outputs**: Declared via `--output` flags, recorded automatically when
  jobs complete successfully
- **Manual scans**: `weft host data <host> --scan` updates the local inventory
- **Explicit fetch requests**: `weft data fetch ... --host ...` downloads a HF
  model or dataset onto a specific host or `localhost` and records the request
  lifecycle

```bash
# Scan a host's HF cache
weft host data titan --scan

# Ask which hosts have a model cached
weft data where hf:meta-llama/Llama-3-8B

# Download a model to a specific host
weft data fetch hf:meta-llama/Llama-3-8B --host cool100

# Download to the local machine without SSH
weft data fetch hf:meta-llama/Llama-3-8B --host localhost

# Review past and current download requests
weft data requests --host cool100
```

### Auto-Remediation

The coordinator automatically diagnoses failed jobs by scanning their logs for
known error patterns and, when possible, fixes the issue and retries.

**Data errors** (auto-retried):
- Missing HuggingFace model/dataset: pre-stages the data from another host, then retries
- Missing file in working directory: re-syncs sources, then retries

**Code errors** (diagnosed, optionally fixed):
- `ModuleNotFoundError`, `ImportError`, `AttributeError`, `NameError`, `SyntaxError`
- If a coding agent is configured, it attempts to fix the code and retry

**Environment errors** (diagnosed only):
- GPU out of memory, CUDA errors

Jobs are retried at most once to prevent loops. Diagnoses are stored in the
database (`error_diagnosis` column) for inspection.

To enable the coding agent for code fixes, add to `~/.config/weft/config.toml`:

```yaml
remediation:
  coding_agent: "claude -p --allowedTools Edit Read Grep Glob Bash"
```

### Crash Safety

Processed intent IDs are persisted to SQLite. If the coordinator crashes and
restarts, it will not re-dispatch intents it already handled. Entries older than
7 days are cleaned up automatically.

## Job Estimation

Weft can predict job duration and resource usage based on historical data.

```bash
# Predict duration/resources for a command on a specific host
weft predict --host atlas 'python train.py'

# Force retrain models from all configured job databases
weft retrain

# Export training data for external analysis
weft export training-data
```

Models are stored at `~/.cache/weft/models/` and auto-retrain when 50+ new
jobs complete. Configure the training data source via `predictor.project_path`
in `~/.config/weft/config.toml`. See
[Estimation and Modeling](docs/reference/estimation.md) for the full pipeline.

### Command-to-Resource Estimation

The predictor estimates peak RSS and GPU memory from the command string and
historical data. These estimates feed into placement as:

- **Hard constraints**: Host is ineligible if predicted peak RSS (95th
  percentile upper bound) exceeds host RAM, or predicted GPU memory exceeds the
  largest GPU on the host.
- **Soft constraints**: Penalty (up to -2.0) when predicted resource usage
  exceeds 80% of currently free RAM or GPU memory.

### Cost-Optimal Cloud Bidding

The `weft campaign launch` command selects the cheapest cloud instance likely to
survive the predicted job duration. A Beta-Binomial survival model learns
price-reliability curves from campaign history — cheaper instances fail more
often, so the bidding system balances cost against the probability of completion.

```bash
# Launch with automatic instance selection
weft campaign launch

# Dry-run to see the cost plan
weft campaign launch --dry-run
```

### Transfer Bandwidth Learning

Weft learns per-(source, destination) transfer bandwidth from observed file
transfers using an exponential moving average. Learned bandwidth is used in:

- **Placement scoring**: Estimating data transfer time to candidate hosts
- **Campaign cost estimation**: Predicting total wall time including transfers

Bandwidth estimates start from static inventory values and converge as transfers
are observed. See [Future Ideas](docs/planning/IDEAS.md) for planned
improvements.

## Cluster Dashboard

The web UI includes a `/cluster` page showing:

- **Host cards** with GPU specs, memory, and online/offline status
- **GPU utilization bars** with live data when the monitor is running
- **Coordinator status** (running/stopped, queue depth, processed count)
- **Recent decisions** from the operations log (placements, transfers, errors)

```bash
# Start web UI and open cluster dashboard
weft web --open
# Then navigate to http://localhost:8127/cluster
```

### API Endpoints

The web UI exposes JSON API endpoints:

- `GET /api/hosts` — host inventory with live GPU utilization
- `GET /api/coordinator` — coordinator status (running, queue depth)
- `GET /api/oplog` — recent operations log entries

## Configuration

Configuration is stored in `~/.config/weft/config.toml`.

### Default Command

By default, running `weft` with no arguments shows the help message. You can change this to run a different command:

```toml
# ~/.config/weft/config.toml
default_command = "tui"
```

Valid values for `default_command`:
- `help` (default): Show help message
- `watch`: Launch the system watch TUI or plain watch, depending on terminal mode
- `tui`: Launch interactive terminal UI
- `list`: Show job list
- `web`: Launch the read-only web UI

### TUI Polling Intervals

Customize how often the TUI refreshes data:

```toml
# ~/.config/weft/config.toml
sync_interval = 15          # Seconds between job status syncs (default: 15)
log_refresh_interval = 3    # Seconds between log refreshes for running jobs (default: 3)
host_refresh_interval = 30  # Seconds between host info refreshes in hosts view (default: 30)
```

### Web UI

The TUI automatically starts a local web UI (localhost only) unless disabled:

```toml
# ~/.config/weft/config.toml
web_enabled = true
web_port = 8127
```

You can also run it directly:

```bash
weft web --open
```

### Log Caching

Completed job logs under 50KB are cached locally for faster access without SSH:

```toml
# ~/.config/weft/config.toml
log_cache_max_size = 51200  # Maximum log size to cache in bytes (default: 50KB)
log_cache_max_age = 7       # Days to keep cached logs (default: 7, 0 to disable)
```

Cached logs are stored in `~/.cache/weft/logs/` and automatically pruned during sync.

### SSH Connection

Tune SSH connection pool behavior for slow or unreliable networks:

```toml
# ~/.config/weft/config.toml
[ssh]
pool_size = 4
max_parallel = 8
connect_timeout = 30
```

Environment variables override the config file:

| Variable | Description | Default |
|----------|-------------|---------|
| `WEFT_SSH_POOL_SIZE` | Persistent sessions per host | 4 |
| `WEFT_SSH_MAX_PARALLEL` | Max concurrent SSH operations | 8 |
| `WEFT_SSH_CONNECT_TIMEOUT` | SSH connect timeout (seconds) | 10 |

Increasing `connect_timeout` also extends the session ready timeout (connect timeout + 5s).

### Vast.ai Cloud GPU

Configure cloud GPU bursting with Vast.ai and optional R2 result upload:

```toml
# ~/.config/weft/config.toml
[vastai]
default_image = "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
sync_timeout = 5

[vastai.r2]
bucket = "my-results-bucket"
account_id = "..."
access_key_id = "..."
secret_access_key = "..."
```

### Source Excludes

Campaign source tarballs and `weft sync` share the same exclude rules.

Global defaults:
- Built-in excludes still cover VCS metadata, virtualenvs, caches, `output/`, and `outputs/`.
- App-level defaults now also exclude `runs`, `wand`, and `wandb`.

App-level overrides live in `~/.config/weft/config.toml`:

```toml
[sync]
exclude_dirs = ["lab-notebook"]
```

Per-project overrides live in `$PROJECT/.weft.toml`:

```toml
[sync]
exclude_dirs = ["data"]

[outputs]
dirs = ["results/"]
max_auto_sync_mb = 200
```

Project excludes are added on top of the global defaults. This is useful for
research repos that keep large datasets or experiment artifacts alongside code.

Cloud instances automatically sync your project sources and collect detailed
telemetry (CPU, memory, GPU usage, failure detection).

Requires the `vastai` CLI: `pip install vastai && vastai set api-key YOUR_KEY`.

## Job Database

Jobs are tracked in a local SQLite database at `~/.config/weft/jobs.db`. The database records:
- Unique job ID (used to identify tmux sessions as `weft-{id}`)
- Host
- Working directory and command
- Optional description
- Start time and end time
- Exit code and status

Log files are stored on remote hosts at `~/.cache/weft/logs/{id}-{timestamp}.log`.

**Job statuses:**
- `starting`: Job is being set up (transient state)
- `running`: Job is currently executing on the remote host
- `completed`: Job finished (check exit code for success/failure)
- `dead`: Job terminated unexpectedly without capturing exit code
- `queued`: Job waiting in a remote queue for scheduling
- `queued` (unplaced): No local host matches constraints; awaiting cloud GPU launch via TUI
- `failed`: Job failed to start (e.g., connection error)

The database is automatically created on first use and updated when checking job status.

## Manual Monitoring

View last 50 lines of a job's output (replace `42` with actual job ID):
```bash
weft log 42
```

Follow log output in real-time:
```bash
weft log 42 -f
```

## Web UI (Read-only)

Start the read-only web monitor on localhost:

```bash
weft web
```

Open it in your browser:

```bash
weft web --open
```

The web UI has two pages:
- **Jobs** (`/`) — job list with status, host, duration, and log links
- **Cluster** (`/cluster`) — host overview with GPU utilization, coordinator
  status, and recent placement decisions

Press `Ctrl+C` to stop following.

## Slack Notifications

To receive Slack notifications when jobs complete:

### 1. Create a Slack App with Incoming Webhook

1. Go to [https://api.slack.com/apps](https://api.slack.com/apps)
2. Click "Create New App" → "From scratch"
3. Name your app (e.g., "Weft") and select your workspace
4. In the sidebar, click "Incoming Webhooks"
5. Toggle "Activate Incoming Webhooks" to On
6. Click "Add New Webhook to Workspace" at the bottom
7. Select the channel where you want notifications
8. Click "Allow"
9. Copy the Webhook URL (starts with `https://hooks.slack.com/services/`)

### 2. Configure the Webhook

Use either method:

**Environment variable** (add to your shell profile):
```bash
export WEFT_SLACK_WEBHOOK="https://hooks.slack.com/services/T.../B.../..."
```

**Config file:**
```bash
mkdir -p ~/.config/weft
echo "SLACK_WEBHOOK=https://hooks.slack.com/services/..." > ~/.config/weft/config
```

### 3. Optional: Configure When to Notify

By default, you'll receive notifications for all jobs. You can customize this with environment variables:

**Notification Mode:**
```bash
# Notify for all jobs (default)
export WEFT_SLACK_NOTIFY="all"

# Notify only for failures
export WEFT_SLACK_NOTIFY="failures"

# Disable notifications
export WEFT_SLACK_NOTIFY="none"
```

**Minimum Duration Threshold:**
```bash
# Default: 15 seconds - jobs shorter than this won't trigger notifications
# (Failed jobs always notify regardless of duration)

# Notify for all jobs regardless of duration
export WEFT_SLACK_MIN_DURATION="0"

# Only notify for jobs longer than 1 minute
export WEFT_SLACK_MIN_DURATION="60"

# For longer jobs only (5 minutes)
export WEFT_SLACK_MIN_DURATION="300"
```

**Verbose Mode:**
```bash
# Include working directory and command in notification
export WEFT_SLACK_VERBOSE="1"
```

### What You'll Get

Notifications include:
- Job name and host
- Success (✓) or failure (✗) status with exit code
- Duration
- (Verbose mode) Working directory and command

## Requirements

- tmux must be installed on the remote host
- SSH access configured in `~/.ssh/config`
- curl on remote host (for Slack notifications)

## How It Works

1. `weft run` writes an intent file (or queues directly if coordinator is unreachable)
2. The coordinator scores hosts and dispatches the job to the best one
3. The Go agent on the remote host picks up the job, allocates GPU, runs it in tmux
4. Your laptop can sleep, disconnect, or travel — the agent runs autonomously
5. `weft tui`, `weft log`, or `weft job status` lets you monitor from anywhere

## Documentation

See [docs/README.md](docs/README.md) for the documentation map.

### User guides

- [Workflow Guide](docs/guides/workflow-guide.md) - Common workflows with examples
- [Campaigns](docs/guides/campaigns.md) - Cloud GPU launch, watch, and instance management
- [Network Resilience](docs/guides/network-resilience.md) - Offline-safe execution and monitoring behavior
- [Debugging](docs/guides/debugging.md) - Remote logs, queue state files, and process inspection

### Reference

- [Command Reference](docs/reference/commands.md) - Full CLI command reference
- [Job Plans](docs/reference/job-plans.md) - YAML plan format and dependency semantics
- [Logging and Progress](docs/reference/logging-and-progress.md) - Live log streaming and progress parsing
- [Estimation and Modeling](docs/reference/estimation.md) - Runtime, resource, transfer, and cost estimation

### Architecture and design

- [Architecture](docs/design/architecture.md) - System structure and component responsibilities
- [Coordinator Architecture](docs/design/coordinator-architecture.md) - Placement, data locality, and daemon design
- [Comparison to SLURM](docs/design/comparison-to-slurm.md) - Tradeoffs versus HPC schedulers
- [Placement Telemetry](docs/design/placement-telemetry.md) - Recorded placement context for offline analysis

### Development and planning

- [Agent Deployment](docs/development/agent-deployment.md) - Rebuilding and deploying the remote agent
- [Manual Testing Runbook](docs/development/manual-testing-runbook.md) - Manual validation procedures
- [Campaign Roadmap](docs/planning/ROADMAP.md) - Campaign-system gaps and priorities
- [Future Ideas](docs/planning/IDEAS.md) - Unscheduled feature ideas
