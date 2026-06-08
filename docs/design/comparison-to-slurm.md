# Comparison to SLURM

This document compares `weft` to SLURM (Simple Linux Utility for Resource
Management), a widely used HPC cluster workload manager.

## TL;DR

**weft** is designed for researchers who run jobs across a mix of unmanaged lab
machines and short-term rentals from heterogeneous, failure-prone GPU markets
such as Vast.ai and RunPod. It treats SSH reachability, laptop disconnection,
cloud bootstrap failures, preemptions, stale rentals, and uneven machine quality
as normal operating conditions.

**SLURM** is designed for centrally administered HPC clusters with managed
nodes, a persistent controller, shared policy, multi-user accounting, and strong
resource enforcement.

Use **weft** when:

- Your capacity is a changing mix of lab machines, borrowed cluster boxes, and
  temporary rental GPUs
- Machines are not uniformly administered and may only share SSH access, a
  project checkout, and a few conventions
- You want placement across known hosts, existing cloud instances, and new
  rentals
- You need data-locality-aware placement, runtime estimates, rental setup-cost
  estimates, and survival-risk-adjusted cloud offer selection
- You want jobs to keep running when your laptop sleeps or VPN drops
- You need automatic relaunch, stall detection, and runaway protection for
  unreliable rental instances

Use **SLURM** when:

- You operate or use a shared managed cluster
- You need enforced CPU, memory, GPU, and wall-time reservations
- You need job arrays, partitions, QoS, fairshare, accounting, or quotas
- Jobs span multiple nodes with MPI or other tightly coupled distributed runtime
- The cluster has administrators who can maintain controllers, node daemons,
  shared storage, and policy

**Hybrid**: weft has an experimental SLURM backend that can act as a frontend for
SLURM clusters, though this path has not been tested recently. In that mode,
SLURM remains the real scheduler and weft adds its job tracking and workflow
surface above it.

## Architecture Comparison

### Fundamental Design

| Aspect | weft | SLURM |
|--------|------|-------|
| **Architecture** | Federated CLI/TUI, SSH, queue-runner, and cloud-agent model for unmanaged hosts and cloud rentals | Centralized cluster management |
| **Capacity model** | Inventory hosts, existing cloud instances, and new rental offers | Fixed managed nodes grouped into partitions |
| **Controller** | Local CLI/TUI/autopilot; edge runners/agents keep jobs moving | `slurmctld` controller plus `slurmd` on each node |
| **Database** | Local SQLite, remote queue state, and cloud live-state markers | Cluster-wide controller/accounting state |
| **Communication** | SSH, provider APIs, and R2/live-state sync where needed | Nodes continuously report to controller |
| **Failure model** | Unreliable hosts and rentals are expected; retry, relaunch, survival scoring, and deferred operations are part of the workflow | Nodes are managed by the cluster; failures are handled inside an administrative domain |
| **Client requirements** | SSH/provider credentials; no always-on controller required | Access to the SLURM login/controller network |
| **Daemon installation** | Queue runner or agent is deployed as needed per host/instance | Daemons must be installed and configured on all nodes |
| **Resource enforcement** | Cooperative placement and queue-runner concurrency; no cgroup reservation | Scheduler-enforced CPU, GPU, memory, and wall-time limits |
| **SLURM integration** | Optional `sbatch`/`squeue`/`scancel` backend | Native |

### Key Architectural Difference: Hybrid Edge/Cloud Operation

The core difference is where authority and failure recovery live.

**weft** keeps execution authority near the machines that run the jobs:

```text
Laptop CLI/TUI
   │
   ├── scores lab hosts
   ├── reuses existing rentals
   ├── launches Vast.ai/RunPod rentals
   ├── dispatches via SSH / queue files / R2
   └── records placement reasons

Lab host or rental instance
   ├── queue runner / weft agent
   ├── tmux or agent-managed job process
   ├── local logs and status
   └── cloud live-state / result sync when applicable
```

When the laptop disconnects, already-dispatched jobs keep
running. Deferred operations replay when contact returns. Rental launches can be
watched, diagnosed, retried, and replaced without assuming the original instance
will survive.

**SLURM** centralizes scheduling in the cluster controller:

```text
                    ┌─ Node 1 (slurmd) ─┐
slurmctld ◄────────►│  Node 2 (slurmd)  │◄──── constant communication
(controller)        │  Node 3 (slurmd)  │
                    └───────────────────┘
        ▲
        │
   Login Node (sbatch, squeue, scancel)
```

