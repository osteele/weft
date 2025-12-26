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

## Notification Channels

Beyond Slack, support other notification methods.

```bash
remote-jobs run --notify discord --notify email cool30 "long-job.sh"
```

- Email notifications
- Discord webhooks
- Generic webhook POST
- Desktop notifications (for local machine)

## Reconnectable Stay-Attached Mode

Extend the `remote-jobs run --allow` pipeline so the CLI can automatically
reconnect if the SSH tail session drops and expose a standalone
`remote-jobs attach <job-id>` command to resume streaming logs later.

### Enhancements
- Detect lost SSH tail sessions, print a notice, and retry a limited number of
  times before giving up.
- Provide a `remote-jobs attach` helper that reuses the wait-and-tail logic
  without starting a new job.
- Surface clearer status when the job finishes while attached (prompt to exit
  or keep streaming for post-run logs).

## Resource-Aware Plan Scheduling

Implement the reserved `when` block in job plans so submissions can wait for
CPU/RAM/GPU thresholds before dispatching to a host.

```yaml
job:
  host: cool42
  command: python train.py
  when:
    cpu_below: 30
    ram_free_gb: 16
    gpu:
      device: any
      util_below: 40
      memory_free_gb: 12
```

### Requirements
- Poll host stats (existing `internal/ssh` helpers) until thresholds are met.
- Integrate with plan DAG so dependent jobs still respect `depends_on`.
- Allow per-job and per-block defaults (e.g., series block waits for available
  GPU before queueing the next job).

## Job Move Command

Add a dedicated `job move` CLI that relocates queued jobs to a different host,
including when the original host is offline.

```bash
# Move queued job 42 from cool30 to cool100
remote-jobs job move 42 cool100
```

### Requirements
- Update the job's host in the database immediately.
- Remove the job from the original host queue (or queue the removal for the
  next `remote-jobs sync` if the host is unreachable).
- Append the job to the target host's queue with the same metadata/env vars.
- Validate that the job is still queued and hasn't started.
- Provide clear user feedback when operations are deferred due to network
  issues.

### Benefits
- Lets users rebalance or evacuate queues without logging into the remote host.
- Keeps queue state consistent even when moving across unstable connections.
- Builds on the deferred operation model introduced for other host actions.

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
