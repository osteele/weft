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

Start a persistent tmux session on a remote host.

```bash
remote-jobs run [flags] <host> <command...>
```

**Flags:**
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-d, --description TEXT`: Description of the job (for logging and queries)
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `-f, --follow`: Follow log output after starting (Ctrl+C to stop following; job continues)
- `--allow`: Stream the job log live and stay attached until interrupted
- `--queue`: Queue job for later instead of running now
- `--queue-on-fail`: Queue job if connection fails
- `--from ID`: Copy settings from existing job ID (allows overriding)
- `--timeout DURATION`: Kill job after duration (e.g., "2h", "30m", "1h30m")
- `--after ID`: Start job after another job succeeds (implies `--queue`)
- `--after-any ID`: Start job after another job completes, success or failure (implies `--queue`)
- `--kill ID`: Kill a job by ID (synonym for `remote-jobs kill`)

**Examples:**
```bash
# Basic usage (uses current directory path)
remote-jobs run deepthought 'python train.py'

# With description (recommended)
remote-jobs run -d "Training GPT-2 with lr=0.001" deepthought 'with-gpu python train.py --lr 0.001'

# Explicit working directory
remote-jobs run -C /mnt/code/LM2 deepthought 'with-gpu python train.py'

# Start and follow log output
remote-jobs run -f -d "Training run" deepthought 'python train.py'

# Stay attached to live output (Ctrl+C detaches, job keeps running)
remote-jobs run --allow -d "Training run" deepthought 'python train.py'

# Set environment variables
remote-jobs run -e CUDA_VISIBLE_DEVICES=0 -e BATCH_SIZE=32 deepthought 'python train.py'

# Queue for later (doesn't start immediately)
remote-jobs run --queue -d "Training run" deepthought 'python train.py'

# Auto-queue if connection fails
remote-jobs run --queue-on-fail -d "Training run" deepthought 'python train.py'

# Run job after another succeeds (auto-queues)
remote-jobs run --after 42 deepthought 'python eval.py'

# Run cleanup job after another completes (success or failure)
remote-jobs run --after-any 42 deepthought 'python cleanup.py'

# Kill a job
remote-jobs run deepthought --kill 42
```

The command:
- Creates a job ID first, then starts the tmux session as `rj-{id}`
- Saves job metadata and logs to `~/.cache/remote-jobs/logs/` on the remote host
- Creates a detached tmux session on the remote host
- Records the job in a local SQLite database (`~/.config/remote-jobs/jobs.db`)
- Captures exit code when job completes
- Sends Slack notification on completion (if configured)
- Returns immediately (non-blocking)
- Prints the job ID and instructions for monitoring

### remote-jobs plan submit

Submit a YAML job execution plan that can mix one-off jobs, parallel groups,
and queue-backed series.

```bash
remote-jobs plan submit plan.yaml
remote-jobs plan submit --host studio plan.yaml   # provide default host via CLI
remote-jobs plan submit - < generated-plan.yaml   # read from stdin / heredoc
```

Plan files support an optional `kill` list, single `job` entries, `parallel`
groups (which simply run without dependencies), and `series` groups (which
queue jobs so each starts only after the prior job completes successfully or
after it finishes in any state). Provide `--host <name>` to supply a default
host for jobs that omit it, and add `--watch <duration>` to keep the CLI
around and report which jobs finished. See `docs/job-plans.md` for the full
schema, examples, and the reserved syntax for future resource-aware triggers.

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

**Exit codes (single job only):**
- `0`: Job completed successfully
- `1`: Job failed or error
- `2`: Job is still running
- `3`: Job not found

**Examples:**
```bash
remote-jobs job status 42           # Check status of job #42
remote-jobs job status 42 43 44     # Check multiple jobs
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
 ↑/↓:nav l:logs s:sync n:new r:restart k:kill p:prune h:hosts q:quit
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
- `--dead`: Show only dead jobs
- `--pending`: Show only pending jobs (not yet started)
- `--host HOST`: Filter by host (replaces old `check <host>` command)
- `--search QUERY`: Search by description or command
- `--limit N`: Limit results (default: 50)
- `--show ID`: Show detailed info for a specific job
- `--cleanup DAYS`: Delete jobs older than N days
- `--sync`: Sync job statuses from remote hosts before listing

