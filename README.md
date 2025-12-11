# Remote Job Utilities

Utilities for running persistent tmux sessions on remote hosts that survive SSH disconnections.

## Problem Solved

When running long-running training jobs or analysis scripts on remote machines via SSH, the job terminates if:
- You close your laptop
- Your network disconnects
- SSH times out

These utilities use tmux to create persistent sessions that continue running even when you disconnect.

## Scripts

### run-remote.sh

Start a persistent tmux session on a remote host.

```bash
run-remote.sh [options] <host> <command...>
```

**Options:**
- `-n, --name NAME`: Session name (default: auto-generated from command)
- `-C, --directory DIR`: Working directory (default: same path as local cwd)
- `-d, --description TEXT`: Description of the job (for logging and queries)
- `--queue`: Queue job for later instead of running now
- `--queue-on-fail`: Queue job if connection fails

**Examples:**
```bash
# Basic usage (auto-generates session name, uses current directory path)
~/code/utils/remote-jobs/run-remote.sh cool30 'python train.py'

# With description (recommended)
~/code/utils/remote-jobs/run-remote.sh -d "Training GPT-2 with lr=0.001" cool30 'with-gpu python train.py --lr 0.001'

# Explicit session name and working directory
~/code/utils/remote-jobs/run-remote.sh -n train-gpt2 -C /mnt/code/LM2 cool30 'with-gpu python train.py'

# Queue for later (doesn't start immediately)
~/code/utils/remote-jobs/run-remote.sh --queue -d "Training run" cool30 'python train.py'

# Auto-queue if connection fails
~/code/utils/remote-jobs/run-remote.sh --queue-on-fail -d "Training run" cool30 'python train.py'
```

The script:
- Archives any existing log file for this session name (renamed with timestamp)
- Saves job metadata (for restart capability)
- Creates a detached tmux session on the remote host
- **Records the job in a local SQLite database** (`~/.config/remote-jobs/jobs.db`)
- Captures exit code when job completes
- Sends Slack notification on completion (if configured)
- Returns immediately (non-blocking)
- Prints the job ID and instructions for monitoring

### check-jobs.sh

Check the status of all running tmux sessions on a remote host.

```bash
check-jobs.sh <host>
```

**Example:**
```bash
~/code/utilities/remote-jobs/check-jobs.sh cool30
```

Shows:
- List of all active tmux sessions
- Status of each job (RUNNING or FINISHED)
- Exit code for finished jobs (success ✓ or failure ✗)
- Last 10 lines of output from each session
- **Updates the local database** when status changes are detected
- **Detects dead jobs** that terminated without completing normally

### check-status.sh

Check the status of a specific job (with database quick-path).

```bash
check-status.sh <host> <session-name>
```

**Example:**
```bash
~/code/utilities/remote-jobs/check-status.sh cool30 train-gpt2
```

This is faster than `check-jobs.sh` for checking a single job because it:
- First checks the local database for terminated jobs
- Only queries the remote host if the job is still running
- Updates the database if status has changed

**Exit codes:**
- `0`: Job completed successfully
- `1`: Job failed or error
- `2`: Job is still running
- `3`: Job not found

### list-jobs.sh

Query and search job history from the local database.

```bash
list-jobs.sh [options]
```

**Options:**
- `--running`: Show only running jobs
- `--completed`: Show only completed jobs
- `--dead`: Show only dead jobs
- `--pending`: Show only pending jobs (not yet started)
- `--host HOST`: Filter by host
- `--search QUERY`: Search by description or command
- `--limit N`: Limit results (default: 50)
- `--show ID`: Show detailed info for a specific job
- `--cleanup DAYS`: Delete jobs older than N days

**Examples:**
```bash
~/code/utilities/remote-jobs/list-jobs.sh                       # Recent jobs
~/code/utilities/remote-jobs/list-jobs.sh --running             # Running jobs
~/code/utilities/remote-jobs/list-jobs.sh --pending             # Pending jobs
~/code/utilities/remote-jobs/list-jobs.sh --host cool30         # Jobs on cool30
~/code/utilities/remote-jobs/list-jobs.sh --search training     # Search jobs
~/code/utilities/remote-jobs/list-jobs.sh --show 42             # Job details
~/code/utilities/remote-jobs/list-jobs.sh --cleanup 30          # Remove old jobs
```

### view-log.sh

View the full log file for a job.

```bash
view-log.sh <host> <session-name> [--follow|-f] [--lines|-n <N>]
```

