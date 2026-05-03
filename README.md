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
  `weft instance launch` provisions Vast.ai or RunPod instances, runs jobs,
  and tears down on completion. Failed jobs enter a grace period for
  resubmission
- **Autopilot**: An auto-mode runs continuously across the open TUIs (and on
  demand via `weft autopilot run`), placing unplaced jobs, launching cloud
  instances, and relaunching orphans. A runaway breaker pauses unattended
  relaunches when a scope churns without progress; inspect with
  `weft autopilot blocked` and reset with
  `weft autopilot blocked --unblock`
- **Auto-relaunch on instance failure**: When a cloud instance fails or hits an
  infrastructure error, `weft instance watch` automatically relaunches orphaned
  jobs on a new instance (up to 3 attempts per job), bounded by configurable
  retry time/cost limits
- **Stall detection**: Adaptive timeouts detect stuck instances (bootstrap stall,
  setup phase stall) using survival analysis on historical durations — thresholds
  are learned per command and workspace, with automatic fallback
- **Instance management**: Launch, watch, and terminate cloud instances from
  the CLI or TUI. `weft instance watch` streams live status, cost, and per-job
  progress until all instances finish. *Campaigns* are the batch record that
  groups instances launched together (used for bulk listing and termination)
- **System watch**: `weft watch` shows all active cloud instances, on-prem jobs,
  and unplaced jobs in a simplified TUI or periodic plain-text summary
- **Progress tracking**: Automatic parsing of `Progress:` lines, tqdm bars,
  and epoch counters. Multi-phase jobs (where progress resets between training
  configs) show Bayesian-estimated overall progress with an `≈` prefix
- **Web dashboard**: Browser UI at `localhost:8127/cluster` with host cards,
  live GPU utilization bars, coordinator status, and recent placement decisions

#### Placement Planner

`weft instance launch` (deprecated aliases: `weft place`,
`weft campaign launch`) opens an interactive planner:

```
Rental GPU jobs (12 jobs, 3 GPU groups)

  [x] A100 (3 jobs)
  [x]   42  Llama-3 70B fine-tune on RedPajama (LoRA, 3 epochs)
  [x]   43  Mixtral 8x7B inference benchmark (batch=64)
> [ ]   44  GPT-NeoX 20B ablation: attention heads vs throughput
  [-] RTX 4090 (5 jobs)
  ...

── Cost Estimate (s to cycle strategy) ────────────────────────────
  cheap      2  ~5h (2h–10h)    ~$2.24 ($1.04–$4.48)
► fast       2  ~1h30 (30m–4h)  ~$5.60 ($2.10–$11.20)
  fastest    2  ~45m (15m–2h)   ~$8.40 ($3.15–$16.80)

↑/↓ navigate  space toggle  a all  n none  d details  enter launch  q quit
```

#### Instance Watch

Monitor running instances with `weft instance watch`:

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

`weft instance watch` (alias: `weft campaign watch`) watches a single campaign.
If you omit the campaign ID, it watches the most recent campaign. Use `--tui` or
`--plain` to override the default terminal-based mode selection.

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

Press `l` in the watch TUI to jump into the cloud launch planner.

Post-launch scope behavior:
- `weft instance launch` (deprecated aliases: `weft place`,
  `weft campaign launch`) watches the just-launched batch's instances
  (including relaunch replacements) and shows all unplaced jobs.
- `weft project launch` (alias: `weft launch project`) watches
  project-scoped instances (including relaunch replacements) and filters
  unplaced jobs to that project.

#### Placement and instance commands

The primary command for launching cloud instances for unplaced jobs is
`weft instance launch`:

```bash
weft instance launch [job-id...] [--all|--project NAME] \
  [--dry-run|--yes|--watch|--no-watch|--plain|--tui] \
  [--strategy cheap|fast|fastest] [--max-spend USD] [--max-time DURATION] \
  [--grace-period DURATION]
weft instance watch [campaign-id] [--plain|--tui]
weft instance list [--plain|--tui]
weft instance ssh <instance-id>
```

A *campaign* is the batch record that groups the instances launched together,
useful for bulk inspection and termination:

```bash
weft campaign list [--plain|--tui]
weft campaign show <campaign-id>
weft campaign terminate <campaign-id>
```

