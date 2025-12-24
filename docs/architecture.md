# Remote Jobs Architecture

This document describes the architecture and design of the Remote Jobs CLI tool.

## Overview

Remote Jobs is a CLI tool for managing persistent tmux sessions on remote hosts. It solves the problem of long-running jobs terminating when SSH connections drop due to network issues, laptop closure, or session timeouts.

```
┌─────────────────────────────────────────────────────────────────────┐
│                          Local Machine                               │
├─────────────────────────────────────────────────────────────────────┤
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │   CLI (cmd)  │───▶│   Database   │    │   Config (YAML)      │  │
│  │              │    │   (SQLite)   │    │                      │  │
│  └──────┬───────┘    └──────────────┘    └──────────────────────┘  │
│         │                                                           │
│         │ SSH                                                       │
│         ▼                                                           │
├─────────────────────────────────────────────────────────────────────┤
│                         Remote Host(s)                               │
├─────────────────────────────────────────────────────────────────────┤
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │    tmux      │───▶│   Job Logs   │    │   Slack Notify       │  │
│  │   Session    │    │   (.log)     │    │   (on completion)    │  │
│  └──────────────┘    └──────────────┘    └──────────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

## Directory Structure

```
remote-jobs/
├── main.go                 # Entry point
├── cmd/                    # CLI commands (Cobra)
│   ├── root.go            # Root command, default command handling
│   ├── job.go             # Job subcommand (groups job operations)
│   ├── run.go             # Start jobs (supports --from, --timeout, --queue)
│   ├── log.go             # View job logs
│   ├── kill.go            # Kill running jobs
│   ├── status.go          # Check job status (via job subcommand)
│   ├── list.go            # Query job history (via job subcommand)
│   ├── restart.go         # Restart jobs (via job subcommand)
│   ├── describe.go        # Set job description (via job subcommand)
│   ├── sync.go            # Sync job statuses
│   ├── cleanup.go         # Clean up finished sessions
│   ├── prune.go           # Remove old jobs
│   ├── queue.go           # Queue commands (add, start, stop, list)
│   ├── tui.go             # Launch interactive TUI
│   ├── embed.go           # Embedded files (notify script)
│   └── notify-slack.sh    # Slack notification script (embedded)
├── internal/
│   ├── config/            # Configuration management
│   │   └── config.go      # YAML config loading
│   ├── db/                # Database operations
│   │   ├── db.go          # Job CRUD, queries, migrations
│   │   └── db_test.go     # Database tests
│   ├── session/           # Session/file path management
│   │   ├── session.go     # Tmux naming, file paths, metadata
│   │   └── session_test.go
│   ├── ssh/               # SSH operations
│   │   ├── ssh.go         # SSH commands, retry logic, process stats
│   │   └── command_test.go
│   └── tui/               # Terminal UI
│       ├── model.go       # Bubble Tea model, update loop, views
│       ├── host.go        # Host info parsing
│       └── styles.go      # Lipgloss styling
└── docs/
    └── architecture.md    # This document
```

## Core Components

### 1. CLI Layer (`cmd/`)

Built with [Cobra](https://github.com/spf13/cobra), the CLI provides subcommands for all operations.

**Command Flow:**

```
remote-jobs run [--from ID] [--timeout DURATION] [--queue] <host> <command>
    │
    ├── 0. (If --from) Copy settings from existing job (can override)
    ├── 1. Create job record in SQLite (status: "starting" or "queued")
    ├── 2. Generate unique tmux session name (rj-{job_id})
    ├── 3. Create log directory on remote (~/.cache/remote-jobs/logs/)
    ├── 4. Save metadata file on remote
    ├── 5. Build wrapper command (cd, logging, exit code, timeout monitor)
    ├── 6. SSH: tmux new-session -d -s 'rj-N' bash -c '...'
    ├── 7. Update job status to "running"
    └── 8. Print monitoring instructions
