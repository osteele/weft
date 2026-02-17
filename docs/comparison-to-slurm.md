# Comparison to SLURM

This document compares `remote-jobs` to SLURM (Simple Linux Utility for Resource Management), a widely-used HPC cluster workload manager.

## TL;DR

**remote-jobs** is designed for individual researchers managing jobs on a few personal machines from a laptop. **SLURM** is designed for shared HPC clusters with multiple users, centralized resource management, and complex job dependencies.

Use **remote-jobs** when:
- You have a few personal machines (like cool30, cool100, studio)
- You SSH from a laptop that sleeps/travels
- You want simple job tracking without cluster infrastructure
- You need jobs to keep running when your laptop is off

Use **SLURM** when:
- Shared cluster with multiple users
- Need automatic resource allocation across many nodes
- Want job arrays for parameter sweeps
- Need accounting/quotas
- Jobs span multiple nodes (MPI)

**Hybrid**: remote-jobs has an experimental SLURM backend that can act as a frontend for SLURM clusters, though this has not been tested recently.

## Architecture Comparison

### Fundamental Design

| Aspect | remote-jobs | SLURM |
|--------|-------------|-------|
| **Architecture** | Decentralized, SSH-based | Centralized cluster management |
| **Controller** | Autonomous queue runner per host | `slurmctld` daemon + `slurmd` on each node |
| **Database** | SQLite on client laptop | Cluster-wide state database |
| **Communication** | Pull model: client queries hosts | Push model: nodes report to controller |
| **Client Requirements** | SSH access only | Must connect to cluster network |
| **Daemon Installation** | None (queue runner auto-deployed) | Requires daemons on all nodes |
| **SLURM Integration** | Optional: can submit via sbatch | N/A |

### Key Architectural Difference: Occasionally-Connected Operation

The fundamental difference is where job execution authority lives:

**remote-jobs** — Queue runner lives on each remote host:
```
Laptop (may disconnect)
   │
   ├──SSH──> Host 1: queue-runner (autonomous) → tmux sessions
   ├──SSH──> Host 2: queue-runner (autonomous) → tmux sessions
   └──SSH──> Host 3: queue-runner (autonomous) → tmux sessions

When laptop disconnects:
   - Queue runners continue processing jobs independently
   - Jobs complete, new queued jobs start automatically
   - Dependencies are resolved on-host without the laptop
   - Laptop syncs state when it reconnects
```

**SLURM** — Centralized controller manages all nodes:
```
                    ┌─ Node 1 (slurmd) ─┐
slurmctld ◄────────►│  Node 2 (slurmd)  │◄──── constant communication
(controller)        │  Node 3 (slurmd)  │
                    └───────────────────┘
        ▲
        │
   Login Node (sbatch, squeue, scancel)
        │
   Must be reachable to submit/monitor jobs
```

**Why this matters:**

- **remote-jobs**: Your laptop can sleep, lose network, or be off entirely. The remote queue runners are self-sufficient — they read from the queue file, resolve dependencies, start jobs, handle completions, and log everything locally. When you reconnect, your laptop just syncs state.

- **SLURM**: The controller (`slurmctld`) must be reachable for job submission (`sbatch`), status queries (`squeue`), and cancellation (`scancel`). The controller is the single source of truth and orchestrates all job scheduling.

## Feature Comparison

### 1. Resource Management

**remote-jobs:**
- Manual host selection (user picks which machine)
- Per-job CPU allotments control concurrency (queue runner keeps total CPU under a target cap)
- `exclusive` tag for jobs needing sole access to a host's resources
- `benchmark` tag waits for system-wide idle (CPU, RAM, GPU, VRAM below thresholds)
- No automatic host selection based on resource requirements

**SLURM:**
```bash
sbatch --gres=gpu:a100:2 --mem=64G --cpus-per-task=16 job.sh
```
- Automatic allocation based on declared requirements
- Tracks available resources across the entire cluster
- Queues jobs until matching resources are available
- GPU/CPU/memory reservation with cgroups enforcement

### 2. Scheduling

**remote-jobs:**
- FIFO queue per host with concurrent job execution
- CPU allotment system: each job declares expected CPU usage; the runner starts multiple jobs until the host utilization target is reached
- Learned CPU history (exponential moving average) for accurate allotment estimates
- `exclusive` and `benchmark` tags for isolation when needed
- No priority system, fairshare, or backfill scheduling

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
```bash
# Run after job 42 completes successfully
remote-jobs run --after 42 cool30 'python analyze.py'

# Run regardless of job 42's exit code
remote-jobs run --after-any 42 cool30 'python cleanup.py'

# YAML plans for multi-job workflows
remote-jobs plan run workflow.yaml
```
- `--after` (success required) and `--after-any` (any completion) dependency flags
- YAML plan files with `parallel` and `series` blocks for multi-job workflows
- Dependencies resolved on-host by the queue runner (no laptop needed)
- No job arrays

**SLURM:**
```bash
# Job 2 runs after job 1 completes successfully
sbatch --dependency=afterok:12345 job2.sh

# Job arrays for parameter sweeps (100 jobs, max 10 concurrent)
sbatch --array=1-100%10 sweep.sh

# Complex dependency graphs
sbatch --dependency=afterok:12345:12346,afterany:12347 job.sh
```
- Complex dependency graphs with multiple dependency types
- Job arrays for parameter sweeps
- Workflow management (singleton, afternotok, etc.)

### 5. Multi-User Support & Accounting