**Examples:**
```bash
~/code/utilities/remote-jobs/view-log.sh cool30 train-gpt2           # Last 50 lines
~/code/utilities/remote-jobs/view-log.sh cool30 train-gpt2 -f        # Follow (like tail -f)
~/code/utilities/remote-jobs/view-log.sh cool30 train-gpt2 -n 100    # Last 100 lines
```

Also **updates the local database** if the job status has changed.

### restart-job.sh

Restart a job using its saved metadata.

```bash
restart-job.sh <host> <session-name>
```

**Example:**
```bash
~/code/utilities/remote-jobs/restart-job.sh cool30 train-gpt2
```

This kills the existing session (if any) and starts a new one with the same command and working directory.

### retry-job.sh

Retry pending jobs that couldn't start (e.g., due to connection failures).

```bash
retry-job.sh <job-id> [--host <new-host>]
retry-job.sh --list [--host <host>]
retry-job.sh --all [--host <host>]
retry-job.sh --delete <job-id>
```

**Examples:**
```bash
~/code/utilities/remote-jobs/retry-job.sh --list               # List pending jobs
~/code/utilities/remote-jobs/retry-job.sh 42                   # Retry job #42
~/code/utilities/remote-jobs/retry-job.sh 42 --host studio     # Retry on different host
~/code/utilities/remote-jobs/retry-job.sh --all                # Retry all pending jobs
~/code/utilities/remote-jobs/retry-job.sh --all --host cool30  # Retry pending jobs for cool30
~/code/utilities/remote-jobs/retry-job.sh --delete 42          # Remove pending job
```

### cleanup-jobs.sh

Clean up finished sessions and old log files.

```bash
cleanup-jobs.sh <host> [--sessions] [--logs] [--older-than <days>] [--dry-run]
```

**Examples:**
```bash
~/code/utilities/remote-jobs/cleanup-jobs.sh cool30                    # Clean both
~/code/utilities/remote-jobs/cleanup-jobs.sh cool30 --sessions         # Only finished sessions
~/code/utilities/remote-jobs/cleanup-jobs.sh cool30 --logs --older-than 3  # Logs > 3 days old
~/code/utilities/remote-jobs/cleanup-jobs.sh cool30 --dry-run          # Preview only
```

### kill-job.sh

Kill a specific tmux session on a remote host.

```bash
kill-job.sh <host> <session-name>
```

**Example:**
```bash
~/code/utilities/remote-jobs/kill-job.sh cool30 train-gpt2
```

## Claude Skills

Claude skills are available in `~/.claude/skills/remote-jobs/` for natural language interaction:

- **start-job**: "Start a job on cool30" or "Run the training script on cool30"
- **check-jobs**: "Check my jobs on cool30" or "What's running on cool30?"
- **check-status**: "Is the train-gpt2 job done?" or "Check status of my training job"
- **list-jobs**: "Show my recent jobs" or "What jobs have I run on cool30?"
- **view-log**: "Show the log for train-gpt2" or "What's the output of my training job?"
- **pending-jobs**: "Show pending jobs" or "Queue job for later" or "Retry pending jobs"
- **kill-job**: "Kill the train-gpt2 job on cool30"

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
4. Configure the app:
   - **Description**: "Shell utilities for running detached long-running jobs on remote hosts via SSH + tmux"
   - **Background color**: `#2E7D32`
5. In the sidebar, click "Incoming Webhooks"
6. Toggle "Activate Incoming Webhooks" to On
7. Click "Add New Webhook to Workspace" at the bottom
8. Select the channel where you want notifications (e.g., #jobs or a DM to yourself)
9. Click "Allow"
10. Copy the Webhook URL (starts with `https://hooks.slack.com/services/`)

### 2. Configure the Webhook

Use either method:

**Environment variable** (add to your shell profile):
```bash
export REMOTE_JOBS_SLACK_WEBHOOK="https://hooks.slack.com/services/T.../B.../..."
```

**Config file:**
```bash
mkdir -p ~/.config/remote-jobs
cp config.example ~/.config/remote-jobs/config
# Edit the file with your webhook URL
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

1. **run-remote.sh** creates a detached tmux session via SSH
2. The SSH command returns immediately (non-blocking)
3. The tmux session continues running on the remote host
4. You can close your laptop, disconnect, etc.
5. **check-jobs.sh** or **ssh + tmux attach** lets you check on the job later
