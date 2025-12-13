# Remote Jobs

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
- `--queue`: Queue job for later instead of running now
- `--queue-on-fail`: Queue job if connection fails

**Examples:**
```bash
# Basic usage (uses current directory path)
remote-jobs run deepthought 'python train.py'

# With description (recommended)
remote-jobs run -d "Training GPT-2 with lr=0.001" deepthought 'with-gpu python train.py --lr 0.001'

# Explicit working directory
remote-jobs run -C /mnt/code/LM2 deepthought 'with-gpu python train.py'

# Queue for later (doesn't start immediately)
remote-jobs run --queue -d "Training run" deepthought 'python train.py'

# Auto-queue if connection fails
remote-jobs run --queue-on-fail -d "Training run" deepthought 'python train.py'
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

### remote-jobs check

Check the status of all running tmux sessions on a remote host.

```bash
remote-jobs check <host>
```

Shows:
- List of all active tmux sessions
- Status of each job (RUNNING or FINISHED)
- Exit code for finished jobs (success ✓ or failure ✗)
- Last 10 lines of output from each session
- Updates the local database when status changes are detected
- Detects dead jobs that terminated without completing normally

### remote-jobs status

Check the status of a specific job by ID.

```bash
remote-jobs status <job-id>
```

**Exit codes:**
- `0`: Job completed successfully
- `1`: Job failed or error
- `2`: Job is still running
- `3`: Job not found

**Example:**
```bash
remote-jobs status 42    # Check status of job #42
```

This is faster than `check` for checking a single job because it:
- First checks the local database for terminated jobs
- Only queries the remote host if the job is still running
- Updates the database if status has changed

### remote-jobs tui

Launch an interactive terminal UI for viewing and managing jobs.

```bash
remote-jobs tui
```

The TUI has two views: **Jobs** and **Hosts**.

#### Jobs View (default)

Split-screen with:
- **Top panel**: Job list with status indicators (colored by status)
- **Bottom panel**: Job details or logs

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
- `k`: Kill highlighted job
- `p`: Prune completed/dead jobs from database
- `h` or `Tab`: Switch to hosts view
- `Esc`: Clear selection / exit logs view
- `q` or `Ctrl-C`: Quit
- `Ctrl-Z`: Suspend (return to shell, resume with `fg`)

#### Hosts View

Shows all hosts that have had jobs, with system info and GPU status.

- **Top panel**: Host list with status, architecture, load, and GPU summary
- **Bottom panel**: Detailed host info including per-GPU stats

```
╭──────────────────────────────────────────────────────────────────────────────╮
│ HOST         STATUS     ARCH             LOAD     GPU                        │
│ deepthought  ● online   Linux x86_64     2.31     6×GeForce 15%              │
│ skynet       ● online   Linux x86_64     0.42     2×A100 0%                  │
│ tardis       ○ offline  -                -        -                          │
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
│ Last checked: 5s ago                                                         │
╰──────────────────────────────────────────────────────────────────────────────╯
 ↑/↓:nav R:refresh j:jobs tab:switch q:quit
```

**Keyboard shortcuts:**
- `↑/↓`: Navigate host list
- `R`: Refresh selected host info
- `j` or `Tab`: Switch to jobs view
- `q`: Quit

**Host details include:**
- Architecture and OS version
- CPU count and memory usage
- Load average with CPU utilization percentage
- GPU table with temperature, utilization, and memory usage

The TUI automatically syncs job statuses every 15 seconds, refreshes logs for running jobs every 3 seconds, and refreshes host info every 30 seconds (configurable).

### remote-jobs list

Query and search job history from the local database.

```bash
remote-jobs list [flags]
```

**Flags:**
- `--running`: Show only running jobs
- `--completed`: Show only completed jobs
- `--dead`: Show only dead jobs
- `--pending`: Show only pending jobs (not yet started)
- `--host HOST`: Filter by host
- `--search QUERY`: Search by description or command
- `--limit N`: Limit results (default: 50)
- `--show ID`: Show detailed info for a specific job
- `--cleanup DAYS`: Delete jobs older than N days
- `--sync`: Sync job statuses from remote hosts before listing

**Examples:**
```bash
remote-jobs list                       # Recent jobs
remote-jobs list --running             # Running jobs
remote-jobs list --running --sync      # Running jobs (sync first)
remote-jobs list --pending             # Pending jobs
remote-jobs list --host deepthought         # Jobs on deepthought
remote-jobs list --search training     # Search jobs
remote-jobs list --show 42             # Job details
remote-jobs list --cleanup 30          # Remove old jobs
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

**Examples:**
```bash
remote-jobs log 42           # Last 50 lines
remote-jobs log 42 -f        # Follow (like tail -f)
remote-jobs log 42 -n 100    # Last 100 lines
```

### remote-jobs restart

Restart a job using its saved metadata.

```bash
remote-jobs restart <job-id>
```

This kills the existing session (if any) and starts a new one with the same command and working directory, creating a new job ID.

### remote-jobs retry

Retry pending jobs that couldn't start (e.g., due to connection failures).

```bash
remote-jobs retry <job-id> [--host <new-host>]
remote-jobs retry --list [--host <host>]
remote-jobs retry --all [--host <host>]
remote-jobs retry --delete <job-id>
```

**Examples:**
```bash
remote-jobs retry --list               # List pending jobs
remote-jobs retry 42                   # Retry job #42
remote-jobs retry 42 --host skynet     # Retry on different host
remote-jobs retry --all                # Retry all pending jobs
remote-jobs retry --all --host deepthought  # Retry pending jobs for deepthought
remote-jobs retry --delete 42          # Remove pending job
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
- `pending`: Job queued but not yet started (for later execution)
- `failed`: Job failed to start (e.g., connection error)

The database is automatically created on first use and updated when checking job status.

## Manual Monitoring

View last 50 lines of a session (replace `42` with actual job ID):
```bash
ssh deepthought 'tmux capture-pane -t rj-42 -p | tail -50'
```

Attach to a session interactively:
```bash
ssh deepthought -t 'tmux attach -t rj-42'
```

Press `Ctrl+B D` to detach from the session while leaving it running.

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
5. `remote-jobs check` or `ssh + tmux attach` lets you check on the job later
