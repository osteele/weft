# Comparison to SLURM

This document compares `remote-jobs` to SLURM (Simple Linux Utility for Resource Management), a widely-used HPC cluster workload manager.

## TL;DR

**remote-jobs** is designed for individual researchers managing jobs on a few personal machines from a laptop. **SLURM** is designed for shared HPC clusters with multiple users, centralized resource management, and complex job dependencies.

Use **remote-jobs** when:
- You have a few personal machines (like cool30, cool100, studio)
- You SSH from a laptop that sleeps/travels
- You want simple job tracking without cluster infrastructure
- You manually decide resource allocation

Use **SLURM** when:
- Shared cluster with multiple users
- Need automatic resource allocation
- Want job arrays for parameter sweeps
- Need accounting/quotas
- Jobs span multiple nodes

## Architecture Comparison

### Fundamental Design

| Aspect | remote-jobs | SLURM |
|--------|-------------|-------|
| **Architecture** | Decentralized, SSH-based | Centralized cluster management |
| **Controller** | None (client pulls state) | `slurmctld` daemon + `slurmd` on each node |
| **Database** | SQLite on client laptop | Cluster-wide state database |
| **Communication** | Pull model: client queries hosts | Push model: nodes report to controller |
| **Client Requirements** | SSH access only | Must connect to cluster network |
| **Daemon Installation** | None required | Requires daemons on all nodes |

### Key Architectural Difference

**remote-jobs:**
```
Laptop ──SSH──> Host 1 (tmux session)
       └──SSH──> Host 2 (tmux session)
       └──SSH──> Host 3 (tmux session)
```

**SLURM:**
```
                ┌─ Node 1 (slurmd)
Login Node ────>│  Node 2 (slurmd)
                │  Node 3 (slurmd)
                └─ slurmctld (controller)
```

## Feature Comparison

### 1. Resource Management

**remote-jobs:**
- Manual host selection
- No resource allocation
- No awareness of CPU/GPU/memory availability

**SLURM:**
```bash
sbatch --gres=gpu:a100:2 --mem=64G --cpus-per-task=16 job.sh
```
- Automatic allocation based on requirements
- Tracks available resources across cluster
- Queues jobs until resources available
- GPU/CPU/memory reservation

### 2. Scheduling

**remote-jobs:**
- Simple FIFO queue per host
- No priority system
- No fairshare
- No backfill scheduling

**SLURM:**
- Sophisticated scheduling algorithms
- Priority queues with configurable weights
- Fairshare policies (ensure equitable resource distribution)
- Backfill scheduling (runs small jobs while waiting for large job resources)
- QoS (Quality of Service) with limits and priorities

### 3. Multi-Host Jobs

**remote-jobs:**
- Each job runs on exactly one host
- No way to span multiple machines
- No MPI integration

**SLURM:**
```bash
sbatch --nodes=4 --ntasks-per-node=8 mpi_job.sh
```
- Allocate jobs across multiple nodes
- Integrated with MPI, OpenMPI
- Network topology awareness
- InfiniBand support

### 4. Job Dependencies & Workflows

**remote-jobs:**
- Sequential queue per host only
- No dependency tracking between jobs

**SLURM:**
```bash
# Job 2 runs after job 1 completes successfully
sbatch --dependency=afterok:12345 job2.sh

# Job arrays for parameter sweeps (100 jobs, max 10 concurrent)
sbatch --array=1-100%10 sweep.sh

# Complex dependency graphs
sbatch --dependency=afterok:12345:12346,afterany:12347 job.sh
```
- Complex dependency graphs
- Job arrays for parameter sweeps
- Workflow management (singleton, afternotok, etc.)

### 5. Multi-User Support & Accounting

**remote-jobs:**
- Single user
- No resource limits
- No accounting
- No isolation between users

**SLURM:**
- Multi-user with cgroups isolation
- Per-user/group quotas
- Detailed accounting (CPU hours, GPU hours, billing)
- `sacct` for usage reports
- Fair-share scheduling ensures equitable access
- Association-based limits (users, groups, accounts)

### 6. Job Control

| Feature | remote-jobs | SLURM |
|---------|-------------|-------|
| **Submit job** | `remote-jobs run <host> <cmd>` | `sbatch script.sh` |
| **Interactive job** | `ssh <host>` | `srun --pty bash` |
| **Job array** | Not supported | `--array=1-100` |
| **Kill job** | `remote-jobs kill <id>` | `scancel <jobid>` |
| **Job status** | `remote-jobs job status <id>` | `squeue -j <jobid>` |
| **Job history** | `remote-jobs job list` | `sacct` |
| **Modify queued job** | Not supported | `scontrol update job` |
| **Hold/release** | Not supported | `scontrol hold/release` |

### 7. Resource Visibility

**remote-jobs:**
- `remote-jobs tui` shows host info (cached)
- `remote-jobs host info <host>` shows system details
- `remote-jobs host load <host>` shows current load
- Manual per-host checking

**SLURM:**
```bash
sinfo              # Cluster-wide resource view
squeue             # All queued/running jobs
sstat <jobid>      # Real-time resource usage
sacct <jobid>      # Historical resource usage
```
- Unified cluster view
- Real-time resource tracking
- Historical usage analysis

### 8. Environment & Modules

**remote-jobs:**
- User manages environment setup
- Command runs in user's shell
- No module system integration

**SLURM:**
- Integrated with environment modules
- `#SBATCH --export=ALL` controls environment
- Can load specific module versions
- Reproducible environments

## What remote-jobs Does Better

### 1. Works from Disconnected Laptop

**remote-jobs:**
- Queue jobs while laptop is on Wi-Fi
- Jobs continue running when laptop sleeps
- Check status when laptop wakes up
- No VPN required if hosts are on different networks

