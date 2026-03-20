# Weft vs SkyPilot: Gap Analysis

A comparison of Weft and [SkyPilot](https://skypilot.readthedocs.io/) — two
tools for running AI/ML workloads on heterogeneous GPU infrastructure. Both
target the same problem space (making it easy to submit jobs to GPUs across
providers) but approach it from different angles.

## Summary

**SkyPilot** is a multi-cloud orchestration layer. It abstracts 20+ cloud
providers and Kubernetes behind a declarative YAML interface, provisions VMs
on-demand, and manages their lifecycle. Its sweet spot is teams that need to
burst across many clouds with cost optimization.

**Weft** is an SSH-first workload scheduler. It targets a hybrid fleet of
on-prem GPU machines plus spot cloud instances, with deep data-locality
awareness and a persistent job database. Its sweet spot is a single researcher
or small team managing a heterogeneous mix of owned and rented GPUs.

---

## Concept / Terminology Mapping

Where the same capability exists in both tools under different names or with
different granularity.

| Capability | SkyPilot | Weft |
|---|---|---|
| Unit of work | **Task** (YAML spec) | **Job** (CLI flags + command string) |
| Compute target | **Cluster** (provisioned VM group) | **Host** (SSH-reachable machine) |
| Cloud burst instance | **Cluster** (cloud-provisioned) | **Cloud Instance** (Vast.ai/RunPod) |
| Batch of cloud instances | *(clusters are independent)* | **Campaign** (group of instances launched together) |
| GPU request | `accelerators: A100:4` | `--gpu a100` / `--gpu nvidia>=40GB` |
| CPU request | `cpus: 16+` | *(not user-specified; auto from host inventory)* |
| Memory request | `memory: 64GB+` | *(not user-specified; predicted from history)* |
| Disk request | `disk_size: 256GB` | *(not user-specified)* |
| Spot instances | `use_spot: true` | Implicit — Vast.ai/RunPod are spot-like by nature |
| Job recovery on preemption | `job_recovery: FAILOVER` | **Grace period** (instance stays alive for resubmit/extend) |
| Autostop idle resources | `autostop: 10` (minutes) | Grace period timeout (default 5m, extendable) |
| Environment variables | `envs:` in YAML | `--env` flag; auto-loaded `.env` / `.envrc` files |
| Secrets | `secrets:` (redacted envs) | *(no separate secret type; passed via env)* |
| File sync to remote | `workdir:` / `file_mounts:` | Automatic rsync on job submission (convention-based) |
| Cloud storage mounts | `file_mounts: /data: s3://bucket` | *(not supported; uses rsync/R2 for result upload)* |
| Setup script | `setup:` block in YAML | *(no separate setup; user includes in command or uses Docker image)* |
| Run script | `run:` block in YAML | Command string (positional arg to `weft run`) |
| Multi-node jobs | `num_nodes: 4` | *(single-node only)* |
| Job queue on cluster | `sky queue` | `weft queue list` / `weft job list` |
| Cancel jobs | `sky cancel` | `weft kill` |
| View logs | `sky logs` | `weft log` (with `-f`, `--grep`, line ranges) |
| Model serving | **SkyServe** (`sky serve up`) | *(not supported)* |
| Autoscaling | Replica policies, scale-to-zero | *(not supported)* |
| Worker pools | **Pools** (`sky jobs pool apply`) | *(not supported; queue runner is closest analog)* |
| Dashboard | **SkyPilot Dashboard** (web) | `weft web` (read-only) + `weft tui` (interactive terminal) |
| Declarative job spec | YAML file | CLI flags (or YAML **job plans** for multi-step) |
| Job dependencies | *(via orchestrators: Airflow, etc.)* | `--after`, `--needs`/`--produces` (artifact-based) |
| Data locality | *(not built-in)* | First-class: `--input hf:model`, placement scoring, pre-staging |
| Resource prediction | *(not built-in)* | ML-based: duration, RSS, GPU memory from job history |
| Host discovery | `sky check` (cloud credentials) | `weft host discover` (SSH probe + YAML generation) |
| Infrastructure config | Cloud credentials + `~/.sky/config.yaml` | Host YAML files in `~/.config/weft/hosts/` |

---

## Features SkyPilot Has That Weft Lacks

### Multi-cloud provisioning (20+ providers)

SkyPilot can provision VMs on AWS, GCP, Azure, OCI, CoreWeave, Lambda, and 15+
others. Weft supports Vast.ai and RunPod as cloud providers; on-prem hosts are
managed manually via SSH.

**Impact**: High for teams needing cloud diversity. Low for teams with
established on-prem fleets plus occasional spot rentals.

### Kubernetes and Slurm integration

SkyPilot treats Kubernetes clusters and Slurm-managed HPC clusters as
first-class backends. Weft has no Kubernetes or Slurm integration (though the
codebase has a `SLURM_TEST_HOST` env var suggesting early exploration).

### Multi-node / distributed training

SkyPilot's `num_nodes` field provisions multi-VM clusters for distributed
training (DeepSpeed, PyTorch DDP, etc.) with inter-node networking (InfiniBand,
GPUDirect). Weft is single-node only.

### Model serving (SkyServe)

SkyPilot includes a full serving stack: replicas, autoscaling, load balancing,
health probes, rolling/blue-green updates, HTTPS. Weft has no serving
capability.

### Worker pools with autoscaling

SkyPilot pools maintain warm workers that scale between min/max based on queue
depth. Weft's queue runner is fixed to one host at a time.

### Cloud storage mounts

SkyPilot can mount S3, GCS, or R2 buckets directly into the job's filesystem.
Weft uses rsync for file transfer and R2 only for result upload from cloud
instances.

### Declarative YAML task specs

SkyPilot tasks are fully declarative: resources, setup, run, file mounts, envs,
and service config all in one YAML. Weft's primary interface is imperative CLI
flags; YAML job plans exist but are limited to multi-step orchestration.

### Spot instance recovery with checkpointing

SkyPilot's managed jobs automatically recover from spot preemptions by
re-provisioning on another cloud/region and resuming from checkpoints. Weft's
grace period keeps the instance alive for manual resubmit but doesn't
auto-migrate.

### SSO / RBAC / Workspaces

SkyPilot 0.10+ has enterprise features: SSO authentication, role-based access
control, and workspace isolation. Weft is single-user.

### Disk tier selection

SkyPilot lets users specify `disk_tier: high` for NVMe-class storage. Weft has
no disk performance selection.

### Network tier selection

SkyPilot's `network_tier: best` enables high-performance networking (GPUDirect,
InfiniBand). Weft has no network tier concept.