```

**Key Flags:**
- `--from <id>`: Copy settings from existing job (command, directory, description)
- `--timeout <duration>`: Automatically kill job after duration (e.g., "2h", "30m")
- `--queue`: Add to job queue instead of running immediately

**Key Design Decisions:**
- Job ID is allocated BEFORE starting the tmux session, ensuring the database always knows about the job
- If SSH fails during setup, the job is marked as "failed" with the error message
- The `--queue-on-fail` flag allows jobs to be queued for later retry on connection errors
- `--from` flag replaces the old `retry` command with a more composable approach

### 2. Database Layer (`internal/db/`)

Uses [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (pure Go SQLite) for zero CGO dependencies.

**Schema:**

```sql
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    host TEXT NOT NULL,
    session_name TEXT,           -- Deprecated: legacy compatibility
    working_dir TEXT NOT NULL,
    command TEXT NOT NULL,
    description TEXT,
    error_message TEXT,          -- For failed jobs
    start_time INTEGER,          -- NULL for queued jobs that haven't started
    end_time INTEGER,
    exit_code INTEGER,
    status TEXT NOT NULL DEFAULT 'running',
    queue_name TEXT              -- Name of queue for queued jobs
);

-- Indexes for common queries
CREATE INDEX idx_jobs_host ON jobs(host);
CREATE INDEX idx_jobs_session ON jobs(session_name);
CREATE INDEX idx_jobs_status ON jobs(status);
CREATE INDEX idx_jobs_start ON jobs(start_time DESC);
```

**Job Status Lifecycle:**

```
┌──────────┐     ┌─────────┐     ┌───────────┐
│ starting │────▶│ running │────▶│ completed │
└──────────┘     └─────────┘     └───────────┘
     │                │
     │                │   (session dies unexpectedly)
     ▼                ▼
┌──────────┐     ┌─────────┐
│  failed  │     │  dead   │
└──────────┘     └─────────┘
     │
     │   (--queue or --queue-on-fail)
     ▼
┌──────────┐
│ queued   │ ────▶ (queue runner or run --from) ────▶ starting
└──────────┘
```

**Status Definitions:**
- `starting`: Job record created, tmux session being set up
- `running`: Tmux session successfully started
- `completed`: Job finished (exit code captured from status file)
- `dead`: Job terminated without writing exit code (crashed/killed)
- `queued`: Job added to remote queue for later execution
- `failed`: Job setup failed (e.g., SSH connection error)

### 3. SSH Layer (`internal/ssh/`)

Wraps SSH/SCP commands with error handling and retry logic.

**Key Functions:**

| Function | Purpose |
|----------|---------|
| `Run` | Execute SSH command, capture stdout/stderr |
| `RunWithTimeout` | SSH with configurable timeout (for TUI responsiveness) |
| `RunWithRetry` | Retry on connection errors (5 attempts, 30s delay) |
| `TmuxSessionExists` | Check if tmux session is running |
| `TmuxKillSession` | Kill a tmux session |
| `ReadRemoteFile` | Read file contents (for status/log files) |
| `GetProcessStats` | Fetch CPU, memory, GPU stats from /proc |

**Connection Error Detection:**

```go
var connectionErrorPattern = regexp.MustCompile(
    `(?i)(connection timed out|operation timed out|no route to host|...)`
)
```

The SSH layer distinguishes connection errors (which may be transient) from command errors (which indicate real failures).

### 4. Session Management (`internal/session/`)

Manages tmux session naming and remote file paths.

**Naming Conventions:**

| Item | Pattern | Example |
|------|---------|---------|
| Tmux session | `rj-{job_id}` | `rj-42` |
| Log file | `~/.cache/remote-jobs/logs/{job_id}-{timestamp}.log` | `42-20251213-143025.log` |
| Status file | `.../{job_id}-{timestamp}.status` | Contains exit code |
| Metadata file | `.../{job_id}-{timestamp}.meta` | Key=value pairs |
| PID file | `.../{job_id}-{timestamp}.pid` | Process ID |

**Wrapper Command:**

Jobs run inside a wrapper that:
1. Logs start timestamp and metadata (including timeout if specified)
2. Changes to working directory (with tilde expansion)
3. Starts timeout monitor in background (if --timeout specified)
4. Runs the actual command, capturing PID for timeout monitoring
5. Captures exit code when job completes
6. Writes exit code to status file
7. Optionally triggers Slack notification

**Timeout Monitor** (optional, if `--timeout` specified):
- Runs in background, monitors elapsed time
- Uses `bc` to parse duration strings (e.g., "2h", "30m", "1h30m")
- Checks every 10 seconds if timeout exceeded
- Kills job if timeout reached, logs timeout message

```bash
echo "=== START $(date) ===" > $LOG_FILE;
# (if timeout) Start background timeout monitor
cd $WORKING_DIR && { ($COMMAND) & CMD_PID=$!; echo $CMD_PID > $PID_FILE; wait $CMD_PID; } 2>&1 | tee -a $LOG_FILE;
EXIT_CODE=${PIPESTATUS[0]};
echo "=== END exit=$EXIT_CODE $(date) ===" >> $LOG_FILE;
echo $EXIT_CODE > $STATUS_FILE $NOTIFY_CMD
```

### 5. TUI (`internal/tui/`)

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea) (Elm architecture) and [Lipgloss](https://github.com/charmbracelet/lipgloss) (styling).

**Architecture:**

```
┌─────────────────────────────────────────────────────────┐
│                      Model                               │
├─────────────────────────────────────────────────────────┤
│  Jobs View:                    Hosts View:               │
│  - jobs []*db.Job              - hosts []*Host           │
│  - selectedIndex               - selectedHostIdx         │
│  - selectedJob (for logs)      - host info cache         │
│  - processStats                                          │
│  - logContent                                            │
├─────────────────────────────────────────────────────────┤
│  Background Operations:                                  │
│  - syncInterval (15s)          - Sync job statuses       │
│  - logRefreshInterval (3s)     - Refresh logs/stats      │
│  - hostRefreshInterval (30s)   - Refresh host info       │
└─────────────────────────────────────────────────────────┘
           │
           │ Update(msg)
           ▼
