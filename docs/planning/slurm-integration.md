# SLURM Integration Plan

Status: **Plan** — describes intended design, not current implementation.

## Context

Weft has an existing but lightly-tested SLURM backend (`internal/ops/slurm.go`)
that submits jobs via `sbatch`, probes status via `squeue`/`sacct`, and cancels
via `scancel`. The backend is auto-detected by `probeBackend()` when a host has
SLURM tools installed, or can be pinned per-host in config.

This plan describes how to evolve that backend into a first-class integration
where weft serves as an ergonomic personal frontend to a shared SLURM cluster.

## Design Principle

Weft is a **facade over SLURM**, not a replacement. SLURM owns scheduling,
resource allocation, and multi-user accounting. Weft adds the
occasionally-connected laptop workflow, richer monitoring, and personal
convenience features on top.

## What Weft Facades (SLURM Does the Work)

These SLURM features are delegated to SLURM — weft translates its commands into
the corresponding SLURM operations:

| Weft operation | SLURM equivalent | Current state |
|---|---|---|
| `weft run <host> <cmd>` | `sbatch --wrap` | Implemented |
| `weft kill <id>` | `scancel` | Implemented |
| Job status polling | `squeue` (active) / `sacct` (historical) | Implemented |
| GPU resource requests (`--gpu`, `--gpu-mem`) | `--gres=gpu:TYPE:N` | **Not yet mapped** |
| CPU/memory requests | `--cpus-per-task`, `--mem` | **Not yet mapped** |
| Working directory | `--chdir` | Implemented |
| Environment variables | `--export` | Implemented (via wrapper script) |
| Job name | `--job-name` | Implemented |
| Time limit (`--timeout`) | `--time` | **Not yet mapped** |

## What Weft Adds on Top

Features that don't exist in SLURM's CLI, or that weft provides a better
experience for:

### Occasionally-connected operation
SLURM requires cluster network access for `sbatch`/`squeue`/`scancel`. Weft
records jobs locally and replays them when the laptop reconnects. Status is
cached in the local SQLite DB so `weft job list` works offline.

### TUI and web monitoring
SLURM's `squeue` is a table dump. Weft's TUI provides a split-pane job list
with live detail view, progress bars, and host overview. The web UI
(`weft web`) gives browser access.

### Progress tracking
SLURM has no built-in progress parsing. Weft extracts `Progress: N%` or
`Progress: N/M` from job logs and displays progress bars in the TUI.

### Log caching and viewing
`sacct` provides metadata but not log content. Weft caches job output locally
(`~/.cache/weft/logs/`) so you can read completed job logs without SSH.

### Job dependencies with offline resolution
SLURM supports `--dependency=afterok:ID` but requires the controller to be
reachable. Weft's `--after` flag works with locally-queued jobs and resolves
when syncing with the host.

### Placement across inventory and rental instances
Weft's placement scorer can decide whether a job runs on an on-prem SLURM
cluster, a different SSH host, or a cloud rental instance based on GPU
requirements, data locality, queue depth, and cost. SLURM only knows about its
own cluster.

### Notifications
Slack (and future channels) on job completion/failure. SLURM has `--mail-type`
but only for email.

### Job plans (YAML workflows)
`weft plan run workflow.yaml` with parallel/series blocks, submitting each step
as a SLURM job. SLURM has no native equivalent (users typically use external
workflow managers).

### Unified view across backends
A single `weft job list` shows jobs from queue-runner hosts, SLURM hosts, and
cloud instances together. SLURM only knows about its own cluster.

## What Weft Features Are Absent or Changed for SLURM Hosts

| Feature | Why absent/different |
|---|---|
| **Queue runner deployment** | SLURM has its own scheduler; no need for weft-agent on compute nodes |
| **Tmux session management** | SLURM manages job processes via cgroups, not tmux |
| **Source sync** | SLURM clusters typically use a shared filesystem (NFS/Lustre); weft's rsync-based sync may not be needed or desired |
| **CPU allotment / concurrency control** | SLURM handles this via partitions, QoS, and cgroup enforcement |
| **Pause/resume** | SLURM supports `scontrol hold/release` but the semantics differ from weft's SIGSTOP-based pause. Needs mapping. |
| **Cloud GPU bursting** | Not applicable — SLURM manages a fixed cluster. (A hybrid where overflow goes to Vast.ai is conceivable but out of scope.) |
| **Exclusive / benchmark tags** | SLURM has `--exclusive` natively; benchmark isolation would use a dedicated partition or QoS |
| **Data locality / placement scoring** | SLURM handles placement; weft's `--input hf:<model>` hints don't map to SLURM concepts (though `--constraint` could be used for node features) |
| **Host discovery (`weft host discover`)** | SLURM nodes are managed by the cluster; `sinfo` replaces host probing |

## Open Questions

1. **Partition selection**: Should weft auto-select partitions, or require the
   user to specify via config/flag? Auto-selection needs `sinfo` parsing and
   heuristics. Explicit selection is simpler but less ergonomic.

2. **Job script vs --wrap**: The current backend uses `sbatch --wrap`. For
   complex jobs (multi-line scripts, module loads), should weft generate and
   upload a job script instead?

3. **SLURM accounting integration**: Should weft import historical jobs from
   `sacct` into its local DB, or only track jobs it submitted? Importing gives a
   complete view but complicates identity (which jobs are "mine" vs submitted by
   other tools?).

4. **Multi-node jobs**: Weft currently assumes one job = one host. SLURM
   supports `--nodes=N` for MPI jobs. Should weft support this, or declare it
   out of scope?

5. **Environment modules**: SLURM clusters often use `module load` for software
   environments. Should weft support a `modules:` config key that prepends
   `module load X Y Z` to job scripts?