**Examples:**
```bash
remote-jobs job list                          # Recent jobs
remote-jobs job list --running                # Running jobs
remote-jobs job list --running --sync         # Running jobs (sync first)
remote-jobs job list --pending                # Pending jobs
remote-jobs job list --host deepthought       # Jobs on deepthought
remote-jobs job list --search training        # Search jobs
remote-jobs job list --show 42                # Job details
remote-jobs job list --cleanup 30             # Remove old jobs
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

Remove completed and dead jobs from the local database and their log files from remote hosts.

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
remote-jobs prune                    # Remove all completed/dead jobs
remote-jobs prune --older-than 7d    # Only jobs older than 7 days
remote-jobs prune --older-than 24h   # Only jobs older than 24 hours
remote-jobs prune --dry-run          # Preview deletions
remote-jobs prune --dead-only        # Only remove dead jobs
remote-jobs prune --keep-files       # Don't delete remote files
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

**Note:** This only works for queued jobs. For running or completed jobs, use `run --from <id>` to create a new job on the desired host.

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

Set environment variables for the remote job. Can be repeated for multiple variables:

```bash
remote-jobs run -e CUDA_VISIBLE_DEVICES=0 cool30 "python train.py"
remote-jobs run -e BATCH_SIZE=32 -e LR=0.001 cool30 "python train.py"
remote-jobs queue add -e TMPDIR=/mnt/data/tmp cool30 "python train.py"
```

**Queue for later (`--queue`)**:
```bash
remote-jobs run --queue <host> <command>
```

Queues the job without running it immediately (same as `queue add`):

```bash
remote-jobs run --queue cool30 "python train.py"
remote-jobs run --queue --from 42            # Queue a copy of job 42
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

### remote-jobs queue

Manage job queues for sequential execution on remote hosts.

Jobs added to a queue run one after another without requiring the local machine to stay connected. The queue runner runs in a tmux session on the remote host and processes jobs in FIFO order.

#### remote-jobs queue add

Add a job to a remote queue.

```bash
remote-jobs queue add [flags] <host> <command...>
```

**Flags:**
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-d, --description TEXT`: Description of the job
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--after ID`: Start job after another job succeeds
- `--after-any ID`: Start job after another job completes (success or failure)
- `--queue NAME`: Queue name (default: "default")

**Examples:**
```bash
remote-jobs queue add cool30 'python train.py --epochs 100'
remote-jobs queue add -d "Training run 1" cool30 'python train.py'
remote-jobs queue add -e CUDA_VISIBLE_DEVICES=0 cool30 'python train.py'
remote-jobs queue add --after 42 cool30 'python eval.py'       # Run after job 42 succeeds
remote-jobs queue add --after-any 42 cool30 'python cleanup.py' # Run after job 42 completes (success or failure)
remote-jobs queue add --queue gpu cool30 'python train.py'
```

#### remote-jobs queue start

Start the queue runner on a remote host.

```bash
remote-jobs queue start [flags] <host>
```

**Flags:**
- `--queue NAME`: Queue name (default: "default")

The queue runner:
- Runs in a tmux session (`rj-queue-{name}`)
- Processes jobs sequentially from the queue file
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

### TUI Polling Intervals

Customize how often the TUI refreshes data:

```yaml
# ~/.config/remote-jobs/config.yaml
sync_interval: 15          # Seconds between job status syncs (default: 15)
log_refresh_interval: 3    # Seconds between log refreshes for running jobs (default: 3)
host_refresh_interval: 30  # Seconds between host info refreshes in hosts view (default: 30)
```

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
- `pending`: Job queued but not yet started (for later manual execution)
- `queued`: Job waiting in a remote queue for sequential execution
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
