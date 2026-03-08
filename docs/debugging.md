# Debugging

When jobs aren't progressing as expected, check both local and remote logs.

## Remote queue runner log

Shows queue start/stop events, job start/complete/fail events:

```bash
ssh <host> 'tail -50 ~/.cache/weft/queue/runner-default.log'
```

## Remote queue state files

```bash
ssh <host> 'cat ~/.cache/weft/queue/default.current'   # Current job ID
ssh <host> 'cat ~/.cache/weft/queue/default.queue'     # Pending jobs
ssh <host> 'cat ~/.cache/weft/queue/default.runner.pid' # Runner PID
```

## Check if queue runner is alive

```bash
ssh <host> 'ps -p $(cat ~/.cache/weft/queue/default.runner.pid) 2>/dev/null || echo "Runner not running"'
```

## Check specific job log

```bash
ssh <host> 'ls ~/.cache/weft/logs/ | grep <job-id>'
ssh <host> 'tail -30 ~/.cache/weft/logs/<job-id>-*.log'
```

## Check if job process is running

```bash
ssh <host> 'cat ~/.cache/weft/logs/<job-id>-*.pid && ps -p $(cat ~/.cache/weft/logs/<job-id>-*.pid) 2>/dev/null || echo "Process not found"'
```

## List tmux sessions

```bash
ssh <host> 'tmux list-sessions'
```

## Operations logs (oplog)

Structured JSON logs of operations performed by the CLI and queue runner:

```bash
# Local ops log (CLI operations)
tail -50 ~/.cache/weft/operations.log

# Remote ops log (queue runner operations) — same as runner log above
ssh <host> 'tail -50 ~/.cache/weft/queue/runner-default.log'
```

These logs record job state changes, sync operations, and queue runner decisions.
Use these to understand why jobs were started/stopped and debug unexpected behavior.

## Process state detection

Jobs have two process identifiers:
- **PID** (`.pid` file): The bash shell wrapper process
- **PGID** (`.pgid` file): The process group leader (e.g., `uv`, `python`)

When checking if a job is paused or running:

```bash
# Check both PID and PGID states
ssh <host> 'for job in <job-id>; do
  pgid=$(cat ~/.cache/weft/logs/$job.pgid 2>/dev/null)
  pid=$(cat ~/.cache/weft/logs/$job.pid 2>/dev/null)
  echo "Job $job: pgid=$pgid, pid=$pid"
  [ -n "$pgid" ] && echo "  PGID state: $(ps -o stat= -p $pgid 2>/dev/null)"
  [ -n "$pid" ] && echo "  PID state: $(ps -o stat= -p $pid 2>/dev/null)"
done'
```

**Important**: Pause detection checks the PGID, not the PID. When a job is paused:
- The PGID (process group leader) will have state `T` (stopped)
- The PID (bash wrapper) may still show state `S+` (sleeping/running)

This is expected behavior because we signal the process GROUP (`-pgid`), not the wrapper shell.
The bash shell remains "running" while waiting for its stopped children.