**SLURM:**
- Requires connection to cluster network
- Can't submit jobs when disconnected
- Typically requires VPN for remote access

### 2. Zero Infrastructure Setup

**remote-jobs:**
- Install single binary on laptop
- Just needs SSH keys
- No daemons on remote hosts
- Works with any Linux machine you have SSH access to

**SLURM:**
- Install and configure `slurmctld` (controller)
- Install `slurmd` on every compute node
- Configure shared filesystem (typically NFS)
- Set up accounting database
- Configure network, partitions, etc.

### 3. Offline Queueing

**remote-jobs:**
```bash
# Host is unreachable right now
remote-jobs run --queue-on-fail cool30 'python train.py'
# Job queued, will start when host becomes reachable
```

**SLURM:**
- Controller must be reachable to submit jobs
- Nodes must be online (or in known state)

### 4. Simplicity

**remote-jobs:**
- Simple mental model: SSH + tmux + database
- Easy to debug (just SSH to host)
- Minimal abstraction
- Perfect for 2-5 machines

**SLURM:**
- Complex configuration
- Many moving parts (controller, daemons, accounting DB)
- Harder to debug
- Overkill for small setups

### 5. Personal Workflow

**remote-jobs:**
- Designed for individual researchers
- TUI optimized for personal job tracking
- Slack notifications to your personal workspace
- Your laptop is the source of truth

**SLURM:**
- Designed for shared clusters
- Multi-user features add complexity
- Centralized job history

## Potential Enhancements to Bridge the Gap

Some features that could make `remote-jobs` more SLURM-like without sacrificing its design philosophy:

### 1. Resource-Aware Scheduling

```bash
# Automatically picks cool30 or cool100 based on available GPUs
remote-jobs run --require gpu:2,mem:32G 'python train.py'

# Pool of hosts, schedules to first available
remote-jobs queue add --pool ml-cluster 'python train.py'
```

**Implementation:**
- Query host resources during sync
- Track GPU/CPU/memory availability
- Schedule to host with required resources

### 2. Job Arrays

```bash
# Submit 100 jobs for hyperparameter sweep
remote-jobs run --array 1-100 cool30 'python sweep.py --param $TASK_ID'

# Limit concurrent jobs
remote-jobs run --array 1-100%10 cool30 'python sweep.py --param $TASK_ID'
```

**Implementation:**
- Create multiple job records with array ID
- Expand `$TASK_ID` environment variable
- Respect concurrency limit in queue runner

### 3. Job Dependencies

```bash
# Run after jobs 42 and 43 complete successfully
remote-jobs run --after 42,43 cool30 'python analyze.py'

# Run regardless of success/failure
remote-jobs run --after-any 42 cool30 'python cleanup.py'
```

**Implementation:**
- Add dependency tracking to database
- Check dependency status before starting job
- Support multiple dependency types

### 4. Multi-Host Queue

```bash
# Pool = [cool30, cool100, studio], schedules to first available
remote-jobs queue add --pool ml-cluster 'python train.py'
```

**Implementation:**
- Define host pools in config
- Queue runner checks all pool hosts
- Schedule to first host with available resources

### 5. Better Resource Tracking

- Track actual GPU/CPU usage during job execution
- Show GPU utilization in TUI
- Suggest underutilized hosts
- Historical resource usage per job

## Use Case: Your Research Workflow

For managing research jobs on cool30/cool100/studio:

### Why remote-jobs is Ideal

✅ **You're the only user** - No need for multi-user features
✅ **Work from laptop** - Can queue jobs from anywhere
✅ **Laptop travels/sleeps** - Jobs persist, sync when reconnected
✅ **Simple per-host queues** - Sufficient for your workflow
✅ **Zero infrastructure** - No daemons to maintain
✅ **Offline queueing** - Queue when host unreachable (e.g., cool30 on Tsinghua network)

### Why SLURM Would Be Overkill

❌ **Requires infrastructure** - Install slurmctld + slurmd on each machine
❌ **Needs always-on controller** - Can't run from laptop
❌ **Multi-user complexity** - Features you don't need
❌ **Network requirements** - Must be on cluster network
❌ **More to maintain** - Daemons, configs, accounting DB

### What You'd Benefit From

1. **Resource-aware scheduling** - "Run this wherever there's a free GPU"
2. **Job arrays** - Hyperparameter sweeps
3. **Better monitoring** - Real-time GPU utilization
4. **Multi-host queue** - Pool of [cool30, cool100, studio]

## Recent Fixes: Status Synchronization

The sync bug we recently fixed (jobs showing "running" in list but "dead" in status) illustrates a fundamental difference:

### SLURM Approach
- Centralized controller knows true state
- Nodes report status to controller
- Single source of truth
- No synchronization lag

### remote-jobs Approach (Before Fix)
- Decentralized: client polls each host
- Fast sync optimization skipped queue jobs
- Temporary inconsistency between commands
- Database could be stale

### remote-jobs Approach (After Fix)
- Optimized single-command status check
- Fast sync now includes queue jobs
- Consistent status across all commands
- Gets most of SLURM's benefit without centralized infrastructure

The fix demonstrates that with careful optimization, a decentralized architecture can achieve consistency without the complexity of a centralized controller.

## Conclusion

**remote-jobs** and **SLURM** serve different use cases:

- **remote-jobs**: Personal job management, works from laptop, zero infrastructure
- **SLURM**: Enterprise HPC, shared resources, complex workflows

For individual researchers with a few machines, `remote-jobs` provides the essential features (persistent jobs, queueing, status tracking) with much lower complexity. For large shared clusters, SLURM's centralized architecture and multi-user features are essential.

The choice depends on your scale and requirements, not on which tool is "better."