Deprecated aliases for `weft instance launch`: `weft place`,
`weft campaign launch`. `weft campaign watch` is also equivalent to
`weft instance watch`. New workflows should prefer the `instance` forms.

#### Project Commands

Project commands default to the current directory's project. Pass a project
name as the first argument to override.

```bash
weft project launch [project-name] [--yes|--watch|--dry-run]
weft project jobs [project-name] [--running|--failed|--completed]
weft project watch [project-name] [--plain|--tui]
weft project list [project-name]
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
  [Cloud GPU Instances](docs/guides/instances.md),
  [Autopilot](docs/guides/autopilot.md),
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
weft log wj42 -f
weft job list --running
weft jobs list --group-by status --unprocessed
weft plan submit plan.yaml
```

Job IDs are shown as `wj<id>` in CLI output. Commands accept both `wj42` and `42`, including mixed ranges like `wj42:47` or `wj42:wj47`.

For shell syntax, queue operations, job control, artifact commands, and advanced flags, use the dedicated reference above.

## Terminal UI

Weft has a family of focused TUIs in addition to the full-screen `weft tui`
described below. Pick the one whose scope matches what you need to watch:

| Command                                | Scope                                                                                              |
| -------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `weft tui`                             | Full-screen Jobs/Hosts split view (the all-purpose TUI; see below)                                 |
| `weft watch`                           | System-wide: every active cloud instance, on-prem jobs, unplaced jobs                              |
| `weft instance watch [id]`             | A single launch batch's instances (live cost, phase, per-job progress)                             |
| `weft project watch [name]`            | The current project's instances + unplaced jobs                                                    |
| `weft list jobs --group-by status`     | Grouped jobs list (status sections, optional `--unprocessed`/`--wait`)                             |

Several of these TUIs run an **autopilot** that auto-places, auto-launches,
and relaunches jobs in the background. While auto-mode is on, press `$` to
open the run-rate / daily-cap prompt, then `Enter` to advance to the daily
step, then `Ctrl-R` to reset the runaway breaker. From the CLI, use
`weft autopilot blocked` to see currently-paused scopes and
`weft autopilot blocked --unblock` to clear the breaker. See
[docs/guides/autopilot.md](docs/guides/autopilot.md).

### `weft tui` — full-screen Jobs/Hosts view

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
│ wj52 deepthought  ● running    2h ago       python train.py --lr 0.001       │
│ wj51 deepthought  ✗ exit 1     3h ago       python test.py                   │
│ wj50 skynet       ✓ done       yesterday    make build                       │
╰──────────────────────────────────────────────────────────────────────────────╯
╭──────────────────────────────────────────────────────────────────────────────╮
│ Details                                                                      │
│ Job wj52 on deepthought                                                      │
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
 ↑/↓:nav l:logs s:sync n:new r:restart k:kill d:draft h:hosts q:quit
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
 ↑/↓:nav l:logs s:sync n:new r:restart k:kill h:hosts q:quit
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

### Autopilot Status, Pause, and Blocked Scopes

The TUIs (list, watch) drive an autopilot that auto-places, auto-launches, and
auto-relaunches jobs. The CLI exposes a singleton state row so other terminals
and external automation can see whether autopilot is currently making
decisions, and pause it when manual control is needed:

```bash
weft autopilot status              # idle | running | stale | paused | never
weft autopilot status --json       # machine-readable
weft autopilot status --quiet      # exit codes: 0=idle, 10=running, 11=stale, 12=paused

weft autopilot pause --reason "..."
weft autopilot resume
```

Pause is sticky across restarts — every autopilot runner (TUIs, future
coordinator daemon) skips its work while paused. Use it before launching
instances or restarting orphaned jobs by hand from another terminal to avoid
racing the autopilot.

When the autopilot reports jobs blocked by `paused: repeated launch failures
without progress`, the runaway breaker has tripped. Inspect the trip metrics
(chain length, orphaned-attempt count, spend, window) and the affected jobs:

```bash
weft autopilot blocked              # list tripped scopes + jobs + metrics
weft autopilot blocked --json       # machine-readable
weft autopilot blocked --unblock    # list and reset the global breaker in one shot
weft autopilot budget reset         # reset only
```

`weft info wj<N>` also surfaces the trip inline on any paused job. See
[docs/guides/autopilot.md](docs/guides/autopilot.md) for the full
inspection-and-reset workflow.

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

