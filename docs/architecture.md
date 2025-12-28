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

## Job States

Every job lives in the local SQLite database and transitions through a finite
set of states. The CLI records each transition so commands such as `status`,
`list`, and `sync` can reason about progress even when the host is offline.

| State       | Description                                                                 |
|-------------|-----------------------------------------------------------------------------|
| `starting`  | Job entry created; CLI is preparing the tmux session and remote files.      |
| `running`   | tmux session launched successfully and the wrapper script is executing.     |
| `completed` | Job wrote an exit code to the `.status` file (success or failure recorded). |
| `dead`      | tmux session disappeared without writing a status file (crash/kill).        |
| `queued`    | Job was added to a remote queue file and awaits the queue runner.           |
| `pending`   | Job deferred locally (e.g., `--queue-on-fail`) until a later retry.         |
| `failed`    | CLI could not finish setup (e.g., SSH error) and recorded the failure text. |

```mermaid
stateDiagram-v2
    [*] --> queued : run --queue / plan series
    [*] --> starting : run
    queued --> starting : queue runner / job start
    starting --> running : tmux session ready
    starting --> failed : setup error
    running --> completed : status file written
    running --> dead : tmux gone, no status file
    starting --> pending : queue-on-fail
    pending --> starting : retry/run --from
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
│   ├── ops/               # Unified job operations (CLI + TUI)
│   │   ├── ops.go         # Queue-and-execute pattern, deferred ops
│   │   ├── kill.go        # Kill job operations
│   │   ├── run.go         # Run/restart job operations
│   │   └── ops_test.go    # Operation tests
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

### 2. Operations Layer (`internal/ops/`)

Provides unified job operations used by both CLI and TUI. All operations follow the same queue-and-execute pattern for network resilience.

**Pattern:**
1. Queue the operation in `deferred_operations` table
2. Attempt to drain the queue for that host
3. If host unreachable, operation stays queued for next sync
4. If host reachable, execute and remove from queue

**Key Functions:**

| Function | Purpose |
|----------|---------|
| `KillJob` | Kill a running job's tmux session |
| `RunJob` | Start a new job on a host |
| `RestartJob` | Kill existing job and start a new one with same command |
| `QueueAndExecute` | Core pattern: queue operation, then drain |
| `ExecuteAllDeferredOperations` | Process all pending ops for a host (used by sync) |

**Conflict Resolution:**

When operations conflict, later operations cancel earlier incompatible ones:
- Killing a job removes any pending run/restart operations for that job
- This prevents "zombie" operations from executing after a job is killed

**Benefits:**
- Consistent behavior between CLI and TUI
- Network-resilient: operations survive disconnections
- Idempotent: duplicate operations are detected and skipped

### 3. Database Layer (`internal/db/`)

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

### 4. SSH Layer (`internal/ssh/`)

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

### 5. Session Management (`internal/session/`)

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

### 6. TUI (`internal/tui/`)

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

### 7. Configuration (`internal/config/`)

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

### Remote Queue Runner

Jobs enqueued via `remote-jobs queue add`, `remote-jobs run --queue`, or plan
`series` blocks are executed by a small bash daemon that lives on each host.

- The script is embedded in the binary (`internal/scripts/queue-runner.sh`) and
  deployed on demand to `~/.cache/remote-jobs/scripts/queue-runner.sh`.
- A tmux session named `rj-queue-{queue}` runs the script so it keeps running
  even when you disconnect.
- Queue data is purely file-based to avoid keeping a network service running:
  - `~/.cache/remote-jobs/queue/{queue}.queue`: FIFO list of jobs (tab-separated).
  - `~/.cache/remote-jobs/queue/{queue}.current`: ID of the job currently
    executing (used by `status`/`sync` to detect runner progress).
  - `~/.cache/remote-jobs/queue/{queue}.runner.pid`: PID of the runner itself.
  - `~/.cache/remote-jobs/queue/{queue}.stop`: Presence signals the runner to
    exit after the current job.
- Each queue entry includes base64 encoded environment variables and dependency
  metadata so the runner knows whether it should wait for other jobs’ status
  files before starting.
- Queue entry columns (tab-separated): `job_id`, `working_dir`, `command`,
  `description`, `env_vars_b64`, `dependencies`.

```mermaid
flowchart TD
    A[CLI queues job] --> B["Append line to ~/.cache/remote-jobs/queue/{queue}.queue"]
    B --> C["rj-queue-{queue} tmux session"]
    C --> D{Queue runner loop}
    D -->|Read first entry| E[Write job ID to .current]
    E --> F[Check dependency status files]
    F -->|blocked| G[Re-append job and sleep]
    F -->|ready| H[Create log/status/meta paths]
    H --> I[Run job command]
    I --> J[Write exit code to .status]
    J --> K["Slack notify (optional)"]
    K --> L[Delete .current, loop]
```

**Activity Notes**

- Dependencies: The runner inspects `~/.cache/remote-jobs/logs/{dep}-*.status`
  files. If they are missing it re-queues the job at the end. If a dependency
  failed and the spec required success, the job is marked skipped by writing a
  log/status pair.
- Environment: The queue entry contains base64 encoded `VAR=value` lines. The
  runner decodes and exports them before launching the command.
- Metadata: A `.meta` file is written before execution so later `sync` calls can
  recover `start_time`, display-friendly command, queue name, etc.
- Queue persistence: Because the queue file is just a text file, jobs survive
  remote reboots. Re-starting the runner tmux session picks up where it left
  off.

The combination of queue files plus the runner loop means no long-lived process
is required on the local machine once the job is queued—the remote host and its
tmux sessions orchestrate everything.