### Custom VM images

SkyPilot supports `image_id` to launch from custom AMIs, GCP images, or Docker
images. Weft uses `default_image` for cloud instances globally but doesn't
support per-job images for on-prem hosts.

### Region/zone affinity and failover

SkyPilot's `any_of` and `ordered` resource blocks let users specify fallback
regions or instance types with automatic failover. Weft's placement is
host-level, not region-level.

---

## Features Weft Has That SkyPilot Lacks

### SSH-first on-prem management

Weft discovers, inventories, and manages bare-metal machines via SSH without
requiring any agent framework, container runtime, or cluster manager. SkyPilot
can use "existing machines" via its SSH backend but it's a secondary path.

### Data locality awareness

Weft tracks which HuggingFace models and datasets are cached on which hosts,
scores placement accordingly, and pre-stages data via rsync. SkyPilot has no
built-in data locality tracking.

### ML-based job prediction

Weft trains estimator models on historical job data to predict duration, peak
memory, and GPU memory for new jobs. This feeds into placement scoring. SkyPilot
has no resource prediction.

### Persistent job database

Weft maintains a local SQLite database of all jobs ever run, with full metadata,
tags, and status history. SkyPilot's state is cluster-scoped.

### Job tagging and filtering

Weft supports arbitrary tags (`exclusive`, `benchmark`, `rental`, etc.) that
affect scheduling behavior. SkyPilot uses instance labels but these don't
influence scheduling.

### Artifact-based dependencies

`--produces` and `--needs` flags let jobs declare file-path dependencies,
creating DAGs without hard-coding job IDs. SkyPilot delegates pipeline
orchestration to external tools (Airflow, Prefect).

### Interactive TUI

Weft has a rich terminal UI (Bubble Tea) with split-screen job/host views, live
log streaming, GPU utilization monitoring, and keyboard-driven operations.
SkyPilot has a web dashboard but no terminal UI.

### Pause/resume jobs

`weft pause` / `weft resume` sends SIGSTOP/SIGCONT to running processes.
SkyPilot can stop/start clusters but not pause individual jobs.

### CPU-capped queue scheduling

Weft's queue runner maintains per-job CPU allotments to keep total host
utilization under a cap. SkyPilot doesn't manage per-job CPU throttling.

### Transfer bandwidth learning

Weft observes rsync transfers to learn per-(source, dest) bandwidth, feeding
this into placement cost estimation. SkyPilot doesn't model transfer costs.

### Operation logging (oplog)

Weft writes structured JSON logs of all operations (placement decisions, state
changes, sync events) to a rotating log file. Useful for debugging scheduling
decisions.

### Auto-remediation

Weft can automatically diagnose failed jobs from log patterns (missing HF
models, Python import errors) and optionally invoke a coding agent to attempt
fixes. SkyPilot has no auto-remediation.

### Slack notifications

Built-in Slack webhook integration for job completion/failure notifications with
configurable verbosity and duration thresholds.

### Grace period with interactive control

When a cloud instance's job fails, the instance enters a grace period where the
user can resubmit a fixed job, extend the deadline, or release — all without
re-provisioning. SkyPilot's recovery is fully automatic (reprovision elsewhere)
with no interactive control.

---

## Architectural Differences

| Dimension | SkyPilot | Weft |
|---|---|---|
| Control plane | API server (optional, for enterprise) | Coordinator daemon on studio (optional) |
| State storage | Per-cluster state; API server DB for managed jobs | Single SQLite DB for all jobs, hosts, campaigns |
| Provisioning | Cloud APIs (create/destroy VMs) | SSH to existing hosts; Vast.ai/RunPod APIs for cloud |
| Agent model | Cloud-init / setup scripts | Go binary (`weft-agent`) deployed via scp |
| Job isolation | VM-level (one cluster per task by default) | Process-level (tmux sessions on shared hosts) |
| Config format | YAML (declarative) | CLI flags + TOML config + host YAML |
| Auth model | Cloud IAM + optional SSO/RBAC | SSH keys |
| Primary user | Team of ML engineers | Solo researcher / small team |

---

## Overlap and Migration Considerations

If evaluating Weft alongside SkyPilot:

1. **Use SkyPilot when** you need multi-cloud bursting across many providers,
   distributed multi-node training, or model serving with autoscaling.

2. **Use Weft when** you have a stable on-prem GPU fleet, care about data
   locality for large models, want a persistent job history across all hosts,
   or need fine-grained control over mixed owned/rented infrastructure.

3. **They could complement each other**: SkyPilot for cloud provisioning, Weft
   for on-prem scheduling — though integrating them would require bridging
   SkyPilot's cluster model with Weft's host model.

---

*Last updated: 2026-03-20*