For private Hugging Face inputs, store the token once and let weft attach it by
reference:

```bash
weft secret set hf "$HF_TOKEN"
weft run --hf-token --input hf:meta-llama/Llama-3-8B "python train.py"
```

On macOS, secrets are stored in the Keychain. Other platforms use
`~/.config/weft/secrets.json` with owner-only permissions. Jobs store
`secret:<name>` references and weft resolves them only when launching a job.

For setup-heavy rental jobs, declare runtime disk headroom separately from data
inputs:

```bash
weft run --input hf:org/model --runtime-disk 24 "uv sync --project scripts/vllm-profiling && python bench.py"
```

Use `--disk <gb>` to force the total rental disk floor. Weft also estimates
runtime cache/build headroom for common `uv`, pip, CUDA, and vLLM commands.

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

# Show whether the predictor is ready, rebuilding, or blocked by schema mismatch
weft estimation status

# Force retrain models from all configured job databases
weft retrain

# Export training data for external analysis
weft export training-data
```

Models are stored at `~/.cache/weft/models/`. When 50+ new jobs complete, weft
rebuilds stale but schema-compatible models in the background and keeps using
the current compatible model until the swap completes. If the stored model
schema is incompatible with the current code, prediction is blocked until the
rebuild finishes. Configure the training data source via
`predictor.project_path` in `~/.config/weft/config.toml`. See
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

The `weft instance launch` command (deprecated aliases: `weft place`,
`weft campaign launch`) selects cloud instances using one of three
strategies (`--strategy`):

- **cheap** (default): Minimizes expected dollar cost including retry risk from
  instance failure, using the Beta-Binomial survival model.
- **fast**: Minimizes expected wall-clock time, weighting DLPerf by survival
  probability.
- **fastest**: Picks the highest raw DLPerf, ignoring the survival model entirely.

A Beta-Binomial survival model learns price-reliability curves from launch
history — cheaper instances fail more often, so the `cheap` and `fast`
strategies balance their objective against the probability of completion.
The model partitions same-family GPUs by VRAM (so a 4090 24GB and a 4090
48GB are scored separately), tracks per-machine reliability (physical
machines with a history of failures are penalized), and applies a
hierarchical geographic adjustment (country → region → machine). All
counts are exponentially decayed by recency so a stale exclusion from a
transient outage decays out within a few cycles instead of being a
permanent ban.

Offers with survival probability below a configurable floor (`--min-survival`,
default 40%) are rejected entirely — GPU classes or machines that consistently
fail won't be selected regardless of price. Use `--min-survival 0` to disable.

Use `weft campaign survival` to inspect the model: posterior survival
probabilities by GPU family and price bucket, and per-machine penalties.

Press `s` in the TUI to cycle between strategies.

```bash
# Launch with automatic instance selection (default: cheap)
weft instance launch

# Launch with fastest strategy
weft instance launch --strategy fastest

# Disable survival floor (allow all offers)
weft instance launch --min-survival 0

# Show the survival model
weft campaign survival

# Dry-run to see the cost plan
weft instance launch --dry-run
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

### Cloud SSH Identity

For unattended cloud runs, use a dedicated SSH key that does not require
1Password, Touch ID, or another interactive approval:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/weft_cloud_ed25519 -N "" -C "weft cloud automation"
vastai create ssh-key ~/.ssh/weft_cloud_ed25519.pub -y
runpodctl ssh add-key --key-file ~/.ssh/weft_cloud_ed25519.pub
```

Then configure Weft to use it for cloud SSH:

```toml
# ~/.config/weft/config.toml
[cloud.ssh]
identity_file = "~/.ssh/weft_cloud_ed25519"
public_key_file = "~/.ssh/weft_cloud_ed25519.pub"
```

Weft adds `BatchMode=yes`, `IdentitiesOnly=yes`, and `-i <identity_file>` to
cloud SSH commands, attaches the public key to new Vast.ai instances, and
registers it with RunPod before pod creation.

### Per-Host Overrides

Use the `hosts` table in `~/.config/weft/config.toml` for per-host overrides:

```toml
[hosts.cool30]
shared = true