**remote-jobs:**
- Single user
- No resource limits or quotas
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
| **Queue job** | `remote-jobs queue add <host> <cmd>` | `sbatch script.sh` |
| **Interactive job** | `ssh <host>` | `srun --pty bash` |
| **Job array** | Not supported | `--array=1-100` |
| **Kill job** | `remote-jobs kill <id>` | `scancel <jobid>` |
| **Pause/resume** | `remote-jobs pause/resume <id>` | `scontrol hold/release` |
| **Job status** | `remote-jobs job status <id>` | `squeue -j <jobid>` |
| **Job history** | `remote-jobs job list` | `sacct` |
| **Dependencies** | `--after <id>`, `--after-any <id>` | `--dependency=afterok:<id>` |
| **Modify queued job** | `remote-jobs queue edit <id>` | `scontrol update job` |
| **Reorder queue** | `remote-jobs queue front <id>` | Priority/QoS |
| **Job plans** | `remote-jobs plan run <file>` | Not built-in (workflow managers) |

### 7. Resource Visibility

**remote-jobs:**
- TUI with split-screen job list and detail pane
- Web UI for browser-based monitoring (`remote-jobs web`)
- Real-time CPU/GPU stats for running jobs
- Progress tracking (parses `Progress: N%` or `Progress: N/M` from logs)
- Per-job resource usage sampling
- Host info and load commands
- AI-generated job descriptions (via ollama)

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
- Environment variables can be passed per job
- No module system integration

**SLURM:**
- Integrated with environment modules
- `#SBATCH --export=ALL` controls environment
- Can load specific module versions
- Reproducible environments

### 9. SLURM Backend

remote-jobs includes an experimental SLURM backend that can submit jobs to SLURM clusters:

```bash
# If the remote host has SLURM, remote-jobs automatically uses sbatch
remote-jobs run slurm-host 'python train.py'
```

- Auto-detects SLURM availability on remote hosts
- Submits via `sbatch`, tracks via `squeue`/`sacct`, cancels via `scancel`
- Maps SLURM states to remote-jobs status codes
- Provides the same offline queueing and TUI on top of SLURM infrastructure

> **Note**: The SLURM backend has not been tested recently and may need updates. The core SSH/tmux-based queue system is the primary and well-tested path.

This means remote-jobs is not strictly an alternative to SLURM — it can also be a more ergonomic frontend for it.

## What remote-jobs Does Better

### 1. Works from Disconnected Laptop

**remote-jobs:**
- Queue jobs while laptop is on Wi-Fi
- Jobs continue running when laptop sleeps
- Dependencies resolve on-host without the laptop
- Check status when laptop wakes up
- Offline log cache for viewing completed job output without SSH

**SLURM:**
- Requires connection to cluster network
- Can't submit jobs when disconnected
- Typically requires VPN for remote access

### 2. Zero Infrastructure Setup

**remote-jobs:**
- Install single binary on laptop
- Just needs SSH keys
- Queue runner script auto-deployed to remote hosts
- Works with any Linux/macOS machine you have SSH access to

**SLURM:**
- Install and configure `slurmctld` (controller)
- Install `slurmd` on every compute node
- Configure shared filesystem (typically NFS)
- Set up accounting database
- Configure network, partitions, etc.

### 3. Offline Queueing

**remote-jobs:**
```bash
# Host is unreachable right now — no problem
remote-jobs run cool30 'python train.py'
# Job recorded locally, synced when host comes back
```
- All operations (run, kill, queue edits) work offline
- Deferred operations replay automatically on reconnect

**SLURM:**
- Controller must be reachable to submit jobs
- Nodes must be online (or in known state)

### 4. Simplicity

**remote-jobs:**
- Simple mental model: SSH + tmux + SQLite
- Easy to debug (SSH to host, check queue files)
- Minimal abstraction
- Perfect for 2-5 machines

**SLURM:**
- Complex configuration
- Many moving parts (controller, daemons, accounting DB)
- Harder to debug
- Overkill for small setups

### 5. Personal Workflow Features

**remote-jobs:**
- TUI and web UI optimized for personal job tracking
- Slack notifications to your personal workspace
- AI-generated job descriptions
- Progress bar parsing from job output
- Job plans (YAML) for orchestrating multi-step workflows
- Pause/resume running jobs

**SLURM:**
- Designed for shared clusters
- Multi-user features add complexity
- Centralized job history
- No built-in progress parsing or AI features

## Potential Enhancements

Features that could further bridge the gap without sacrificing remote-jobs' design philosophy:

### 1. Resource-Aware Scheduling

```bash
# Automatically picks cool30 or cool100 based on available GPUs
remote-jobs run --require gpu:2,mem:32G 'python train.py'
```

### 2. Job Arrays

```bash
# Submit 100 jobs for hyperparameter sweep
remote-jobs run --array 1-100 cool30 'python sweep.py --param $TASK_ID'
```

### 3. Multi-Host Queue

```bash
# Pool = [cool30, cool100, studio], schedules to first available
remote-jobs queue add --pool ml-cluster 'python train.py'
```

## Conclusion

**remote-jobs** and **SLURM** serve different use cases:

- **remote-jobs**: Personal job management, works from laptop, zero infrastructure, experimental SLURM backend
- **SLURM**: Enterprise HPC, shared resources, multi-user accounting, multi-node jobs

For individual researchers with a few machines, `remote-jobs` provides job dependencies, concurrent scheduling, progress tracking, and a TUI/web interface — all with much lower complexity than SLURM. For large shared clusters, SLURM's centralized architecture and multi-user features are essential.

When a SLURM cluster is available, remote-jobs can in principle sit on top of it via its experimental SLURM backend, adding offline queueing and its own monitoring interface while delegating actual scheduling to SLURM. This backend has not been tested recently and may need updates.
