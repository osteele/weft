# SkyPilot Backend Integration Plan

Status: **Plan** — describes intended design, not current implementation.

## Context

Weft currently provisions cloud GPU instances via direct Vast.ai and RunPod API
calls in `internal/campaign/`. SkyPilot is a multi-cloud GPU scheduler with
integrations for 20+ cloud providers, spot instance management, and automatic
failover.

This plan describes how weft could use SkyPilot as a cloud provider abstraction
layer — replacing or supplementing the direct Vast.ai/RunPod calls — while
retaining weft's own placement scoring, cost modeling, and lifecycle management.

See also: `docs/design/comparison-to-skypilot.md` for architectural differences.

## Design Principle

SkyPilot would serve as a **provider facade** — weft decides *whether* to burst
to cloud and *which GPU class* to request; SkyPilot decides *which provider and
region* to provision from. Weft retains ownership of placement scoring (local
queue wait vs. cloud cost), data locality tracking, and the job lifecycle.

## What SkyPilot Would Provide

### Multi-provider access
Currently weft talks to Vast.ai and RunPod directly. SkyPilot adds AWS, GCP,
Azure, Lambda, CoreWeave, and 15+ others through a single interface. Weft would
gain access to these providers without writing per-provider API integrations.

### Spot instance management
SkyPilot handles spot/preemptible instance lifecycle: automatic failover to
on-demand when spot is preempted, cross-region retry, and managed checkpointing.
Weft's current grace-period system (R2-based control messages) handles
failure-tolerant sessions but doesn't manage spot-specific retry logic.

### Instance type selection within a provider
Given a GPU requirement like "A100 80GB", SkyPilot finds the cheapest matching
instance type across regions and availability zones. Weft currently does this
manually for Vast.ai (searching offers by GPU spec and sorting by price).

### Cloud credential management
SkyPilot handles multi-cloud authentication. Weft currently requires users to
configure Vast.ai API keys and R2 credentials separately.

## What Weft Retains

### Placement scoring (local vs. cloud)
SkyPilot doesn't model on-prem queue wait or local data locality. Weft's Monte
Carlo completion-time simulation compares running locally (free but queued) vs.
bursting to cloud (setup overhead + rental cost). This decision happens before
SkyPilot is involved.

### Data locality and transfer cost modeling
Weft tracks per-host HF model caches and computes transfer time penalties from
measured bandwidth. SkyPilot colocates with cloud storage regions but doesn't
track individual model cache state. Weft's `--input hf:<model>` declarations
feed disk estimation and download time modeling that SkyPilot doesn't replicate.

### Disk estimation
Weft queries the HuggingFace API for model sizes, estimates UV dependency
install sizes from cached manifests, and computes total disk requirements. This
determines minimum instance disk size — information weft would pass to SkyPilot
as a constraint.

### Job lifecycle and result collection
Weft's agent wrapper, R2-based result collection, grace period system, and
local SQLite job tracking remain unchanged. SkyPilot provisions the instance;
weft deploys its agent and manages the job.

### Survival modeling and risk-adjusted cost
Weft's Beta-Binomial survival model estimates preemption risk per GPU
family/price bucket. SkyPilot handles spot failover operationally but doesn't
expose survival probabilities for cost optimization.

### On-prem scheduling
SkyPilot is cloud-only. Weft's queue runner, SLURM backend, and on-prem
placement continue to operate independently.

## Integration Architecture

```
User: weft run --gpu a100>=80GB "train.py"
          │
          ▼
  Weft placement scorer
  ├── On-prem hosts (queue runner / SLURM)
  ├── Direct cloud (Vast.ai / RunPod)  ← existing path
  └── SkyPilot cloud (AWS / GCP / Azure / ...)  ← new path
          │
          ▼
  SkyPilot: cheapest instance across providers
          │
          ▼
  Weft agent deployed via SSH → job runs → results via R2
```

SkyPilot would be a new backend alongside `queue_runner`, `slurm`, and `vastai`,
selected when the user configures SkyPilot-managed providers or when weft's
placement scorer determines cloud burst is optimal and SkyPilot offers a cheaper
option than direct Vast.ai/RunPod.

## What Weft Features Are Absent or Changed with SkyPilot

| Feature | Status |
|---|---|
| **Grace period (R2 control messages)** | Unclear — SkyPilot manages instance lifecycle differently; grace-wait polling may conflict with SkyPilot's own retry/failover |
| **Instance SSH access (`weft instance ssh`)** | SkyPilot provides `sky ssh`; weft would need to bridge or defer to it |
| **Direct provider queries** | Weft currently queries Vast.ai offers for pricing; with SkyPilot this is delegated |
| **Per-instance cost tracking** | SkyPilot tracks cost at the cluster level; weft's per-instance `cost_per_hour_cents` would need a different data source |
| **Donor/seed copy orchestration** | SkyPilot doesn't support phone-tree data fan-out; this would still use weft's direct SSH |

## Open Questions

1. **Lifecycle ownership**: Who owns the instance lifecycle? If SkyPilot manages
   spot failover, weft's grace period and R2 control messages may conflict. One
   option: use SkyPilot for provisioning only, then detach and let weft manage
   the running instance.

2. **Language boundary**: SkyPilot is Python; weft is Go. Integration options:
   - Shell out to `sky` CLI (simple but slow, limited error handling)
   - Use SkyPilot's REST API if/when available
   - Write a thin Python sidecar that wraps SkyPilot's Python API

3. **Cost data flow**: Weft's placement scorer needs real-time pricing to
   compare local-vs-cloud. SkyPilot's `sky show-gpus` provides catalog pricing
   but not the same granularity as Vast.ai's offer-level pricing. How to unify?

4. **Checkpoint integration**: SkyPilot's managed spot includes automatic
   checkpointing. Weft's jobs don't currently checkpoint. Should weft adopt
   SkyPilot's checkpointing protocol, or treat spot instances as unreliable and
   rely on weft's existing retry logic?

5. **When to use SkyPilot vs. direct providers**: Vast.ai and RunPod often have
   lower prices than major cloud providers for GPU workloads. SkyPilot adds
   breadth but may not improve cost for the GPU-heavy workloads weft typically
   runs. Should SkyPilot be a fallback when direct providers lack capacity?