[hosts.cool100]
backend = "queue-runner"
shared = true
```

Hosts marked `shared = true` are treated as multi-tenant for placement.
Auto-placed `benchmark` jobs skip them unless the job is explicitly
inventory-tagged. Direct `--host` submissions still target the named host.

Queue-runner setup commands (for example `uv sync` or `direnv allow`) are
time-limited to prevent hung installs:
- Default setup timeout: `20m`
- Per-host override: set `setup_timeout` in
  `~/.config/weft/hosts/<host>.yaml`

```yaml
# ~/.config/weft/hosts/cool100.yaml
name: cool100
setup_timeout: 90m
```

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

### Campaign Retry Limits

Set hard retry guardrails for cloud relaunches in watch mode (auto and manual
`r` retries). For each retry tier, weft stops when either time or cost is
reached, whichever comes first.

```toml
[campaign]
reliability = 0.95           # Default provider-offer reliability floor (0 disables)
retry_first_time_limit = "45m" # First retry tier
retry_first_cost_limit = 1.0    # USD
retry_next_time_limit = "45m"   # Second+ retry tiers
retry_next_cost_limit = 0.25    # USD
```

`campaign.reliability` controls the minimum provider reliability accepted during
offer search. Default is `0.95`; set to `0` to disable reliability filtering.

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

[cloud]
image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"  # Override default Docker image
min_driver = "535"                              # Optional NVIDIA driver floor
min_cuda = "12.9"                               # Optional CUDA compatibility floor
image_pull_secret = "ghcr.io"                   # Optional [registry] key
```

Project excludes are added on top of the global defaults. This is useful for
research repos that keep large datasets or experiment artifacts alongside code.

The `[cloud] image` setting overrides the global `vastai.default_image` for jobs
from this project. When a campaign contains jobs from multiple projects with
different images, weft automatically splits instance groups so each instance uses
the correct image.

For curated or private images, configure registry credentials in the app config:

```toml
[registry."ghcr.io"]
username = "osteele"
password_env = "WEFT_GHCR_TOKEN"
runpod_auth_name = "weft-ghcr" # optional
```

Weft matches private image credentials by registry hostname unless
`image_pull_secret` names a specific `[registry]` entry. It also reads
`NVIDIA_REQUIRE_CUDA` from image metadata when available and applies driver/CUDA
floors during cloud placement; explicit `min_driver` and `min_cuda` values are
fallbacks or stricter overrides.

To inspect what will actually be included, run:

```bash
weft sync inspect                 # Summary for the current directory
weft sync inspect --show-excludes # Also print the effective exclude patterns
weft sync inspect --json          # Machine-readable output
```

Cloud instances automatically sync your project sources and collect detailed
telemetry (CPU, memory, GPU usage, failure detection). Benchmark jobs
(`--tag benchmark`) get additional telemetry controls: a benchmark barrier
waits for background uploads to finish before the job starts (preventing I/O
interference), and an optional GPU warmup pass initializes CUDA contexts
before measurement begins. See [Campaigns](docs/guides/campaigns.md) for
telemetry details and configuration.

Requires the `vastai` CLI: `pip install vastai && vastai set api-key YOUR_KEY`.

### RunPod Cloud GPU

RunPod can be enabled for cloud offer search independently from launch-time
template setup.

```toml
# ~/.config/weft/config.toml
[runpod]
enabled = true
default_image = "nvidia/cuda:12.4.1-runtime-ubuntu22.04"
```

Install and authenticate the CLI first:

```bash
brew install runpodctl
runpodctl doctor
```

Then use the built-in setup flow:

```bash
weft runpod doctor
weft runpod template print-bootstrap
weft runpod setup
```

`weft runpod setup` enables RunPod, creates or reuses a compatible bootstrap
template, and writes `runpod.bootstrap_template_id` into
`~/.config/weft/config.toml`.

RunPod launches still use the shared R2 bootstrap configuration under
`[vastai.r2]`, so `weft runpod doctor` also validates that block before
reporting launch readiness.

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
- `queued` (unplaced): No local host matches constraints; awaiting rental GPU launch via TUI
- `failed`: Job failed to start (e.g., connection error)

The database is automatically created on first use and updated when checking job status.

Reserved scheduler tags such as `rental`, `inventory`, `benchmark`, and
`interruptible` steer placement. See the
[Placement guide](docs/guides/placement.md#reserved-tags) for the catalog.

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
