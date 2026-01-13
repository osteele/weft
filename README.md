# Remote Jobs

[![Go Reference](https://pkg.go.dev/badge/github.com/osteele/remote-jobs.svg)](https://pkg.go.dev/github.com/osteele/remote-jobs)
[![Go Report Card](https://goreportcard.com/badge/github.com/osteele/remote-jobs)](https://goreportcard.com/report/github.com/osteele/remote-jobs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A CLI tool for running persistent tmux sessions on remote hosts that survive SSH disconnections.

## Problem Solved

When running long-running training jobs or analysis scripts on remote machines via SSH, the job terminates if:
- You close your laptop
- Your network disconnects
- SSH times out

Remote Jobs uses tmux to create persistent sessions that continue running even when you disconnect.

### Designed for unreliable networks

Laptops move between Wi-Fi networks, VPNs flap, and SSH servers occasionally
drop. Remote Jobs treats those scenarios as normal operations:

- Jobs always start locally first, so connection failures never lose metadata.
- Failed SSH attempts automatically defer work to the host queue (or the local
  pending list) and are replayed by `remote-jobs sync` when the host returns.
- Blocking commands such as `status --wait` and `plan submit --watch` keep
  polling while the host is down and announce when the connection comes back.

See [docs/network-resilience.md](docs/network-resilience.md) for the full story
on how the CLI keeps itself useful while you roam across networks.

### Agents-first ergonomics

Remote Jobs was designed for workflows where an automated agent drives the CLI
while a human keeps an eye on the TUI. Most commands print suggested follow-up
commands (“next steps”) directly in their output so agents can keep the relevant
context in their prompt without hunting through reference docs or skills files.
For example, `remote-jobs run` prints the `status`, `log`, and `start` commands
that make sense for the job it just created, which agents can copy verbatim. The
TUI then becomes the dashboard where humans monitor progress, adjust queues, or
apply manual fixes when needed.

### Occasionally connected workflow

The queue runner lives on each remote host and operates autonomously. Once you
queue a job, the remote host handles execution, completion, logging, and
starting the next queued job—all without any connection to your laptop.

```
Laptop (may sleep, travel, disconnect)
   │
   └──SSH──> Remote Host
              └── queue-runner (autonomous)
                   ├── Reads from queue file
                   ├── Starts jobs in tmux sessions
                   ├── Logs output, captures exit codes
                   └── Processes next job when current completes
```

Every action is recorded locally first—jobs, queue operations, even "start this
queued job now"—and synchronized with the remote host whenever it's reachable.
If a host is offline the CLI keeps showing the most recent known state, queues
the requested mutations, and replays them on the next connection. This approach
lets agents submit work in bulk without waiting for SSH, while humans can rely
on the TUI to show what will happen once hosts come back.

This architecture is fundamentally different from centralized job managers like
SLURM, where the controller must be reachable to submit or monitor jobs. See
[Comparison to SLURM](docs/comparison-to-slurm.md) for a detailed analysis.

## Installation

```bash
go install github.com/osteele/remote-jobs@latest
```

Or build from source:
```bash
git clone https://github.com/osteele/remote-jobs
cd remote-jobs
go install .
```

## Commands

### remote-jobs run

Queue a job on a remote host for managed execution.

```bash
remote-jobs run [flags] <host> <command...>
```

By default, jobs are added to a queue and scheduled by the queue runner. It can run multiple jobs on a host while keeping total CPU usage under a target cap. Use `--immediate` (`-i`) to start a job immediately.

Use `start <job-id>` to start a queued job immediately.

**Flags:**
- `-i, --immediate`: Start job immediately instead of queuing
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-m, --message TEXT`: Description of the job (for logging and queries)
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--tag TAG`: Tag to attach to the job (can be repeated)
- `--draft`: Record the job locally in draft status (never contacts the host until you later promote it)
- `-f, --follow`: Follow log output after starting (requires `--immediate`)
- `--allow`: Stream the job log live and stay attached (requires `--immediate`)
- `--from ID`: Copy settings from existing job ID (allows overriding)
- `--timeout DURATION`: Kill job after duration (e.g., "2h", "30m", "1h30m")
- `--after, --depends-on ID`: Start job after another job succeeds
- `--after-any ID`: Start job after another job completes, success or failure
- `--kill ID`: Kill a job by ID (synonym for `remote-jobs kill`)

If an immediate run can't reach the host, the CLI automatically records the job
locally and defers it to the remote queue. The next sync (or any command that
touches that host) will append the saved entry so it runs as soon as the host is
reachable again.

Draft jobs (`--draft`) stay entirely local. They’re useful for capturing a job
definition you want to tweak later or to keep certain jobs from ever syncing to
the host. When you’re ready to discard the draft, run `remote-jobs job draft <id>`
to toggle the status (or do it from the TUI, described below).

**Examples:**
```bash
# Queue a job (default behavior)
remote-jobs run deepthought 'python train.py'

# Start a queued job immediately
remote-jobs start 123

# Start immediately instead of queuing
remote-jobs run -i deepthought 'python train.py'

# With description (recommended)
remote-jobs run -m "Training GPT-2 with lr=0.001" deepthought 'with-gpu python train.py --lr 0.001'

# Explicit working directory
remote-jobs run -C /mnt/code/LM2 deepthought 'with-gpu python train.py'

# Start immediately and follow log output
remote-jobs run -i -f -m "Training run" deepthought 'python train.py'

# Stay attached to live output (Ctrl+C detaches, job keeps running)
remote-jobs run -i --allow -m "Training run" deepthought 'python train.py'

# Set environment variables
remote-jobs run -e CUDA_VISIBLE_DEVICES=0 -e BATCH_SIZE=32 deepthought 'python train.py'

# Tag jobs for later filtering
remote-jobs run --tag exp-012 --tag notebook-sync deepthought 'python train.py'

# Run job after another succeeds
remote-jobs run --after 42 deepthought 'python eval.py'

# Run cleanup job after another completes (success or failure)
remote-jobs run --after-any 42 deepthought 'python cleanup.py'

# Kill a job
remote-jobs run deepthought --kill 42
```

### remote-jobs artifact

Track and retrieve job outputs through a durable local artifact store.

Artifacts are declared by writing a manifest on the remote host. The CLI
syncs those files into `~/.config/remote-jobs/artifacts/` so they survive
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
- `RJ_JOB_ID`
- `RJ_ARTIFACT_MANIFEST` (default: `~/.cache/remote-jobs/artifacts/<job-id>.json`)
- `RJ_ARTIFACT_ROOT` (default: `.`)

**Examples:**
```bash
# Sync artifacts for job 2073 into the local store
remote-jobs artifact sync 2073

# List cached artifacts
remote-jobs artifact list 2073

# Retrieve by name or path
remote-jobs artifact get 2073 selectivity_results -o ./results.json
remote-jobs artifact get 2073 output/selectivity_results.json -o ./results.json

# Resolve latest job by tag
remote-jobs artifact get --tag exp-012 --latest selectivity_results -o ./results.json
```

The command:
- Creates a job ID and adds it to the remote queue (or starts immediately with `-i`)
- Queue runner schedules queued jobs in FIFO order (subject to CPU allotments)
- Saves job metadata and logs to `~/.cache/remote-jobs/logs/` on the remote host
- Records the job in a local SQLite database (`~/.config/remote-jobs/jobs.db`)
- Captures exit code when job completes
- Sends Slack notification on completion (if configured)
- Returns immediately (non-blocking)
- Prints the job ID and instructions for starting immediately or monitoring

### remote-jobs plan submit

Submit a YAML job execution plan that can mix one-off jobs, parallel groups,
and queue-backed series.

```bash
remote-jobs plan submit plan.yaml
remote-jobs plan submit --host studio plan.yaml   # provide default host via CLI
remote-jobs plan submit - < generated-plan.yaml   # read from stdin / heredoc
```

Plan files must start with `version: 1` to opt into the current schema and
remain compatible with future releases.

Plan files support an optional `kill` list, single `job` entries, `parallel`
groups, and `series` groups. Every block and job may declare `id`, `alias`,
`depends_on`, and `continue_on_failure`. The CLI resolves these references into
a DAG, auto-generating IDs (`block0`, `block0.job0`, etc.) when missing, so you
can declare multi-phase pipelines in a single YAML file. Provide `--host <name>`
to supply a default host for jobs that omit it, and add `--watch <duration>` to
keep the CLI around and report which jobs finished. See `docs/job-plans.md` for
the full schema plus dependency examples.

Inspect or lint a plan without running it:

```bash
remote-jobs plan validate plan.yaml
remote-jobs plan show --ids plan.yaml   # show generated IDs, aliases, hosts, deps
```

> **Agents welcome:** Remote Jobs (and the plan syntax in particular) was
> designed for coding agents as well as humans. The YAML shape is easy for an
> agent to emit directly from a prompt, so consider giving your agent runtime a
> skill/instruction that invokes `remote-jobs plan submit` with generated plans.
> This lets automated assistants spin up, chain, and monitor jobs using the same
> dependency and queueing logic described below.

### remote-jobs job status

Check the status of one or more jobs by ID.

```bash
remote-jobs job status <job-id>...
remote-jobs job status --wait 42         # block until the job finishes
remote-jobs job status --wait --wait-timeout 30m 42
remote-jobs job status --wait 42 43 44   # wait for all (exits 0 only if all succeed)
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
remote-jobs job status 42           # Check status of job #42
remote-jobs job status 42 43 44     # Check multiple jobs
remote-jobs job status 100:105      # Check jobs 100 through 105
```

This command:
- First checks the local database for terminated jobs
- Only queries the remote host if the job is still running
- Updates the database if status has changed
- Use `--wait` (with optional `--wait-timeout`) to block until jobs finish.
  The command exits with `0` only if every waited-on job succeeds.

### remote-jobs tui

Launch an interactive terminal UI for viewing and managing jobs.

```bash
remote-jobs tui
remote-jobs tui --mouse   # enable mouse clicks (disables terminal selection)
```

The TUI has two views: **Jobs** and **Hosts**.
Press `f` at any time to cycle the Jobs view between showing all jobs, only queued/running jobs, completed successes, or completed failures.

#### Jobs View (default)

Split-screen with:
- **Top panel**: Job list with status indicators (colored by status)
- **Bottom panel**: Job details or logs

Jobs are sorted by the newest job IDs so your latest or actively queued entries stay near the top of the list.

Press `d` on a highlighted job to flip it into draft mode. Drafting a queued job
removes it from the remote queue (or defers the removal if the host is offline),
while drafting a running job kills it and marks the record so future syncs don’t
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

**Keyboard shortcuts:**
- `↑/↓`: Navigate job list
- `l`: Toggle logs view (shows full logs, navigate between jobs while viewing)
- `s`: Sync job statuses from remote hosts
- `n`: Create new job (opens input form)
- `r`: Restart highlighted job
- `R`: Edit & restart (opens new job form pre-filled with job's parameters)
- `k`: Kill highlighted job
- `d`: Mark highlighted job as draft (removes remote queue entries or kills running jobs)
- `P`: Prune completed/dead jobs from database
- `S`: Start queue runner (for queued jobs)
- `g`: Start queued job now (bypasses `--after` dependency)
- `x`: Remove job from list
- `h` or `Tab`: Switch to hosts view
- `f`: Cycle job filter (All → Queued/Running → Success → Failure)
- `Esc`: Clear selection / exit logs view

Mouse support is off by default so you can select/copy text with your terminal. Pass `--mouse` (or set `enable_mouse: true` in `~/.config/remote-jobs/config.yaml`) if you prefer clickable rows instead.
- `q` or `Ctrl-C`: Quit
- `Ctrl-Z`: Suspend (return to shell, resume with `fg`)

**Log caching:** When a host goes offline, the TUI shows the last successfully fetched log content with a "(cached - host offline)" indicator.

#### Hosts View

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

**Keyboard shortcuts:**
- `↑/↓`: Navigate host list
- `j` or `Tab`: Switch to jobs view
- `q`: Quit

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

### remote-jobs job list

Query and search job history from the local database.

```bash
remote-jobs job list [flags]
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
remote-jobs job list                          # Recent jobs
remote-jobs job list --running                # Running jobs
remote-jobs job list --running --sync         # Running jobs (sync first)
remote-jobs job list --host deepthought       # Jobs on deepthought
remote-jobs job list --tag exp-012            # Jobs with a tag
remote-jobs job list --status unprocessed     # Jobs missing the processed tag
remote-jobs job list --search training        # Search jobs
remote-jobs job list --show 42                # Job details
remote-jobs job list --cleanup 30             # Remove old jobs
```

### remote-jobs tag

Attach or remove tags on jobs stored in the local database.

```bash
remote-jobs tag add <job-id> <tag>
remote-jobs tag rm <job-id> <tag>
```

**Examples:**
```bash
remote-jobs tag add 42 exp-012
remote-jobs tag rm 42 exp-012
```

### remote-jobs mark-processed

Mark a job as processed by adding the reserved `processed` tag.

```bash
remote-jobs mark-processed <job-id>
```

**Examples:**
```bash
remote-jobs mark-processed 42
remote-jobs job list --status unprocessed
```

### remote-jobs sync

Sync job statuses from all remote hosts with running jobs.

```bash
remote-jobs sync [flags]
```

**Flags:**
- `-v, --verbose`: Show detailed progress

Automatically finds hosts with running jobs and updates their status in the local database. Connection failures are silently ignored (unreachable hosts are skipped).

**Examples:**
```bash
remote-jobs sync              # Sync all hosts
remote-jobs sync --verbose    # Show progress
```

### remote-jobs prune

Tombstone completed/dead jobs so they disappear from listings, and optionally delete their log files on remote hosts.

```bash
remote-jobs prune [flags]
```

**Flags:**
- `--older-than DURATION`: Only remove jobs older than this (e.g., `7d`, `24h`, `30m`)
- `--dead-only`: Only remove dead jobs (not completed)
- `--dry-run`: Preview what would be deleted without actually deleting
- `--keep-files`: Don't delete remote log files (only remove from database)

**Examples:**
```bash
remote-jobs prune                    # Tombstone all completed/dead jobs
remote-jobs prune --older-than 7d    # Only tombstone jobs older than 7 days
remote-jobs prune --older-than 24h   # Only tombstone jobs older than 24 hours
remote-jobs prune --dry-run          # Preview which jobs would be tombstoned
remote-jobs prune --dead-only        # Only tombstone dead jobs
remote-jobs prune --keep-files       # Tombstone locally but keep remote files

Tombstoned jobs remain in the database for auditing and can still be viewed with
`remote-jobs job status <id>` or `remote-jobs log <id>`, but they disappear from
`remote-jobs list` and the TUI.
```

### remote-jobs log

View the full log file for a job.

```bash
remote-jobs log <job-id> [flags]
```

**Flags:**
- `-f, --follow`: Follow log in real-time (like `tail -f`)
- `-n, --lines N`: Number of lines to show (default: 50)
- `--from N`: Show lines starting from line N
- `--to N`: Show lines up to line N
- `--grep PATTERN`: Filter lines matching pattern

**Examples:**
```bash
remote-jobs log 42           # Last 50 lines
remote-jobs log 42 -f        # Follow (like tail -f)
remote-jobs log 42 -n 100    # Last 100 lines
remote-jobs log 42 --from 100 --to 200  # Lines 100-200
remote-jobs log 42 --from 500           # From line 500 onwards
remote-jobs log 42 --to 100             # First 100 lines
remote-jobs log 42 --grep error         # Lines containing "error"
remote-jobs log 42 -f --grep epoch      # Follow, filter for "epoch"
```

**Notes:**
- `--from`/`--to` cannot be used with `-n`/`--lines`
- `--follow` cannot be used with `--to`
- `--grep` can be combined with any other option

### remote-jobs job restart

Restart a job using its saved metadata.

```bash
remote-jobs job restart <job-id>
```

This kills the existing session (if any) and starts a new one with the same command and working directory, creating a new job ID.

**Note:** For most use cases, `run --from <id>` is more flexible as it allows overriding settings.

### remote-jobs retry

Clone a previous job and queue it again with the same host, directory, command, description, and environment variables. Dependencies are not copied so the retried job starts as soon as it reaches the front of the queue.

```bash
remote-jobs retry <job-id>
remote-jobs job retry <job-id>   # Alias
```

The command prints the new job ID and whether it was queued immediately or deferred until the host is online.
Queued jobs that previously depended on the retried job automatically update their dependency to the new job ID.

### remote-jobs job move

Move a queued job to a different host.

```bash
remote-jobs job move <job-id> <new-host>
```

This command updates the host for a job that hasn't started yet (status=queued). Useful when you've queued work but want to run it on a different machine.

**Examples:**
```bash
remote-jobs job move 42 cool100   # Move job 42 to cool100
remote-jobs job move 43 studio    # Move job 43 to studio
```

### remote-jobs job start

Start a queued job immediately, bypassing its queue order. The job is removed
from the remote queue file, marked as running, and launched right away.

```bash
remote-jobs job start <job-id>
remote-jobs run <job-id>           # Shorthand (same effect)
```

Examples:
```bash
remote-jobs run 512                # Start queued job 512 immediately
remote-jobs job start 512          # Same as above
remote-jobs job start 9001         # Bypass queue order and run now
```

Only jobs with status `queued` can be started this way. The command preserves
the job's working directory, environment variables, and metadata.

**Note:** For running or completed jobs, use `run --from <id>` to create a new job on the desired host.

### Advanced run options

The `run` command supports several advanced options for more control:

**Copy settings from existing job (`--from`)**:
```bash
remote-jobs run --from <job-id> [<host>] [<command>]
```

Copies command, working directory, and description from an existing job. You can override any of these:

```bash
remote-jobs run --from 42                    # Rerun job 42 with same settings
remote-jobs run --from 42 cool100            # Rerun on different host
remote-jobs run --from 42 --timeout 4h       # Rerun with longer timeout
remote-jobs run --from 42 cool100 "python train.py --epochs 200"  # Override everything
```

**Timeout (`--timeout`)**:
```bash
remote-jobs run --timeout <duration> <host> <command>
```

Automatically kills the job after the specified duration (e.g., "2h", "30m", "1h30m"):

```bash
remote-jobs run --timeout 2h cool30 "python train.py"
remote-jobs run --timeout 30m --from 42      # Retry with timeout
```

**Environment variables (`-e, --env`)**:
```bash
remote-jobs run -e VAR=value <host> <command>
```

Set environment variables for the job. Can be repeated for multiple variables:

```bash
remote-jobs run -e CUDA_VISIBLE_DEVICES=0 cool30 "python train.py"
remote-jobs run -e BATCH_SIZE=32 -e LR=0.001 cool30 "python train.py"
remote-jobs queue add -e TMPDIR=/mnt/data/tmp cool30 "python train.py"
```

### remote-jobs cleanup

Clean up finished sessions and old log files.

```bash
remote-jobs cleanup <host> [flags]
```

**Flags:**
- `--sessions`: Kill finished sessions only
- `--logs`: Remove archived log files only
- `--older-than N`: Only clean items older than N days (default: 7)
- `--dry-run`: Preview without actually deleting

**Examples:**
```bash
remote-jobs cleanup deepthought                    # Clean both
remote-jobs cleanup deepthought --sessions         # Only finished sessions
remote-jobs cleanup deepthought --logs --older-than 3  # Logs > 3 days old
remote-jobs cleanup deepthought --dry-run          # Preview only
```

### remote-jobs kill

Kill a running job.

```bash
remote-jobs kill <job-id>
```

**Example:**
```bash
remote-jobs kill 42    # Kill job #42
```

### remote-jobs job draft

Move a job into draft status and make sure it never runs remotely (queued or otherwise).

```bash
remote-jobs job draft <job-id>
```

When you draft a job:
- Running jobs are killed and removed from the remote host queue/state
- Queued jobs are removed from queue files so they won’t start later
- Offline hosts keep the “draft pending” request until the next `remote-jobs sync`
- Already-completed jobs simply change status locally

Drafting is handy when you realize a queued job shouldn’t run anymore but you
want to keep its metadata/log references around for editing or cloning later.
You can also trigger the same action from the TUI by pressing `d`.

### remote-jobs queue

Manage job queues for CPU-capped execution on remote hosts.

Jobs added to a queue are scheduled in FIFO order, and the queue runner can run multiple jobs per host while keeping total CPU usage under a target cap. The queue runner runs in a tmux session on the remote host and keeps working when you disconnect. CPU allotments are set in the TUI (presets); the CLI has no setter.

#### remote-jobs queue add

Add a job to a remote queue.

```bash
remote-jobs queue add [flags] <host> <command...>
```

**Flags:**
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-d, --description TEXT`: Description of the job
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--draft`: Save a draft queue entry locally without touching the remote queue file
- `--after, --depends-on ID`: Start job after another job succeeds
- `--after-any ID`: Start job after another job completes (success or failure)
- `--queue NAME`: Queue name (default: "default")

Draft queue entries behave like sticky notes: they keep the command, env vars,
and metadata in your local database while guaranteeing they never reach the
remote queue runner. Convert them later with `remote-jobs job draft <id>` (or
the `d` key in the TUI) once you decide they should be eligible to run.

**Examples:**
```bash
remote-jobs queue add cool30 'python train.py --epochs 100'
remote-jobs queue add -d "Training run 1" cool30 'python train.py'
remote-jobs queue add -e CUDA_VISIBLE_DEVICES=0 cool30 'python train.py'
remote-jobs queue add --after 42 cool30 'python eval.py'       # Run after job 42 succeeds
remote-jobs queue add --after-any 42 cool30 'python cleanup.py' # Run after job 42 completes (success or failure)
remote-jobs queue add --queue gpu cool30 'python train.py'
```

#### remote-jobs edit

Edit a queued job’s metadata—description, working directory, command, environment variables, or dependencies.  
`remote-jobs queue edit` is an alias for this command and accepts the same flags (with an optional `--queue` override).

```bash
remote-jobs edit [flags] <job-id>
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
remote-jobs edit 1595 --depends-on 1599
remote-jobs edit 1600 --depends-on 1400 --depends-on-any 1401
remote-jobs edit 1700 --clear-depends
remote-jobs edit 1800 --command "python eval.py" -C ~/project -e FOO=bar
```

Changes are validated so you can only depend on jobs that run on the same host. If the host is offline, the update is deferred like other queue operations and reapplied once it reconnects.

#### remote-jobs queue start

Start the queue runner on a remote host.

```bash
remote-jobs queue start [flags] <host>
```

**Flags:**
- `--queue NAME`: Queue name (default: "default")

The queue runner:
- Runs in a tmux session (`rj-queue-{name}`)
- Processes queued jobs in FIFO order with a CPU cap
- Continues running even when you disconnect
- Sends Slack notifications (if configured)

**Examples:**
```bash
remote-jobs queue start cool30
remote-jobs queue start --queue gpu cool30
```

#### remote-jobs queue stop

Stop the queue runner after the current job completes.

```bash
remote-jobs queue stop [flags] <host>
```

**Flags:**
- `--queue NAME`: Queue name (default: "default")

**Examples:**
```bash
remote-jobs queue stop cool30
remote-jobs queue stop --queue gpu cool30
```

#### remote-jobs queue list

Show jobs waiting in the queue and the currently running job.

```bash
remote-jobs queue list [flags] <host>
```

**Flags:**
- `--queue NAME`: Queue name (default: "default")

**Examples:**
```bash
remote-jobs queue list cool30
remote-jobs queue list --queue gpu cool30
```

#### remote-jobs queue status

Show the status of the queue runner.

```bash
remote-jobs queue status [flags] <host>
```

**Flags:**
- `--queue NAME`: Queue name (default: "default")

**Examples:**
```bash
remote-jobs queue status cool30
remote-jobs queue status --queue gpu cool30
```

#### remote-jobs queue upgrade

Redeploy and restart the queue runner if the remote script is out of date. The
CLI records a build number on the first line of the embedded script, compares
it with the version on the host, and restarts the runner only when needed.

```bash
remote-jobs queue upgrade cool30
remote-jobs queue upgrade --queue gpu cool30
```

#### Queue Workflow Example

```bash
# Start the queue runner (does nothing if already running)
remote-jobs queue start cool30

# Add jobs to the queue - laptop can disconnect after these commands
remote-jobs queue add cool30 "python train.py --epochs 100"
remote-jobs queue add cool30 "python train.py --epochs 200"
remote-jobs queue add cool30 "python evaluate.py"

# Check queue status (when back online)
remote-jobs queue status cool30

# View what's in the queue
remote-jobs queue list cool30

# Stop the queue after current job
remote-jobs queue stop cool30
```

#### Job Dependencies

You can create job chains where one job runs after another completes:

```bash
# Start the queue runner
remote-jobs queue start cool30

# Job 42: Training
remote-jobs queue add -d "Training" cool30 "python train.py"

# Job 43: Evaluate after training succeeds (waits for job 42)
remote-jobs queue add --after 42 -d "Evaluation" cool30 "python eval.py"

# Job 44: Generate report after evaluation (waits for job 43)
remote-jobs queue add --after 43 -d "Report" cool30 "python report.py"

# Job 45: Cleanup runs regardless of whether job 42 succeeded or failed
remote-jobs queue add --after-any 42 -d "Cleanup" cool30 "python cleanup.py"

# Disconnect laptop - jobs run in sequence on the remote host
```

**Dependency flags:**
- `--after ID`: Waits for the job to succeed (exit code 0). Skips if parent fails.
- `--after-any ID`: Waits for the job to complete (any exit code). Always runs.

Both flags work entirely on the remote host (no laptop connection needed) and can be used with both `queue add` and `run` commands.

> **Note:** Dependencies must stay on the same host. If you try to start a job on
> `cool30` that waits on a job recorded on `studio`, the CLI errors immediately
> instead of queuing work that can never start.

## Configuration

Configuration is stored in `~/.config/remote-jobs/config.yaml`.

### Default Command

By default, running `remote-jobs` with no arguments shows the help message. You can change this to run a different command:

```yaml
# ~/.config/remote-jobs/config.yaml
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
# ~/.config/remote-jobs/config.yaml
sync_interval: 15          # Seconds between job status syncs (default: 15)
log_refresh_interval: 3    # Seconds between log refreshes for running jobs (default: 3)
host_refresh_interval: 30  # Seconds between host info refreshes in hosts view (default: 30)
```

### Web UI

The TUI automatically starts a local web UI (localhost only) unless disabled:

```yaml
# ~/.config/remote-jobs/config.yaml
web_enabled: true
web_port: 8127
```

You can also run it directly:

```bash
remote-jobs web --open
```

### Log Caching

Completed job logs under 50KB are cached locally for faster access without SSH:

```yaml
# ~/.config/remote-jobs/config.yaml
log_cache_max_size: 51200  # Maximum log size to cache in bytes (default: 50KB)
log_cache_max_age: 7       # Days to keep cached logs (default: 7, 0 to disable)
```

Cached logs are stored in `~/.cache/remote-jobs/logs/` and automatically pruned during sync.

## Job Database

Jobs are tracked in a local SQLite database at `~/.config/remote-jobs/jobs.db`. The database records:
- Unique job ID (used to identify tmux sessions as `rj-{id}`)
- Host
- Working directory and command
- Optional description
- Start time and end time
- Exit code and status

Log files are stored on remote hosts at `~/.cache/remote-jobs/logs/{id}-{timestamp}.log`.

**Job statuses:**
- `starting`: Job is being set up (transient state)
- `running`: Job is currently executing on the remote host
- `completed`: Job finished (check exit code for success/failure)
- `dead`: Job terminated unexpectedly without capturing exit code
- `queued`: Job waiting in a remote queue for scheduling
- `failed`: Job failed to start (e.g., connection error)

The database is automatically created on first use and updated when checking job status.

## Manual Monitoring

View last 50 lines of a job's output (replace `42` with actual job ID):
```bash
remote-jobs log 42
```

Follow log output in real-time:
```bash
remote-jobs log 42 -f
```

## Web UI (Read-only)

Start the read-only web monitor on localhost:

```bash
remote-jobs web
```

Open it in your browser:

```bash
remote-jobs web --open
```

Press `Ctrl+C` to stop following.

## Slack Notifications

To receive Slack notifications when jobs complete:

### 1. Create a Slack App with Incoming Webhook

1. Go to [https://api.slack.com/apps](https://api.slack.com/apps)
2. Click "Create New App" → "From scratch"
3. Name your app (e.g., "Remote Jobs") and select your workspace
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
export REMOTE_JOBS_SLACK_WEBHOOK="https://hooks.slack.com/services/T.../B.../..."
```

**Config file:**
```bash
mkdir -p ~/.config/remote-jobs
echo "SLACK_WEBHOOK=https://hooks.slack.com/services/..." > ~/.config/remote-jobs/config
```

### 3. Optional: Configure When to Notify

By default, you'll receive notifications for all jobs. You can customize this with environment variables:

**Notification Mode:**
```bash
# Notify for all jobs (default)
export REMOTE_JOBS_SLACK_NOTIFY="all"

# Notify only for failures
export REMOTE_JOBS_SLACK_NOTIFY="failures"

# Disable notifications
export REMOTE_JOBS_SLACK_NOTIFY="none"
```

**Minimum Duration Threshold:**
```bash
# Default: 15 seconds - jobs shorter than this won't trigger notifications
# (Failed jobs always notify regardless of duration)

# Notify for all jobs regardless of duration
export REMOTE_JOBS_SLACK_MIN_DURATION="0"

# Only notify for jobs longer than 1 minute
export REMOTE_JOBS_SLACK_MIN_DURATION="60"

# For longer jobs only (5 minutes)
export REMOTE_JOBS_SLACK_MIN_DURATION="300"
```

**Verbose Mode:**
```bash
# Include working directory and command in notification
export REMOTE_JOBS_SLACK_VERBOSE="1"
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

1. `remote-jobs run` creates a detached tmux session via SSH
2. The SSH command returns immediately (non-blocking)
3. The tmux session continues running on the remote host
4. You can close your laptop, disconnect, etc.
5. `remote-jobs log` or `remote-jobs job status` lets you check on the job later

## Documentation

- [Architecture](docs/architecture.md) - Detailed technical architecture and design
- [Comparison to SLURM](docs/comparison-to-slurm.md) - How remote-jobs compares to HPC workload managers
- [Ideas](docs/IDEAS.md) - Future feature ideas and enhancements
