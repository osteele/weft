# Weft

[![Go Reference](https://pkg.go.dev/badge/github.com/osteele/weft.svg)](https://pkg.go.dev/github.com/osteele/weft)
[![Go Report Card](https://goreportcard.com/badge/github.com/osteele/weft)](https://goreportcard.com/report/github.com/osteele/weft)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A coordinator-based workload scheduler for GPU compute clusters, with resource
inventory, data locality awareness, and automatic job placement.

## Overview

Weft manages jobs across a cluster of GPU hosts. Submit a job from your laptop
without specifying a host, and the coordinator places it on the best available
machine based on GPU capabilities, data locality, current utilization, and queue
depth. Jobs run in tmux sessions that survive SSH disconnections, laptop sleep,
and network outages.

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
  hosts with the data cached, or pre-stages it via rsync
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

### Designed for unreliable networks

Laptops move between Wi-Fi networks, VPNs flap, and SSH servers occasionally
drop. Weft treats those scenarios as normal operations:

- Jobs always start locally first, so connection failures never lose metadata.
- Failed SSH attempts automatically defer work to the host queue (or the local
  pending list) and are replayed by `weft sync` when the host returns.
- Blocking commands such as `status --wait` and `plan submit --wait` keep
  polling while the host is down and announce when the connection comes back.

See [docs/network-resilience.md](docs/network-resilience.md) for the full story
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
same scoring logic. See [Comparison to SLURM](docs/comparison-to-slurm.md) for
how this differs from centralized job managers.

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

## Commands

### weft run

Queue a job on a remote host for managed execution.

```bash
weft run [flags] [host] <command...>
```

The host is optional when a coordinator is running — the placement engine
automatically selects the best host based on GPU constraints, data locality,
current utilization, and queue depth.

By default, jobs are added to a queue and scheduled by the queue runner. It can run multiple jobs on a host while keeping total CPU usage under a target cap. Use `--immediate` (`-i`) to start a job immediately.

Use `start <job-id>` to start a queued job immediately.

**Flags:**
- `-i, --immediate`: Start job immediately instead of queuing
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-m, --message TEXT`: Description of the job (for logging and queries)
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--tag TAG`: Tag to attach to the job (can be repeated). Special tags: `exclusive` makes the job run alone (waits for other jobs to finish, blocks new jobs while running); `benchmark` like exclusive but also waits for system-wide idle (low CPU, RAM, GPU, VRAM)
- `--draft`: Record the job locally in draft status (never contacts the host until you later promote it)
- `-f, --follow`: Follow log output after starting (requires `--immediate`)
- `--allow`: Stream the job log live and stay attached (requires `--immediate`)
- `--from ID`: Copy settings from existing job ID (allows overriding)
- `--timeout DURATION`: Kill job after duration (e.g., "2h", "30m", "1h30m")
- `--after, --depends-on ID`: Start job after another job succeeds
- `--after-any ID`: Start job after another job completes, success or failure
- `--kill ID`: Kill a job by ID (synonym for `weft kill`)
- `--input ASSET`: Declare a data input (e.g., `hf:meta-llama/Llama-3-8B`). Influences placement scoring and triggers pre-staging
- `--output ASSET`: Declare a data output (e.g., `checkpoint:llama-ft-v1`). Recorded on successful completion for downstream jobs
- `--gpu CLASS`: GPU constraint with optional memory (e.g., `a100`, `ampere+`, `nvidia>=24GB`)
- `--gpu-class CLASS`: Require a specific GPU class or generation (e.g., `a100`, `ampere+`)
- `--gpu-mem GB`: Require minimum GPU memory in GB
- `--produces PATH`: Artifact path this job produces (repeatable, e.g., `output/model.pt`)
- `--needs PATH:VERSION`: Artifact path:version this job needs (repeatable, e.g., `output/model.pt:100`)
- `--dry-run`: Show placement scores without submitting the job
- `--no-sync`: Skip source sync before submission
- `--wait`: Wait for the job to complete before returning

If an immediate run can't reach the host, the CLI automatically records the job
locally and defers it to the remote queue. The next sync (or any command that
touches that host) will append the saved entry so it runs as soon as the host is
reachable again.

Draft jobs (`--draft`) stay entirely local. They’re useful for capturing a job
definition you want to tweak later or to keep certain jobs from ever syncing to
the host. When you’re ready to discard the draft, run `weft job draft <id>`
to toggle the status (or do it from the TUI, described below).

**Examples:**
```bash
# Queue a job (default behavior)
weft run deepthought 'python train.py'

# Start a queued job immediately
weft start 123

# Start immediately instead of queuing
weft run -i deepthought 'python train.py'

# With description (recommended)
weft run -m "Training GPT-2 with lr=0.001" deepthought 'with-gpu python train.py --lr 0.001'

# Explicit working directory
weft run -C /mnt/code/LM2 deepthought 'with-gpu python train.py'

# Start immediately and follow log output
weft run -i -f -m "Training run" deepthought 'python train.py'

# Stay attached to live output (Ctrl+C detaches, job keeps running)
weft run -i --allow -m "Training run" deepthought 'python train.py'

# Set environment variables
weft run -e CUDA_VISIBLE_DEVICES=0 -e BATCH_SIZE=32 deepthought 'python train.py'

# Tag jobs for later filtering
weft run --tag exp-012 --tag notebook-sync deepthought 'python train.py'

# Run a job exclusively (waits until no other jobs are running, blocks others while running)
weft run --tag exclusive deepthought 'python large_model.py'

# Run a benchmark (exclusive + waits for system-wide idle: low CPU, RAM, GPU, VRAM)
weft run --tag benchmark deepthought 'python bench_encode.py'

# Run job after another succeeds
weft run --after 42 deepthought 'python eval.py'

# Run cleanup job after another completes (success or failure)
weft run --after-any 42 deepthought 'python cleanup.py'

# Artifact-based dependencies (file-path deps instead of job-ID deps)
# Job 100: training — declares it will produce a checkpoint
weft run --produces output/model.pt -m "Fine-tune Llama-3 8B" 'python train.py'

# Job 101: eval — depends on job 100's checkpoint
weft run --needs output/model.pt:100 -m "Eval on MMLU" 'python eval.py'

# Job 100 fails. Retry as job 102, replacing version 100:
weft run --produces output/model.pt:100 -m "Fine-tune (retry)" 'python train.py --resume'
# Job 101 automatically picks up job 102's output — no re-editing needed

# Kill a job
weft run deepthought --kill 42
```

The `--produces`/`--needs` flags let you build multi-step pipelines without
hard-coding job IDs into downstream commands. When a producer fails and you
retry it with a version suffix (`:100`), all consumers keyed to that version
pick up the replacement automatically.

### weft artifact

Track and retrieve job outputs through a durable local artifact store.

Artifacts are declared by writing a manifest on the remote host. The CLI
syncs those files into `~/.config/weft/artifacts/` so they survive
remote cleanup.

**Manifest format:**
```json
{
  "job_id": 2073,
  "artifact_root": ".",
  "artifacts": [
    {"name": "selectivity_results", "path": "output/selectivity_results.json"},
    {"name": "probe_ckpt", "path": "runs/roberta-base/probe.pt"}
  ]
}
```

**Environment variables available to job scripts:**
- `WEFT_JOB_ID` — the job ID
- `WEFT_ARTIFACT_MANIFEST` — path to the artifact manifest (default: `~/.cache/weft/artifacts/<job-id>.json`)
- `WEFT_ARTIFACT_ROOT` — artifact root directory (default: `.`)
- `RJ_JOB_ID`, `RJ_ARTIFACT_MANIFEST`, `RJ_ARTIFACT_ROOT` — legacy aliases (same values)

**Examples:**
```bash
# Sync artifacts for job 2073 into the local store
weft artifact sync 2073

# Sync all outstanding artifacts across jobs
weft artifact sync

# List cached artifacts
weft artifact list 2073

# Retrieve by name or path
weft artifact get 2073 selectivity_results -o ./results.json
weft artifact get 2073 output/selectivity_results.json -o ./results.json

# Write artifact to stdout
weft artifact get 2073 selectivity_results -o -
weft artifact cat 2073 selectivity_results | jq '.metric'

# Resolve latest job by tag
weft artifact get --tag exp-012 --latest selectivity_results -o ./results.json
```

#### Automatic output collection

Jobs automatically track files written to `output/` or `outputs/` in the
working directory. On successful completion, the runner records discovered files
in the completion record and auto-syncs small outputs (< 100 MB) back to the
coordinator.

Customize output directories and the auto-sync threshold in `.weft.yaml`:

```yaml
outputs:
  dirs: ["results/"]         # Default: ["output/", "outputs/"]
  max_auto_sync_mb: 200      # Default: 100
```

Use `weft artifact list <job-id>` to see discovered outputs and
`weft artifact sync <job-id>` to pull them from the remote host on demand.

The command:
- Creates a job ID and adds it to the remote queue (or starts immediately with `-i`)
- Queue runner schedules queued jobs in FIFO order (subject to CPU allotments)
- Saves job metadata and logs to `~/.cache/weft/logs/` on the remote host
- Records the job in a local SQLite database (`~/.config/weft/jobs.db`)
- Captures exit code when job completes
- Sends Slack notification on completion (if configured)
- Returns immediately (non-blocking)
- Prints the job ID and instructions for starting immediately or monitoring

### weft plan submit

Submit a YAML job execution plan that can mix one-off jobs, parallel groups,
and queue-backed series.

```bash
weft plan submit plan.yaml
weft plan submit --host studio plan.yaml   # provide default host via CLI
weft plan submit - < generated-plan.yaml   # read from stdin / heredoc
```

Plan files must start with `version: 1` to opt into the current schema and
remain compatible with future releases.

Plan files support an optional `kill` list, single `job` entries, `parallel`
groups, and `series` groups. Every block and job may declare `id`, `alias`,
`depends_on`, and `continue_on_failure`. The CLI resolves these references into
a DAG, auto-generating IDs (`block0`, `block0.job0`, etc.) when missing, so you
can declare multi-phase pipelines in a single YAML file. Provide `--host <name>`
to supply a default host for jobs that omit it, and add `--wait <duration>` to
keep the CLI around and report which jobs finished. See `docs/job-plans.md` for
the full schema plus dependency examples.

Inspect or lint a plan without running it:

```bash
weft plan validate plan.yaml
weft plan show --ids plan.yaml   # show generated IDs, aliases, hosts, deps
```

> **Agents welcome:** Weft (and the plan syntax in particular) was
> designed for coding agents as well as humans. The YAML shape is easy for an
> agent to emit directly from a prompt, so consider giving your agent runtime a
> skill/instruction that invokes `weft plan submit` with generated plans.
> This lets automated assistants spin up, chain, and monitor jobs using the same
> dependency and queueing logic described below.

### weft job status

Check the status of one or more jobs by ID.

```bash
weft job status <job-id>...
weft job status --wait 42         # block until the job finishes
weft job status --wait --wait-timeout 30m 42
weft job status --wait 42 43 44   # wait for all (exits 0 only if all succeed)
```

**Job ID syntax:**
- Single IDs: `42`, `43`, `44`
- Ranges: `42:45` (expands to 42, 43, 44, 45)
- Mixed: `42 50:53 60` (expands to 42, 50, 51, 52, 53, 60)

Duplicate IDs are automatically removed with a warning.

**Exit codes (single job only):**
- `0`: Job completed successfully
- `1`: Job failed or error
- `2`: Job is still running
- `3`: Job not found

**Examples:**
```bash
weft job status 42           # Check status of job #42
weft job status 42 43 44     # Check multiple jobs
weft job status 100:105      # Check jobs 100 through 105
```

This command:
- First checks the local database for terminated jobs
- Only queries the remote host if the job is still running
- Updates the database if status has changed
- Use `--wait` (with optional `--wait-timeout`) to block until jobs finish.
  The command exits with `0` only if every waited-on job succeeds.

### weft job list

Query and search job history from the local database.

```bash
weft job list [flags]
```

**Flags:**
- `--running`: Show only running jobs
- `--completed`: Show only completed jobs
- `--queued`: Show only queued jobs
- `--dead`: Show only dead jobs
- `--status STATUS`: Filter by status (`running`, `completed`, `queued`, `dead`, `processed`, `unprocessed`)
- `--host HOST`: Filter by host (replaces old `check <host>` command)
- `--search QUERY`: Search by description or command
- `--tag TAG`: Filter by tag (can be repeated)
- `--limit N`: Limit results (default: 50)
- `--show ID`: Show detailed info for a specific job
- `--cleanup DAYS`: Delete jobs older than N days
- `--sync`: Sync job statuses from remote hosts before listing

**Examples:**
```bash
weft job list                          # Recent jobs
weft job list --running                # Running jobs
weft job list --running --sync         # Running jobs (sync first)
weft job list --host deepthought       # Jobs on deepthought
weft job list --tag exp-012            # Jobs with a tag
weft job list --status unprocessed     # Jobs missing the processed tag
weft job list --search training        # Search jobs
weft job list --show 42                # Job details
weft job list --cleanup 30             # Remove old jobs
```

### weft tag

Attach or remove tags on jobs stored in the local database.

```bash
weft tag add <job-id> <tag>
weft tag rm <job-id> <tag>
```

**Examples:**
```bash
weft tag add 42 exp-012
weft tag rm 42 exp-012
```

### weft mark-processed

Mark a job as processed by adding the reserved `processed` tag.

```bash
weft mark-processed <job-id>
```

**Examples:**
```bash
weft mark-processed 42
weft job list --status unprocessed
```

### weft sync

Sync job statuses from all remote hosts with running jobs.

```bash
weft sync [flags]
```

**Flags:**
- `-v, --verbose`: Show detailed progress

Automatically finds hosts with running jobs and updates their status in the local database. Connection failures are silently ignored (unreachable hosts are skipped).

**Examples:**
```bash
weft sync              # Sync all hosts
weft sync --verbose    # Show progress
```

### weft prune

Tombstone completed/dead jobs so they disappear from listings, and optionally delete their log files on remote hosts.

```bash
weft prune [flags]
```

**Flags:**
- `--older-than DURATION`: Only remove jobs older than this (e.g., `7d`, `24h`, `30m`)
- `--dead-only`: Only remove dead jobs (not completed)
- `--dry-run`: Preview what would be deleted without actually deleting
- `--keep-files`: Don't delete remote log files (only remove from database)

**Examples:**
```bash
weft prune                    # Tombstone all completed/dead jobs
weft prune --older-than 7d    # Only tombstone jobs older than 7 days
weft prune --older-than 24h   # Only tombstone jobs older than 24 hours
weft prune --dry-run          # Preview which jobs would be tombstoned
weft prune --dead-only        # Only tombstone dead jobs
weft prune --keep-files       # Tombstone locally but keep remote files

Tombstoned jobs remain in the database for auditing and can still be viewed with
`weft job status <id>` or `weft log <id>`, but they disappear from
`weft list` and the TUI.
```

### weft log

View the full log file for a job.

```bash
weft log <job-id> [flags]
```

**Flags:**
- `-f, --follow`: Follow log in real-time (like `tail -f`)
- `-n, --lines N`: Number of lines to show (default: 50)
- `--from N`: Show lines starting from line N
- `--to N`: Show lines up to line N
- `--grep PATTERN`: Filter lines matching pattern

**Examples:**
```bash
weft log 42           # Last 50 lines
weft log 42 -f        # Follow (like tail -f)
weft log 42 -n 100    # Last 100 lines
weft log 42 --from 100 --to 200  # Lines 100-200
weft log 42 --from 500           # From line 500 onwards
weft log 42 --to 100             # First 100 lines
weft log 42 --grep error         # Lines containing "error"
weft log 42 -f --grep epoch      # Follow, filter for "epoch"
```

**Notes:**
- `--from`/`--to` cannot be used with `-n`/`--lines`
- `--follow` cannot be used with `--to`
- `--grep` can be combined with any other option

### weft job restart

Restart a job using its saved metadata.

```bash
weft job restart <job-id>
```

This kills the existing session (if any) and starts a new one with the same command and working directory, creating a new job ID.

**Note:** For most use cases, `run --from <id>` is more flexible as it allows overriding settings.

### weft retry

Clone a previous job and queue it again with the same host, directory, command, description, and environment variables. Dependencies are not copied so the retried job starts as soon as it reaches the front of the queue.

```bash
weft retry <job-id>
weft job retry <job-id>   # Alias
```

The command prints the new job ID and whether it was queued immediately or deferred until the host is online.
Queued jobs that previously depended on the retried job automatically update their dependency to the new job ID.

### weft job move

Move a queued job to a different host.

```bash
weft job move <job-id> <new-host>
```

This command updates the host for a job that hasn't started yet (status=queued). Useful when you've queued work but want to run it on a different machine.

**Examples:**
```bash
weft job move 42 atlas   # Move job 42 to atlas
weft job move 43 studio    # Move job 43 to studio
```

### weft job start

Start a queued job immediately, bypassing its queue order. The job is removed
from the remote queue file, marked as running, and launched right away.

```bash
weft job start <job-id>
weft run <job-id>           # Shorthand (same effect)
```

Examples:
```bash
weft run 512                # Start queued job 512 immediately
weft job start 512          # Same as above
weft job start 9001         # Bypass queue order and run now
```

Only jobs with status `queued` can be started this way. The command preserves
the job's working directory, environment variables, and metadata.

**Note:** For running or completed jobs, use `run --from <id>` to create a new job on the desired host.

### Advanced run options

The `run` command supports several advanced options for more control:

**Copy settings from existing job (`--from`)**:
```bash
weft run --from <job-id> [<host>] [<command>]
```

Copies command, working directory, and description from an existing job. You can override any of these:

```bash
weft run --from 42                    # Rerun job 42 with same settings
weft run --from 42 atlas            # Rerun on different host
weft run --from 42 --timeout 4h       # Rerun with longer timeout
weft run --from 42 atlas "python train.py --epochs 200"  # Override everything
```

**Timeout (`--timeout`)**:
```bash
weft run --timeout <duration> <host> <command>
```

Automatically kills the job after the specified duration (e.g., "2h", "30m", "1h30m"):

```bash
weft run --timeout 2h titan "python train.py"
weft run --timeout 30m --from 42      # Retry with timeout
```

**Environment variables (`-e, --env`)**:
```bash
weft run -e VAR=value <host> <command>
```

Set environment variables for the job. Can be repeated for multiple variables:

```bash
weft run -e CUDA_VISIBLE_DEVICES=0 titan "python train.py"
weft run -e BATCH_SIZE=32 -e LR=0.001 titan "python train.py"
weft queue add -e TMPDIR=/mnt/data/tmp titan "python train.py"
```

**Automatic environment file loading**:

The queue runner automatically loads environment files from the job's working directory before executing the command. Files are loaded in this order (later files override earlier ones):

1. `.env` - loaded with auto-export (`set -a`)
2. `.env.local` - loaded with auto-export (`set -a`)
3. `.envrc` - loaded without auto-export (for direnv compatibility)

The job log will show "Loading .env" etc. when these files are found and sourced.

### weft cleanup

Clean up finished sessions and old log files.

```bash
weft cleanup <host> [flags]
```

**Flags:**
- `--sessions`: Kill finished sessions only
- `--logs`: Remove archived log files only
- `--older-than N`: Only clean items older than N days (default: 7)
- `--dry-run`: Preview without actually deleting

**Examples:**
```bash
weft cleanup deepthought                    # Clean both
weft cleanup deepthought --sessions         # Only finished sessions
weft cleanup deepthought --logs --older-than 3  # Logs > 3 days old
weft cleanup deepthought --dry-run          # Preview only
```

### weft kill

Kill a running job.

```bash
weft kill <job-id>
```

**Example:**
```bash
weft kill 42    # Kill job #42
```

### weft pause

Pause a running job (SIGSTOP).

```bash
weft pause <job-id>
```

**Example:**
```bash
weft pause 42   # Pause job #42
```

### weft resume

Resume a paused job (SIGCONT).

```bash
weft resume <job-id>
```

**Example:**
```bash
weft resume 42  # Resume job #42
```

### weft job draft

Move a job into draft status and make sure it never runs remotely (queued or otherwise).

```bash
weft job draft <job-id>
```

When you draft a job:
- Running jobs are killed and removed from the remote host queue/state
- Queued jobs are removed from queue files so they won’t start later
- Offline hosts keep the “draft pending” request until the next `weft sync`
- Already-completed jobs simply change status locally

Drafting is handy when you realize a queued job shouldn’t run anymore but you
want to keep its metadata/log references around for editing or cloning later.
You can also trigger the same action from the TUI by pressing `d`.

### weft queue

Manage job queues for CPU-capped execution on remote hosts.

Jobs added to a queue are scheduled in FIFO order, and the queue runner can run multiple jobs per host while keeping total CPU usage under a target cap. The queue runner runs in a tmux session on the remote host and keeps working when you disconnect. CPU allotments and GPU memory reservations can be set via `weft job describe --cpu <percent>` and `--gpu-mem <gb>`.

#### weft queue add

Add a job to a remote queue.

```bash
weft queue add [flags] <host> <command...>
```

**Flags:**
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-d, --description TEXT`: Description of the job
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--draft`: Save a draft queue entry locally without touching the remote queue file
- `--after, --depends-on ID`: Start job after another job succeeds
- `--after-any ID`: Start job after another job completes (success or failure)

Draft queue entries behave like sticky notes: they keep the command, env vars,
and metadata in your local database while guaranteeing they never reach the
remote queue runner. Convert them later with `weft job draft <id>` (or
the `d` key in the TUI) once you decide they should be eligible to run.

**Examples:**
```bash
weft queue add titan 'python train.py --epochs 100'
weft queue add -d "Training run 1" titan 'python train.py'
weft queue add -e CUDA_VISIBLE_DEVICES=0 titan 'python train.py'
weft queue add --after 42 titan 'python eval.py'       # Run after job 42 succeeds
weft queue add --after-any 42 titan 'python cleanup.py' # Run after job 42 completes (success or failure)
```

#### weft edit

Edit a queued job’s metadata—description, working directory, command, environment variables, or dependencies.  
`weft queue edit` is an alias for this command and accepts the same flags.

```bash
weft edit [flags] <job-id>
```

**Flags:**
- `-m, --message TEXT`: Set job description
- `-C, --directory DIR`: Set working directory
- `--command CMD`: Replace the queued command
- `-e, --env VAR=value`: Replace environment variables (repeat flag to set multiple)
- `--clear-env`: Remove all environment variables
- `--depends-on ID[,ID...]`: Require the listed jobs to succeed before running
- `--depends-on-any ID[,ID...]`: Wait for the listed jobs to finish (success or failure)
- `--clear-depends`: Remove all dependencies from the job

IDs can also be suffixed with `+` or `:any` to mark them as completion-based dependencies, e.g. `--depends-on 101+` or `--depends-on 101:any`.

**Examples:**
```bash
weft edit 1595 --depends-on 1599
weft edit 1600 --depends-on 1400 --depends-on-any 1401
weft edit 1700 --clear-depends
weft edit 1800 --command "python eval.py" -C ~/project -e FOO=bar
```

Changes are validated so you can only depend on jobs that run on the same host. If the host is offline, the update is deferred like other queue operations and reapplied once it reconnects.

#### weft queue start

Start the queue runner on a remote host.

```bash
weft queue start [flags] <host>
```

The queue runner:
- Runs in a tmux session (`weft-queue-default`)
- Processes queued jobs in FIFO order with a CPU cap
- Continues running even when you disconnect
- Sends Slack notifications (if configured)

**Examples:**
```bash
weft queue start titan
```

#### weft queue stop

Stop the queue runner after the current job completes.

```bash
weft queue stop [flags] <host>
```

**Examples:**
```bash
weft queue stop titan
```

#### weft queue list

Show jobs waiting in the queue and the currently running job.

```bash
weft queue list [flags] <host>
```

**Examples:**
```bash
weft queue list titan
```

#### weft queue status

Show the status of the queue runner.

```bash
weft queue status [flags] <host>
```

**Examples:**
```bash
weft queue status titan
```

#### weft queue upgrade

Redeploy and restart the queue runner if the remote script is out of date. The
CLI records a build number on the first line of the embedded script, compares
it with the version on the host, and restarts the runner only when needed.

```bash
weft queue upgrade titan
```

#### Queue Workflow Example

```bash
# Start the queue runner (does nothing if already running)
weft queue start titan

# Add jobs to the queue - laptop can disconnect after these commands
weft queue add titan "python train.py --epochs 100"
weft queue add titan "python train.py --epochs 200"
weft queue add titan "python evaluate.py"

# Check queue status (when back online)
weft queue status titan

# View what's in the queue
weft queue list titan

# Stop the queue after current job
weft queue stop titan
```

#### Job Dependencies

You can create job chains where one job runs after another completes:

```bash
# Start the queue runner
weft queue start titan

# Job 42: Training
weft queue add -d "Training" titan "python train.py"

# Job 43: Evaluate after training succeeds (waits for job 42)
weft queue add --after 42 -d "Evaluation" titan "python eval.py"

# Job 44: Generate report after evaluation (waits for job 43)
weft queue add --after 43 -d "Report" titan "python report.py"

# Job 45: Cleanup runs regardless of whether job 42 succeeded or failed
weft queue add --after-any 42 -d "Cleanup" titan "python cleanup.py"

# Disconnect laptop - jobs run in sequence on the remote host
```

**Dependency flags:**
- `--after ID`: Waits for the job to succeed (exit code 0). Skips if parent fails.
- `--after-any ID`: Waits for the job to complete (any exit code). Always runs.

Both flags work entirely on the remote host (no laptop connection needed) and can be used with both `queue add` and `run` commands.

> **Note:** Dependencies must stay on the same host. If you try to start a job on
> `titan` that waits on a job recorded on `studio`, the CLI errors immediately
> instead of queuing work that can never start.

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

Mouse support is off by default so you can select/copy text with your terminal. Pass `--mouse` (or set `enable_mouse: true` in `~/.config/weft/config.yaml`) if you prefer clickable rows instead.

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

### Pre-staging

Before dispatching a job, the coordinator pre-stages missing input data via
rsync. If a job needs `hf:meta-llama/Llama-3-8B` and it exists on titan but
not atlas, the coordinator transfers it before dispatch. Pre-staging is
best-effort — dispatch proceeds even if transfers fail.

### Data Locality

The system tracks what data exists on which hosts:

- **HuggingFace models/datasets**: Scanned from `~/.cache/huggingface` during
  host sync (`weft sync`)
- **Job outputs**: Declared via `--output` flags, recorded automatically when
  jobs complete successfully
- **Manual assets**: Registered via `weft host data`

```bash
# Scan a host's HF cache
weft host data --scan titan

# List data assets on a host
weft host data titan
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

To enable the coding agent for code fixes, add to `~/.config/weft/config.yaml`:

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
in `~/.config/weft/config.yaml`.

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

Configuration is stored in `~/.config/weft/config.yaml`.

### Default Command

By default, running `weft` with no arguments shows the help message. You can change this to run a different command:

```yaml
# ~/.config/weft/config.yaml
default_command: tui
```

Valid values for `default_command`:
- `help` (default): Show help message
- `tui`: Launch interactive terminal UI
- `list`: Show job list
- `web`: Launch the read-only web UI

### TUI Polling Intervals

Customize how often the TUI refreshes data:

```yaml
# ~/.config/weft/config.yaml
sync_interval: 15          # Seconds between job status syncs (default: 15)
log_refresh_interval: 3    # Seconds between log refreshes for running jobs (default: 3)
host_refresh_interval: 30  # Seconds between host info refreshes in hosts view (default: 30)
```

### Web UI

The TUI automatically starts a local web UI (localhost only) unless disabled:

```yaml
# ~/.config/weft/config.yaml
web_enabled: true
web_port: 8127
```

You can also run it directly:

```bash
weft web --open
```

### Log Caching

Completed job logs under 50KB are cached locally for faster access without SSH:

```yaml
# ~/.config/weft/config.yaml
log_cache_max_size: 51200  # Maximum log size to cache in bytes (default: 50KB)
log_cache_max_age: 7       # Days to keep cached logs (default: 7, 0 to disable)
```

Cached logs are stored in `~/.cache/weft/logs/` and automatically pruned during sync.

### SSH Connection

Tune SSH connection pool behavior for slow or unreliable networks:

```yaml
# ~/.config/weft/config.yaml
ssh:
  pool_size: 4        # Persistent sessions per host (default: 4)
  max_parallel: 8     # Max concurrent SSH operations across all hosts (default: 8)
  connect_timeout: 30 # SSH connect timeout in seconds (default: 10)
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

```yaml
# ~/.config/weft/config.yaml
vastai:
  default_image: "pytorch/pytorch:2.1.0-cuda12.1-cudnn8-runtime"
  sync_timeout: 5  # Timeout in seconds for R2 result checks (default: 5)
  r2:
    bucket: "my-results-bucket"
    account_id: "..."
    access_key_id: "..."
    secret_access_key: "..."
```

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
- `needs_rental`: No local host matches constraints; awaiting cloud GPU launch via TUI
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

- [Workflow Guide](docs/workflow-guide.md) - Common workflows with examples (pipelines, sweeps, cloud GPU, data locality)
- [Campaigns](docs/campaigns.md) - Cloud GPU campaign system: launching, monitoring, and managing batch cloud deployments
- [Campaign Roadmap](docs/ROADMAP.md) - Remaining gaps for llm-performance-models integration
- [Architecture](docs/architecture.md) - Detailed technical architecture and design
- [Coordinator Architecture](docs/coordinator-architecture.md) - Coordinator daemon design, placement scoring, and migration phases
- [Comparison to SLURM](docs/comparison-to-slurm.md) - How weft compares to HPC workload managers
- [Debugging](docs/debugging.md) - Remote log locations, queue state files, and process inspection
- [Agent Deployment](docs/agent-deployment.md) - Cross-compiling and deploying the Go agent to remote hosts
- [Ideas](docs/IDEAS.md) - Future feature ideas and enhancements
