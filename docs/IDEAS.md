# Future Ideas

Ideas for future enhancements that are not currently prioritized.

## Idle Timeout

Add `--idle-timeout <duration>` flag to kill jobs that stop producing output.

### Use Case
Jobs that hang or get stuck but don't exit. For example:
- Waiting for user input that will never come
- Deadlocked processes
- Network timeouts without proper error handling

### Implementation Challenges
- Requires monitoring log file modification time
- Need a background process or periodic checker
- False positives: jobs that legitimately have long gaps between output
- How to distinguish "no output" from "working quietly"?

### Potential Approach
```bash
# In wrapper script
last_mtime=$(stat -c %Y "$LOG_FILE")
while kill -0 $PID 2>/dev/null; do
  current_mtime=$(stat -c %Y "$LOG_FILE")
  if [ $(($(date +%s) - current_mtime)) -gt $IDLE_TIMEOUT ]; then
    kill $PID
    echo "Killed due to idle timeout" >> "$LOG_FILE"
    break
  fi
  sleep 60
done
```

### Questions
- Should touching the log file count as activity?
- What about jobs that only update other files (not stdout/stderr)?
- Should this be opt-in per job or configurable per host?

## Job Dependencies

Allow jobs to specify dependencies on other jobs.

```bash
# Start job B only after job A completes successfully
remote-jobs run --after 42 cool30 "process-results.py"

# Or in queue
remote-jobs queue add --after 42 cool30 "process-results.py"
```

### Implementation
- Store dependency graph in database
- Queue runner checks dependencies before starting each job
- Handle cycles, missing dependencies
- What about cross-host dependencies?

## Resource Limits

Specify CPU, memory, or GPU requirements.

```bash
remote-jobs run --cpus 4 --mem 16G --gpu 1 cool30 "train.py"
```

### Implementation
- Check available resources before scheduling
- Use cgroups or similar for enforcement
- Queue jobs waiting for resources
- May require host-level resource tracking

## Job Tags

Tag jobs for organization and bulk operations.

```bash
remote-jobs run --tag experiment-v2 --tag ablation cool30 "run.py"
remote-jobs job list --tag experiment-v2
remote-jobs job kill --tag experiment-v2  # Kill all matching
```

## Offline Host Support for Job Operations

Enhance `job kill` and `job move` to work with offline/unreachable hosts.

### Enhanced `job kill`

Support killing jobs and removing queued jobs even when hosts are offline.

```bash
# Kill active jobs and remove queued jobs
remote-jobs job kill 42

# If host is unreachable:
# - Mark job for deferred kill/removal
# - Kill/remove on next successful sync
# - Store pending operations in database
```

**Implementation:**
- Add `deferred_operations` table to database
- Store pending kill/remove operations
- Execute during `sync` when host becomes reachable
- Support force flag to delete from DB immediately without contacting host

### Enhanced `job move`

Queue move operations for execution when host becomes reachable.

```bash
# Move queued job to different host
remote-jobs job move 42 cool100

# If original host is unreachable:
# - Update job's host in database
# - Queue deferred removal from original host's queue file
# - Execute removal on next sync
```

**Implementation:**
- Update database immediately
- Store deferred queue file operation
- Execute during sync when host becomes reachable
- Handle edge cases: job already started, queue file modified externally

### Benefits
- Better offline workflow: can queue operations from laptop
- Resilient to network issues
- Cleaner database state management
- Support for "queue these commands for next sync"

## Notification Channels

Beyond Slack, support other notification methods.

```bash
remote-jobs run --notify discord --notify email cool30 "long-job.sh"
```

- Email notifications
- Discord webhooks
- Generic webhook POST
- Desktop notifications (for local machine)

## Job Templates

Save common job configurations as templates.

```bash
# Save current job as template
remote-jobs job save-template 42 "gpu-training"

# Use template
remote-jobs run --template gpu-training cool30 "train.py --epochs 100"
```

### Storage
- Store in `~/.config/remote-jobs/templates/`
- Template includes: working directory, env vars, timeouts, notification settings
- Allow overriding specific fields

## Multi-Host Scheduling

Automatically select best host based on load, availability, resources.

```bash
# Run on any host from a group
remote-jobs run --hosts cool30,cool100,studio "benchmark.py"
```

### Implementation
- Check load average, available resources on each host
- Score and rank hosts
- Fall back to next host if first fails
- May need host groups/pools configuration

## Job Arrays

Run the same command with different parameters (like SLURM job arrays).

```bash
# Run with different parameters
remote-jobs run --array 1-10 cool30 "process.py --task \$TASK_ID"

# Creates 10 jobs with TASK_ID=1..10
```

### Use Cases
- Parameter sweeps
- Processing multiple input files
- Monte Carlo simulations

## Better Log Management

- Automatic log rotation for long-running jobs
- Compression of old logs
- Stream logs to external storage (S3, etc.)
- Search across all job logs

## Web UI

Browser-based interface as alternative to TUI.

- View all jobs, hosts, queues
- Real-time log streaming
- Start/stop/kill operations
- Historical charts and analytics

### Technology
- Go backend with WebSocket for real-time updates
- Minimal frontend (htmx or similar)
- Optional feature, not required for core functionality
