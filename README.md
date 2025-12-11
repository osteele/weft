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
- `-n, --name NAME`: Session name (default: auto-generated from command)
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-d, --description TEXT`: Description of the job (for logging and queries)
- `--queue`: Queue job for later instead of running now
- `--queue-on-fail`: Queue job if connection fails

**Examples:**
```bash
# Basic usage (auto-generates session name, uses current directory path)
remote-jobs run cool30 'python train.py'

# With description (recommended)
remote-jobs run -d "Training GPT-2 with lr=0.001" cool30 'with-gpu python train.py --lr 0.001'

# Explicit session name and working directory
remote-jobs run -n train-gpt2 -C /mnt/code/LM2 cool30 'with-gpu python train.py'

# Queue for later (doesn't start immediately)
remote-jobs run --queue -d "Training run" cool30 'python train.py'

# Auto-queue if connection fails
remote-jobs run --queue-on-fail -d "Training run" cool30 'python train.py'
```

The command:
- Archives any existing log file for this session name (renamed with timestamp)
- Saves job metadata (for restart capability)
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

Check the status of a specific job (with database quick-path).

```bash
remote-jobs status <host> <session>
```

**Exit codes:**
- `0`: Job completed successfully
- `1`: Job failed or error
- `2`: Job is still running
- `3`: Job not found

This is faster than `check` for checking a single job because it:
- First checks the local database for terminated jobs
- Only queries the remote host if the job is still running
- Updates the database if status has changed

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
remote-jobs list --host cool30         # Jobs on cool30
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

Remove completed and dead jobs from the local database.

```bash
remote-jobs prune [flags]
```

**Flags:**
- `--older-than DURATION`: Only remove jobs older than this (e.g., `7d`, `24h`, `30m`)
- `--dead-only`: Only remove dead jobs (not completed)
- `--dry-run`: Preview what would be deleted without actually deleting

**Examples:**
```bash
remote-jobs prune                    # Remove all completed/dead jobs
remote-jobs prune --older-than 7d    # Only jobs older than 7 days
remote-jobs prune --older-than 24h   # Only jobs older than 24 hours
remote-jobs prune --dry-run          # Preview deletions
remote-jobs prune --dead-only        # Only remove dead jobs
```

### remote-jobs log

View the full log file for a job.

```bash
remote-jobs log <host> <session> [flags]
```

**Flags:**
- `-f, --follow`: Follow log in real-time (like `tail -f`)
- `-n, --lines N`: Number of lines to show (default: 50)

**Examples:**
```bash
remote-jobs log cool30 train-gpt2           # Last 50 lines
remote-jobs log cool30 train-gpt2 -f        # Follow (like tail -f)
remote-jobs log cool30 train-gpt2 -n 100    # Last 100 lines
```

### remote-jobs restart

Restart a job using its saved metadata.

```bash
remote-jobs restart <host> <session>
```

This kills the existing session (if any) and starts a new one with the same command and working directory.

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
remote-jobs retry 42 --host studio     # Retry on different host
remote-jobs retry --all                # Retry all pending jobs
remote-jobs retry --all --host cool30  # Retry pending jobs for cool30
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
remote-jobs cleanup cool30                    # Clean both
remote-jobs cleanup cool30 --sessions         # Only finished sessions
remote-jobs cleanup cool30 --logs --older-than 3  # Logs > 3 days old
remote-jobs cleanup cool30 --dry-run          # Preview only
```

### remote-jobs kill

Kill a specific tmux session on a remote host.

```bash
remote-jobs kill <host> <session>
```

## Job Database

Jobs are tracked in a local SQLite database at `~/.config/remote-jobs/jobs.db`. The database records:
- Host and session name
- Working directory and command
- Optional description
- Start time and end time
- Exit code and status

**Job statuses:**
- `running`: Job is currently executing on the remote host
- `completed`: Job finished (check exit code for success/failure)
- `dead`: Job terminated unexpectedly without capturing exit code
- `pending`: Job queued but not yet started (for later execution)

The database is automatically created on first use and updated when checking job status.

## Manual Monitoring

View last 50 lines of a session:
```bash
ssh cool30 'tmux capture-pane -t train-gpt2 -p | tail -50'
```

Attach to a session interactively:
```bash
ssh cool30 -t 'tmux attach -t train-gpt2'
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

### What You'll Get

Notifications include:
- Job name and host
- Success (✓) or failure (✗) status with exit code
- Duration

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
