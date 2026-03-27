# Future Ideas

Ideas for future enhancements that are not currently prioritized.

## Disk Full Recovery

When the disk fills up during job execution, the queue runner can get into an inconsistent state because it can't write state files.

### Prevention
- **Pre-flight disk check**: Refuse to start jobs if disk < threshold (e.g., 50MB)
- **Low disk warning**: Alert users before it becomes critical (implemented in TUI)

### Robustness
- **Atomic writes with fsync**: Ensure state files are written completely or not at all
- **Retry logic**: Queue runner should retry failed state file writes
- **Journal/WAL**: Use write-ahead logging for critical state changes

### Recovery
- **Self-healing**: Queue runner detects inconsistent state (current job doesn't exist/already completed) and recovers
- **Sync repairs remote state**: When sync detects a completed job, also clear `default.current` on remote

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

## Resource Enforcement via cgroups

GPU constraints (`--gpu`, `--gpu-class`, `--gpu-mem`) and CPU allotments are
already implemented at the scheduling level. This idea is about kernel-level
enforcement using cgroups to hard-limit CPU, memory, and GPU access per job.

```bash
weft run --cpus 4 --mem 16G titan "train.py"
```

### Implementation
- Use cgroups (v2) to enforce CPU and memory limits per job
- Integrate with existing CPU allotment system for consistency
- Prevent runaway jobs from starving other jobs on shared hosts

## macOS Per-Job Stats

Add per-job CPU/memory/thread stats for macOS hosts in the TUI.

### Motivation
- The job detail CPU panel currently relies on `/proc`, so it reports no per-job
  stats on macOS.
- Host-level top processes now work on macOS, but per-job stats remain empty.

### Possible Approach
- Use `ps -p <pid> -o %cpu=,rss=,comm=` and `ps -M <pid>` or `sysctl`/`proc_pidinfo`
  equivalents to estimate CPU time and thread count.
- Mirror the Linux fields we already show (CPU%, RSS, threads).
- Keep the Linux path unchanged; add a macOS branch in `internal/ssh.GetProcessStats`.

## Notification Channels

Beyond Slack, support other notification methods.

```bash
weft run --notify discord --notify email titan "long-job.sh"
```

- Email notifications
- Discord webhooks
- Generic webhook POST
- Desktop notifications (for local machine)

## Agent Coaching Output

Expose a structured way for the CLI to describe “next steps” so autonomous
agents can more easily keep context. Today commands print helpful follow-up
lines (“run `weft status 42` next”), but the format is unstructured text.

### Idea
- Emit a machine-readable block (JSON or YAML) that mirrors the human-readable
  hints so agent runtimes can parse and insert them into their memories.
- Allow users to opt into different verbosity levels (minimal vs. verbose hints)
  so humans don’t feel overwhelmed while agents still get the detail they need.
- Let `weft plan` emit suggested commands for every job ID it creates,
  making agent chaining even easier.

### Benefits
- Keeps the agent-friendly workflow first-class without requiring bespoke
  “skills” files.
- Helps human+agent pairs stay in sync: humans can read the regular hints,
  agents can consume the structured metadata.

## Reconnectable Stay-Attached Mode

Extend the `weft run --allow` pipeline so the CLI can automatically
reconnect if the SSH tail session drops and expose a standalone
`weft attach <job-id>` command to resume streaming logs later.

### Enhancements
- Detect lost SSH tail sessions, print a notice, and retry a limited number of
  times before giving up.
- Provide a `weft attach` helper that reuses the wait-and-tail logic
  without starting a new job.
- Surface clearer status when the job finishes while attached (prompt to exit
  or keep streaming for post-run logs).

## Deferred Operation Insights

Provide better tooling around the “occasionally connected” design so users and
agents can see exactly what is queued for each host.

### Possibilities
- `weft ops list` showing pending deferred operations, their age, and the
  command that created them.
- TUI panel that highlights hosts with a large backlog so humans know which
  machines need attention.
- Notifications (or next-step hints) when commands finish replaying after a host
  reconnects.

### Impact
- Reinforces the core workflow where agents happily queue work offline while
  humans supervise connectivity.
- Makes it easier to debug “nothing is happening on host X” situations because
  you can inspect the pending queue locally.

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

## Job Templates

Save common job configurations as templates.

```bash
# Save current job as template
weft job save-template 42 "gpu-training"

# Use template
weft run --template gpu-training titan "train.py --epochs 100"
```

### Storage
- Store in `~/.config/weft/templates/`
- Template includes: working directory, env vars, timeouts, notification settings
- Allow overriding specific fields

## Job Arrays

Run the same command with different parameters (like SLURM job arrays).

```bash
# Run with different parameters
weft run --array 1-10 titan "process.py --task \$TASK_ID"

# Creates 10 jobs with TASK_ID=1..10
```

### Use Cases
- Parameter sweeps
- Processing multiple input files
- Monte Carlo simulations

## On-Prem Agent (Pull Model)

Deploy `weft-agent` to on-prem hosts as a persistent service, making them
behave like cloud instances: autonomous, pull-based, and capable of operating
without the laptop or coordinator online.

### Motivation

The cloud agent already demonstrates a pull model: it polls for work, runs
jobs, reports status via R2, and handles data fetching autonomously. On-prem
hosts currently use a push model (laptop SSHes in, appends to queue file, polls
for status). This asymmetry means several coordinator features exist only
because the laptop can't be assumed online.

### What This Enables Without a Coordinator

| Capability | Current state | With on-prem agent |
|---|---|---|
| **Data pre-staging** | Needs coordinator to transfer data before dispatch | Agent pulls declared `--input` deps itself |
| **Async submission** | Needs coordinator or laptop online | Write job to shared queue (R2); agent polls and claims work |
| **Retry on failure** | Coordinator retry drainer, or 60s TUI poll for benchmarks | Job returns to shared queue; any agent can pick it up |
| **Status without laptop** | Laptop must SSH-poll | Agent reports to R2/shared DB; laptop reads on reconnect |
| **Offline placement** | Coordinator resolves intent files later | Agents self-select from unplaced jobs matching their capabilities |

### What Still Benefits from a Coordinator

- **Optimal cross-host scheduling** — When multiple jobs compete for
  heterogeneous hosts with different data already cached, a central authority
  makes better global placement decisions than agents independently claiming
  work.
- **Transfer cost arbitrage** — "Run on atlas because the model is already
  there" requires knowing state across all hosts simultaneously.

Both are soft optimizations. An agent pull model where agents filter by
hardware capabilities and prefer jobs whose `--input` data they already have
gets ~80% of the benefit.

### Implementation Sketch

1. Deploy `weft-agent` as a systemd service on on-prem hosts
2. Agent periodically queries `ListUnplacedJobs()` (or R2 equivalent),
   filtered by its own GPU/memory/disk capabilities
3. Agent claims a job via `AssignJobHost()` (atomic, race-safe)
4. Agent runs the job, reports status to R2 or shared DB
5. On failure, agent releases the job back to unplaced

The `needs_rental` → derived unplaced refactor provides the foundation: agents
query for `status='queued' AND host=''` jobs and claim them.

### Questions

- Should agents use R2 as the job queue (like cloud grace period) or query the
  SQLite DB directly (requires shared DB access or an API)?
- How to handle agent upgrades on on-prem hosts? (Cloud instances are
  ephemeral; on-prem hosts persist.)
- Should the coordinator become optional, or remain as an optimization layer
  that agents consult for placement hints?

## Interruptible (Bid) Instance Support

Allow campaigns to opt into interruptible (bid-priced) instances via
`--rental-type bid --bid-price <$/hr>`. Currently all instances are on-demand
(direct) since `CreateInstance` never passes `--bid_price` to the Vast.ai CLI.

Evaluate when interruptible instances are cost-effective — e.g., for short jobs
where retry overhead is low, or when the bid price discount is large enough to
offset expected preemption costs. The existing survival model in
`internal/bidding/` could be extended to compare expected cost across rental
types.

Vast.ai CLI reference:
- `vastai search offers --type bid` returns bid pricing in `dph_total`
- `vastai create instance <id> --bid_price <$/hr>` creates an interruptible instance
- `min_bid` field in search results shows minimum bid price per offer

## Better Log Management

- Automatic log rotation for long-running jobs
- Compression of old logs
- Stream logs to external storage (S3, etc.)
- Search across all job logs

## Transfer Time Prediction Extensions

The `internal/transferbw` package tracks per-(source, dest) bandwidth via EMA.
Future extensions:

- **Source-sync bandwidth**: Instrument `sync.sources` for source-sync observations
- **Per-host-pair tracking**: When enough data accumulates, split aggregate dest estimates into per-pair
- **`weft host bandwidth` CLI**: Inspect learned estimates and raw observations
- **Time-based decay**: Down-weight observations older than N days
- **Advertised-bandwidth sub-keys**: For cloud instances, bucket by provider-advertised Mbps to get finer-grained estimates (raw observations already store instance IDs for retroactive re-keying)

## Anomaly Detection for Running Jobs

Learn typical GPU utilization and memory curves per job type. Flag jobs that are
likely stuck (GPU idle but process alive) or about to OOM (memory climbing
linearly toward limit).

### Approach

- Collect per-job GPU util / memory time series from `internal/ssh.GetProcessStats`
- Build lightweight baselines per cluster (from workload clustering, below, or
  per-command rolling statistics)
- At runtime, compare a job's trajectory against its baseline and fire alerts:
  - **Stuck job**: GPU utilization drops to near-zero for N minutes while PID is
    still alive
  - **OOM trajectory**: GPU memory increasing linearly with projected
    intersection of the device limit within the next M minutes
- Surface alerts in the TUI job detail view and optionally via Slack notification

### Relation to Idle Timeout

The existing Idle Timeout idea (above) uses output-activity as a proxy.
Anomaly detection uses learned GPU/memory baselines, which catches cases where
the process still writes output but the GPU is idle (e.g., stuck in a data
loading loop).

## Historical Phase Counts as Priors for Progress Estimation

The multi-phase progress tracker (`internal/progress/`) currently uses a fixed
Poisson(λ=2) prior to estimate total phases. Once we have enough completed jobs
with observed phase counts, we can use historical data as an empirical prior.

### Approach

- Record the observed total phase count (from the agent's PhaseTracker) when a
  job completes, stored in the jobs table or a separate stats table.
- When a new job starts, derive a prior from the distribution of phase counts
  of previously completed jobs — optionally filtered by project, command, or
  host to make the prior more specific.
- Replace the fixed λ with the empirical distribution's parameters (e.g., fit a
  Poisson or negative binomial to the histogram of observed phase counts).

### Benefits

- Progress estimates for multi-phase jobs converge faster because the prior
  already reflects typical workloads.
- Projects that routinely run 5-phase sweeps get accurate progress from the
  first restart instead of conservatively estimating 3 phases.

## Workload Clustering

Cluster historical jobs by resource profile (GPU utilization pattern, duration,
peak memory) to automatically discover "job types" without manual labeling.

### Use Cases

- **Cold-start prediction**: New jobs that match a known cluster get reasonable
  duration and resource estimates even without an exact command match in the
  predictor
- **Anomaly baselines**: Per-cluster GPU/memory curves feed the anomaly
  detection system (above)
- **Capacity planning**: Understand the mix of workload types to inform hardware
  purchasing decisions

### Implementation Sketch

- Extract feature vectors from completed jobs: duration, peak RSS, peak GPU mem,
  mean GPU util, GPU util variance, number of GPUs used
- Run a simple clustering algorithm (k-means or HDBSCAN) periodically during
  `weft retrain`
- Store cluster assignments in the jobs database; expose via `weft jobs --cluster`
- Use cluster centroids as priors in the predictor for unseen commands