SLURM is stronger when a managed cluster exists: the controller owns global
policy, resource reservation, accounting, fairshare, partitions, and node state.
weft is stronger when the "cluster" is a changing set of machines that were not
designed to be one administered system.

## Feature Comparison

### 1. Resource and Placement Model

**weft:**

- Jobs may omit a host; Weft scores eligible
  inventory hosts, existing cloud instances, and possible new rental instances
- Placement uses GPU constraints (`--gpu`, `--gpu-class`, `--gpu-mem`), CPU
  allotment, queue depth, live utilization, data locality, transfer estimates,
  setup cost, and runtime prediction
- Reserved tags steer placement: `rental`, `inventory`, `provider:<name>`,
  `interruptible`, `cpu-intensive`, `exclusive`, and `benchmark-isolation`
- Data-locality inputs such as `--input hf:<model>` reward hosts that already
  have the model cached and can trigger pre-staging before dispatch
- `exclusive` and `benchmark-isolation` cover isolation cases on unmanaged machines, but
  they are cooperative conventions rather than enforced reservations

**SLURM:**

```bash
sbatch --gres=gpu:a100:2 --mem=64G --cpus-per-task=16 job.sh
```

- Allocates managed cluster resources from declared requirements
- Tracks node, CPU, GPU, memory, and partition state centrally
- Enforces reservations with scheduler policy and cgroups
- Queues work until matching resources are available

### 2. Cloud Offer Selection and Survival Estimation

**weft:**

- Directly provisions Vast.ai and RunPod instances for jobs that should spill to
  rental capacity
- Compares local queue wait against cloud setup time, data transfer time, rental
  rate, and predicted runtime
- Uses a Beta-Binomial survival model for cloud offers, grouped by provider/GPU
  family/price bucket with provider reliability and global backstops
- Applies machine-level penalties when provider metadata exposes a physical
  machine identifier with enough history
- Rejects offers below a configurable survival floor by default and exposes
  `weft campaign survival` for inspection
- Models expected rental cost under retries, not just nominal hourly price
- Detects bootstrap/setup stalls from historical duration distributions and can
  relaunch orphaned jobs on replacement instances

**SLURM:**

- Does not select public marketplace offers
- Does not model rental survival probability or machine-specific marketplace
  reliability
- Cloud bursting is possible through separate products or site-specific
  integrations, but it is outside core SLURM behavior

### 3. Scheduling and Autopilot

**weft:**

- Per-host queue runners start jobs cooperatively based on FIFO order, CPU
  allotment, and isolation tags
- Autopilot can place unplaced jobs, launch cloud instances, reuse existing
  rentals, relaunch orphans, and rebalance queued work between rentals
- A singleton autopilot state prevents multiple TUI/CLI runners from racing
- Pause/resume and blocked-state commands make unattended automation explicit
- A runaway breaker pauses repeated relaunch failures for a scope instead of
  churning through rental attempts
- No fairshare, QoS, priority weights, or backfill scheduler

**SLURM:**

- Mature centralized scheduling with priority policies, fairshare, QoS, backfill,
  reservations, limits, and partitions
- Better fit for shared cluster governance and predictable resource policy
- Does not natively manage ad hoc rental lifecycle or laptop-disconnected edge
  queues

### 4. Failure Recovery and Lifecycle Management

**weft:**

- Cloud launch records track provider, offer, instance, interruptible/preemptible
  status, relaunch chains, termination reasons, and live bootstrap state
- `weft instance watch` and autopilot can keep instances alive for debugging,
  relaunch replacement instances, and reconnect orphaned jobs
- Bootstrap phases, setup stalls, agent readiness, and cloud live state are shown
  in the TUI and stored for later analysis
- Deferred kill, move, and queue edits can replay when hosts return
- Job moves include safeguards so autopilot does not place a job while a move is
  in progress

**SLURM:**

- Handles managed node drain/down/requeue workflows inside the cluster
- Provides strong admin tools for node health, reservations, and job requeue
  policy
- Assumes the cluster owns the machines; it is not designed around replacing
  unreliable marketplace instances per job campaign

### 5. Multi-Host Jobs

**weft:**

- Each job runs on exactly one host or cloud instance
- No MPI integration
- No topology-aware multi-node allocation

**SLURM:**

