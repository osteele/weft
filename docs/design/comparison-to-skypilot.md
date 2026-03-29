# Comparison to SkyPilot

This document compares weft to [SkyPilot](https://github.com/skypilot-org/skypilot), a multi-cloud GPU job scheduler with cost optimization.

## TL;DR

Both weft and SkyPilot schedule GPU jobs with cost optimization, but they solve
different problems. **SkyPilot** optimizes cloud resource selection — finding the
cheapest instance type across 20+ cloud providers. **Weft** manages a persistent
on-prem GPU cluster with cloud burst for overflow, using prediction-driven
placement that integrates data locality, runtime estimation, and cost modeling.

Use **weft** when:
- You have on-prem GPU machines and want automatic placement across them
- You need hybrid local + cloud scheduling with cost-aware placement
- Your jobs recur and benefit from learned runtime prediction
- You want ML-artifact-aware data locality (HF model caches per host)

Use **SkyPilot** when:
- Your compute is entirely cloud-based across multiple providers
- You want automated spot instance management and preemption recovery
- You need cloud resource selection across 20+ providers
- You don't manage on-prem hardware

## Why Weft Isn't Built on SkyPilot

SkyPilot is the most similar system in spirit. But building on it doesn't work:

**Language mismatch.** Weft is Go (Bubble Tea TUI, Cobra CLI, embedded agent
binary, native SSH); SkyPilot is Python. The compiled edge agent with autonomous
local scheduling has no equivalent in SkyPilot's architecture.

**Different abstraction level.** SkyPilot optimizes *cloud resource selection* —
cheapest instance type across 20+ providers. It doesn't manage a fixed on-prem
cluster with persistent state (per-host HF model caches, historical job data for
prediction). Adding on-prem cluster management with edge agents would fight
SkyPilot's architecture.

**Different scheduling contract.** SkyPilot's
[policy engine](https://docs.skypilot.co/en/latest/reference/config.html) allows
custom placement logic but assumes SkyPilot owns the job lifecycle. Weft's edge
agents own execution autonomy — that's a different scheduling contract, not a
plugin.

**Novel parts don't need it.** The quantile-regression prediction, Monte Carlo
placement, and ML-artifact locality tracking are internal scoring algorithms that
don't benefit from SkyPilot's cloud provider integrations.

## Feature Comparison

| Capability | Weft | SkyPilot |
|---|---|---|
| **Runtime prediction** | LightGBM quantile regression + roofline fallback | None |
| **Prediction uncertainty** | Per-quantile (p10/p50/p90) with Monte Carlo simulation | — |
| **Data locality** | Per-host HF model/dataset cache tracking | Cloud region colocation |
| **Transfer cost in placement** | Model size x bandwidth penalty | — |
| **Hybrid local + cloud** | Queue wait vs. setup + download + rental cost | Cloud-to-cloud only |
| **Heterogeneous workloads** | Training, eval, benchmarking, inference | General cloud jobs |
| **Heterogeneous GPU support** | Per-host GPU specs + roofline performance scaling | Instance type selection |
| **Cold-start handling** | Roofline estimates from hardware specs | — |
| **Data pre-staging** | Donor/seed O(log N) fan-out (cloud) + host-to-host rsync (on-prem) | Per-instance download from source |
| **Instance failure recovery** | Grace period with R2-based control messages | Managed spot with auto-recovery |
| **Multi-cloud providers** | Vast.ai, RunPod (direct API) | 20+ providers |

## Key Architectural Differences

### Data Locality

Weft tracks ML-specific artifacts — HuggingFace models, datasets, Python virtual
environments — as cacheable assets with known sizes (queried from HF API). The
placement scorer rewards hosts that already have required inputs cached and
computes transfer time penalties from actual model sizes and measured bandwidth.

SkyPilot colocates jobs with cloud storage regions but doesn't track individual
model cache state per host. This is a reasonable design for ephemeral cloud
instances where caches don't persist, but weft's on-prem hosts maintain
persistent HF caches that make locality a meaningful placement factor.

### Hybrid Local + Cloud Placement

Weft's cost estimator compares local execution (free but queued behind other
jobs) against ephemeral cloud instances (setup overhead + source sync + model
download + rental rate x predicted runtime). This bridges a persistent on-prem
cluster and dynamically-selected cloud hosts with a unified cost model.

SkyPilot optimizes *cloud-to-cloud* — cheapest region and instance type — but
doesn't model queue wait on a local cluster. It assumes all compute is cloud-based.

### Cloud Burst

Both systems can provision cloud GPU instances, but from different starting
points:

- **SkyPilot**: Cloud-native. Provisions across 20+ providers with sophisticated
  spot instance management, failover, and auto-recovery. This is its core
  competency.

- **Weft**: On-prem-first. Cloud burst is overflow for when local hosts are busy
  or lack required GPU capabilities. Weft talks to Vast.ai and RunPod directly
  with disk estimation (per-model download sizes from HF API) and cost modeling
  (local queue wait vs. cloud rental) that SkyPilot doesn't replicate.

## Where SkyPilot Could Complement Weft

SkyPilot could serve as a cloud provider abstraction layer, replacing weft's
direct Vast.ai and RunPod API calls in `internal/campaign/`. This would give weft
access to SkyPilot's 20+ provider integrations and spot instance management.

Currently weft handles cloud provisioning directly because it needs tight
integration with disk estimation (computing required disk from declared
`--input hf:<model>` dependencies), per-model download time modeling, and
local-vs-cloud cost comparison — none of which SkyPilot provides.