┌─────────────────────────────────────────────────────────┐
│  Message Types:                                          │
│  - tea.KeyMsg              - Keyboard input              │
│  - jobsRefreshedMsg        - DB query result             │
│  - syncCompletedMsg        - Background sync done        │
│  - logFetchedMsg           - SSH log fetch result        │
│  - processStatsMsg         - CPU/GPU stats               │
│  - hostInfoMsg             - Host system info            │
│  - tickMsg                 - Timer for background ops    │
└─────────────────────────────────────────────────────────┘
           │
           │ View()
           ▼
┌─────────────────────────────────────────────────────────┐
│  ╭─────────────────────────────────────────────────────╮│
│  │ ID   HOST         STATUS    STARTED   COMMAND      ││
│  │ 52   deepthought  ● running 2h ago    python ...   ││
│  │ 51   deepthought  ✗ exit 1  3h ago    python ...   ││
│  ╰─────────────────────────────────────────────────────╯│
│  ╭─────────────────────────────────────────────────────╮│
│  │ Details / Logs                                      ││
│  │ Job 52 on deepthought                               ││
│  │ Cmd: python train.py                                ││
│  │ CPU: 45% (1h23m user, 5m sys)                       ││
│  │ GPU 0: 85% util, 12.5GiB                            ││
│  ╰─────────────────────────────────────────────────────╯│
│  ↑/↓:nav l:logs s:sync n:new r:restart k:kill q:quit   │
└─────────────────────────────────────────────────────────┘
```

**Key TUI Features:**
- Two views: Jobs (default) and Hosts
- Split-screen: list at top, details/logs at bottom
- Background polling for status updates
- Real-time CPU/GPU stats for running jobs
- Modal overlays for job creation and long operations

### 6. Configuration (`internal/config/`)

YAML configuration at `~/.config/remote-jobs/config.yaml`:

```yaml
default_command: tui    # "help", "list", or "tui"
sync_interval: 15       # Seconds between status syncs
log_refresh_interval: 3 # Seconds between log refreshes
host_refresh_interval: 30
```

## Data Flow

### Starting a Job

```
User: remote-jobs run cool30 'python train.py'
                    │
                    ▼