```bash
sbatch --nodes=4 --ntasks-per-node=8 mpi_job.sh
```

- Allocates jobs across multiple nodes
- Integrates with MPI/OpenMPI and cluster interconnects
- Can account for partitions, reservations, topology, and node constraints

### 6. Job Dependencies and Workflows

**weft:**

```bash
# Run after job 42 completes successfully
weft run --after 42 'python analyze.py'

# Run regardless of job 42's exit code
weft run --after-any 42 'python cleanup.py'

# YAML plans for multi-job workflows
weft plan run workflow.yaml
```

- `--after` and `--after-any` dependency flags
- YAML plan files with `parallel` and `series` blocks
- Rental dependencies act as placement gates; downstream jobs wait before cloud
  selection
- No native job arrays

**SLURM:**

```bash
sbatch --dependency=afterok:12345 job2.sh
sbatch --array=1-100%10 sweep.sh
```

- Rich dependency types
- Job arrays for parameter sweeps
- Mature ecosystem integration with workflow managers

### 7. Multi-User Support and Accounting

**weft:**

- Optimized for one researcher or a small trusted lab workflow
- No fairshare, quotas, account hierarchy, chargeback, or user isolation
- Shared-host flags can avoid placing jobs on multi-tenant machines, but this is
  not a substitute for cluster policy

**SLURM:**

- Multi-user accounting with `sacct`
- Fairshare and association-based limits
- Per-user/group/account quotas
- Enforced isolation and cluster governance

### 8. Job Control

| Feature | weft | SLURM |
|---------|------|-------|
| **Submit job** | `weft run <cmd>` or `weft run <host> <cmd>` | `sbatch script.sh` |
| **Automatic placement** | Omit host; Weft scores targets | Scheduler allocates nodes from requested resources |
| **Force rental** | `weft run --tag rental --gpu a100 <cmd>` | Site-specific cloud integration |
| **Interactive work** | `ssh <host>` or connect to instance | `srun --pty bash` |
| **Job array** | Not supported | `--array=1-100` |
| **Kill job** | `weft kill <id>` | `scancel <jobid>` |
| **Pause/resume** | `weft pause/resume <id>` | `scontrol hold/release` |
| **Job status** | `weft job status <id>` | `squeue -j <jobid>` |
| **Job history** | `weft job list` | `sacct` |
| **Dependencies** | `--after <id>`, `--after-any <id>` | `--dependency=afterok:<id>` |
| **Modify queued job** | `weft queue edit <id>` | `scontrol update job` |
| **Move queued job** | `weft job move <id> <target>` | Requeue/update within scheduler policy |
| **Reorder queue** | `weft queue front <id>` | Priority/QoS |
| **Job plans** | `weft plan run <file>` | External workflow managers |
| **Cloud lifecycle** | `weft instance launch/watch/status/ssh` | Outside core SLURM |
| **Autopilot** | `weft autopilot run/status/pause/blocked` | Scheduler policy, not rental lifecycle automation |

### 9. Resource Visibility

**weft:**

- Grouped TUI for jobs, unplaced work, launching instances, running jobs, and
  completions
- Web dashboard for cluster view, GPU utilization, placement
  decisions, and job details
- Real-time CPU/GPU stats for running jobs
- Progress tracking from job logs (`Progress: N%`, `Progress: N/M`, tqdm-style
  output, and related formats)
- Per-job resource sampling, cloud launch state, bootstrap state, relaunch
  history, and placement reasons
- AI-generated job descriptions and failure diagnostics where configured

**SLURM:**

```bash
sinfo
squeue
sstat <jobid>
sacct <jobid>
```

- Unified cluster-wide resource view
- Real-time and historical resource tracking
- Mature accounting and reporting for managed clusters

### 10. Environment and Modules

**weft:**

- Commands run in the user's shell on the selected machine or in the cloud
  bootstrap environment
- Environment variables and working directories can be specified per job
- Project sync, declared inputs, artifact needs, and bootstrap scripts handle much
  of the cross-machine setup burden
- No cluster module system or enforced environment policy

**SLURM:**

- Commonly integrated with environment modules
- `#SBATCH --export=ALL` and site policy control environment propagation
- Better fit for centrally maintained software stacks

### 11. SLURM Backend

weft includes an experimental SLURM backend:

```bash
# If the remote host has SLURM, weft can submit through sbatch
weft run slurm-host 'python train.py'
```

- Detects SLURM availability on remote hosts
- Submits via `sbatch`, tracks via `squeue`/`sacct`, and cancels via `scancel`
- Maps SLURM states into weft status codes
- Can provide weft's job tracking and UI surface above a SLURM cluster

> **Note**: The SLURM backend has not been tested recently and may need updates.
> The SSH/tmux/agent path and cloud-rental path are the primary weft workflows.

## What weft Does Better

### 1. Works Across Unmanaged and Rental Capacity

**weft:**

- Handles lab machines that were not installed as one managed cluster
- Treats rental GPUs as disposable capacity that may fail during provisioning,
  setup, or execution
- Can choose among inventory hosts, active cloud instances, and fresh rental
  offers for the same job
- Keeps enough metadata to explain placement, relaunch, and survival decisions

**SLURM:**

- Best when the machines are administered as one cluster
- Does not natively reason about public marketplace offer quality or per-machine
  rental survival

### 2. Works from a Disconnected Laptop

**weft:**

- Queue jobs while on intermittent Wi-Fi or VPN
- Jobs continue running when the laptop sleeps
- Dependencies and queue execution continue at the edge after dispatch
- Status, logs, and deferred operations sync when connectivity returns

**SLURM:**

- Submission and control require access to the SLURM cluster
- Login/controller access is usually gated by VPN or cluster network policy

### 3. Zero Cluster Infrastructure

**weft:**

- Install one CLI binary locally
- Use SSH keys and provider credentials
- Deploys queue runners/agents as needed
- Works on ordinary Linux/macOS machines with minimal assumptions

**SLURM:**

- Requires controller and node daemon installation
- Usually needs shared storage, accounting, partitions, limits, and cluster
  administration

### 4. Risk-Aware Cloud Bursting

**weft:**

```bash
weft run --tag rental --gpu a100 'python train.py'
weft instance launch
weft instance watch <id>
weft campaign survival
```

- Provisions Vast.ai/RunPod instances when local capacity is insufficient or too
  slow
- Scores offers by expected completion and cost under setup, transfer, runtime,
  and survival risk
- Suppresses low-survival offers by default
- Automatically relaunches jobs after infrastructure failures within configured
  guardrails

**SLURM:**

- Can be paired with external cloud-bursting systems, but this is an operational
  layer around SLURM rather than core scheduler behavior

### 5. Personal Research Workflow Features

**weft:**

- TUI and web UI optimized for one researcher's active queue
- Job log progress parsing
- Slack notifications to a personal workspace
- YAML job plans for small workflows
- Per-job notes, descriptions, tags, and local history

**SLURM:**

- Designed for shared cluster policy and accounting
- Relies on external tools for many personal workflow features

## Remaining Gaps

Features SLURM has that weft does not support or does not attempt to replace:

### Job Arrays

```bash
sbatch --array=1-100%10 sweep.sh
```

weft has no native equivalent. Parameter sweeps currently require submitting
individual jobs, using scripts, or writing YAML plan files with explicit entries.

### Enforced Resource Isolation

weft's placement and concurrency controls are cooperative. SLURM can enforce CPU,
GPU, memory, wall-time, and account limits through scheduler policy and cgroups.

### Multi-Node and MPI Workloads

weft places one job on one host or instance. SLURM is the right tool for
multi-node jobs, MPI, topology-aware allocation, and tightly coupled distributed
runtime.

### Shared-Cluster Governance

weft does not provide fairshare, QoS, quotas, chargeback, account hierarchies, or
cluster-wide administrative policy. SLURM's complexity buys real capability here.

## Conclusion

`weft` and SLURM serve different centers of gravity:

- **weft**: Hybrid personal/lab/cloud job management across unmanaged machines
  and unreliable short-term rentals, with placement, survival estimation,
  relaunch, and disconnected operation built in
- **SLURM**: Managed multi-user HPC scheduling with strong policy, accounting,
  resource enforcement, and multi-node support

For researchers stitching together lab machines and opportunistic rental GPUs,
weft provides a practical scheduler and control surface without turning those
machines into a formal cluster. For a centrally managed shared cluster, SLURM's
controller, policy, and enforcement model are the right foundation.

When a SLURM cluster is available, weft can in principle sit above it via the
experimental backend, adding its tracking and UI while delegating scheduling to
SLURM. That backend should be treated as secondary until it is refreshed and
tested again.