┌───────────────────────────────────────────────────────────────┐
│ cmd/run.go                                                     │
│ 1. Parse args: host="cool30", command="python train.py"        │
│ 2. Get working dir (current dir with ~ substitution)          │
└───────────────────────────────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────────┐
│ internal/db/db.go                                              │
│ 3. INSERT INTO jobs ... RETURNING id                          │
│    status = "starting"                                         │
│    Returns job_id = 42                                         │
└───────────────────────────────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────────┐
│ internal/session/session.go                                    │
│ 4. Generate paths:                                             │
│    - tmuxSession = "rj-42"                                     │
│    - logFile = "~/.cache/remote-jobs/logs/42-20251213-...log" │
│    - statusFile, metadataFile, pidFile                         │
└───────────────────────────────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────────┐
│ internal/ssh/ssh.go                                            │
│ 5. ssh cool30 "tmux has-session -t 'rj-42' ..."               │
│    (check session doesn't exist)                               │
│ 6. ssh cool30 "mkdir -p ~/.cache/remote-jobs/logs"            │
│ 7. ssh cool30 "cat > ...meta << 'EOF'\n...\nEOF"              │
│ 8. ssh cool30 "tmux new-session -d -s 'rj-42' bash -c '...'"  │
└───────────────────────────────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────────┐
│ internal/db/db.go                                              │
│ 9. UPDATE jobs SET status = "running" WHERE id = 42           │
└───────────────────────────────────────────────────────────────┘
```

### Checking Job Status

```
User: remote-jobs sync
            │
            ▼
┌────────────────────────────────────────────────────────┐
│ cmd/sync.go                                             │
│ 1. SELECT DISTINCT host FROM jobs WHERE status='running'│
└────────────────────────────────────────────────────────┘
            │
            ▼
    For each host:
            │
            ▼
┌────────────────────────────────────────────────────────┐
│ For each running job on host:                           │
│ 2. ssh host "tmux has-session -t 'rj-42' && echo YES"  │
│                                                         │
│    If session exists:                                   │
│      -> Job still running (no DB update)                │
│                                                         │
│    If session doesn't exist:                            │
│ 3. ssh host "cat ~/.../42-...status"                   │
│                                                         │
│    If status file exists:                               │
│      -> Job completed, UPDATE with exit code            │
│                                                         │
│    If no status file:                                   │
│      -> Job died unexpectedly, mark as "dead"          │
└────────────────────────────────────────────────────────┘
```

## External Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/spf13/cobra` | CLI framework |
| `github.com/charmbracelet/bubbletea` | TUI framework (Elm architecture) |
| `github.com/charmbracelet/bubbles` | TUI components (text input) |
| `github.com/charmbracelet/lipgloss` | TUI styling |
| `modernc.org/sqlite` | Pure Go SQLite (no CGO) |
| `gopkg.in/yaml.v3` | YAML config parsing |

## Remote Host Requirements

- **tmux**: Creates persistent sessions
- **bash**: Shell for wrapper commands
- **curl**: Slack notifications (optional)
- **nvidia-smi**: GPU stats (optional)

## File Locations

### Local (Client Machine)

| Path | Purpose |
|------|---------|
| `~/.config/remote-jobs/jobs.db` | SQLite database |
| `~/.config/remote-jobs/config.yaml` | Configuration |
| `~/.config/remote-jobs/config` | Legacy config (Slack webhook) |

### Remote (Server)

| Path | Purpose |
|------|---------|
| `~/.cache/remote-jobs/logs/{id}-{ts}.log` | Job output |
| `~/.cache/remote-jobs/logs/{id}-{ts}.status` | Exit code |
| `~/.cache/remote-jobs/logs/{id}-{ts}.meta` | Metadata |
| `~/.cache/remote-jobs/logs/{id}-{ts}.pid` | Process ID |
| `/tmp/remote-jobs-notify-slack.sh` | Notification script (deployed at runtime) |

## Design Decisions

### Why tmux instead of nohup/screen?

- **Session naming**: tmux allows named sessions that can be queried
- **Session persistence**: Sessions survive SSH disconnects
- **Widely available**: Pre-installed on most Linux servers
- **Status checking**: Can detect if session is still running

### Why SQLite?

- **Zero setup**: No server process needed
- **Single file**: Easy backup, portability
- **modernc.org/sqlite**: Pure Go, no CGO required
- **Local queries**: Fast filtering, searching, cleanup

### Why Bubble Tea for TUI?

- **Elm architecture**: Clean separation of state, updates, and views
- **Async-friendly**: Natural handling of SSH operations
- **Good ecosystem**: lipgloss for styling, bubbles for components

### Why embed the notify script?

- **Zero remote setup**: Script deployed with each job
- **Version consistency**: Script matches client version
- **Simpler workflow**: No separate installation step

## Error Handling

### Connection Errors

- Detected via regex pattern matching on SSH output
- Retry logic with configurable attempts and delays
- `--queue-on-fail` flag queues job for later retry

### Job Failures

- Exit code captured in status file
- Jobs without status files marked as "dead"
- Error messages stored in database for debugging

### TUI Resilience

- SSH operations use timeouts to prevent UI blocking
- Failed fetches preserve previous data
- Background operations continue even if some hosts unreachable

## Queue System

The queue system allows jobs to run sequentially on a remote host without requiring the local machine to stay connected.

### Queue Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                          Local Machine                               │
├─────────────────────────────────────────────────────────────────────┤
│  ┌──────────────┐    ┌──────────────┐                               │
│  │  queue add   │───▶│   Database   │  Records job with             │
│  │              │    │   (SQLite)   │  status="queued"              │
│  └──────┬───────┘    └──────────────┘                               │
│         │                                                           │
│         │ SSH: Append to queue file                                 │
│         ▼                                                           │
├─────────────────────────────────────────────────────────────────────┤
│                         Remote Host                                  │
├─────────────────────────────────────────────────────────────────────┤
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │ Queue Runner │◀───│  Queue File  │    │   Job Logs           │  │
│  │ (tmux)       │    │  (.queue)    │    │   (.log, .status)    │  │
│  └──────┬───────┘    └──────────────┘    └──────────────────────┘  │
│         │                                                           │
│         │ For each job: run, capture output, notify                 │
│         ▼                                                           │
│  ┌──────────────┐                                                   │
│  │ Slack Notify │                                                   │
│  │ (on complete)│                                                   │
│  └──────────────┘                                                   │
└─────────────────────────────────────────────────────────────────────┘
```

### Queue Components

**Queue Runner Script** (`cmd/queue-runner.sh`):
- Embedded bash script deployed to remote host
- Runs in a tmux session (`rj-queue-{name}`)
- Processes jobs from queue file in FIFO order
- Continues to next job regardless of exit code
- Handles stop signals gracefully

**Queue File Format** (tab-separated):
```
{job_id}\t{working_dir}\t{command}\t{description}
```

**Queue Status** (`queued`):
- Job status indicating job is in a remote queue awaiting execution
- Jobs are added with `queue add` or `run --queue`
- Transitions to `starting` when the queue runner processes it

### Queue File Structure on Remote

```
~/.cache/remote-jobs/
├── queue/
│   ├── {name}.queue        # Queue file (jobs waiting)
│   ├── {name}.current      # Currently running job ID
│   ├── {name}.runner.pid   # Runner process ID
│   └── {name}.stop         # Stop signal file
├── logs/
│   ├── {job_id}-{ts}.log   # Job output
│   ├── {job_id}-{ts}.status # Exit code
│   └── {job_id}-{ts}.meta  # Metadata
└── scripts/
    └── queue-runner.sh     # Deployed runner script
```

### Queue Command Flow

**Adding a Job:**
```
remote-jobs queue add cool30 'python train.py'
    │
    ├── 1. Record job in SQLite (status: "queued")
    ├── 2. SSH: mkdir -p ~/.cache/remote-jobs/queue/
    └── 3. SSH: echo "job_line" >> ~/.cache/remote-jobs/queue/default.queue
```

**Starting the Runner:**
```
remote-jobs queue start cool30
    │
    ├── 1. Check if runner already exists (tmux has-session)
    ├── 2. Deploy queue-runner.sh to remote
    ├── 3. Deploy notify-slack.sh if Slack configured
    └── 4. SSH: tmux new-session -d -s 'rj-queue-default' bash -c 'queue-runner.sh default'
```

**Runner Processing Loop:**
```
queue-runner.sh default
    │
    ├── Write PID to runner.pid
    ├── While not stopped:
    │   ├── Check for stop signal file
    │   ├── Read first line from queue file
    │   ├── Remove line from queue file (atomic)
    │   ├── Write job ID to .current file
    │   ├── Execute job, capture to log file
    │   ├── Write exit code to .status file
    │   ├── Send Slack notification
    │   └── Clear .current file
    └── Exit gracefully
```

### Queue Jobs

All jobs added with `queue add` or `run --queue` have status `queued` in the database.

**Queue Execution:**
- Jobs are stored both in the local DB and in a remote queue file
- The queue runner processes jobs automatically in FIFO order
- No local machine connection needed after queue runner starts
- Jobs transition from `queued` → `starting` → `running` → `completed`

**Manual Execution:**
- Use `run --from <id>` to manually execute a queued job
- This creates a new job with copied settings, doesn't modify the original
- Allows overriding host, command, or adding timeout
