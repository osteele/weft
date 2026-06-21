# Comparison to SkyPilot

This document compares weft to [SkyPilot](https://github.com/skypilot-org/skypilot),
a multi-cloud/Kubernetes/Slurm GPU job scheduler with cost optimization.

## TL;DR

Both weft and SkyPilot schedule GPU jobs with cost optimization, but they solve
different problems. **SkyPilot** optimizes cloud resource selection (finding the
cheapest instance across 20+ clouds plus Kubernetes and Slurm) and manages the
job lifecycle on durable, managed infrastructure. **Weft** manages a persistent
on-prem GPU cluster with cloud burst for overflow, using prediction-driven
placement that integrates data locality, runtime estimation, and cost modeling.

Many of the per-feature differences below are not independent design choices.
They follow from one root decision: **weft is built around the Vast.ai
marketplace** (cheapest GPUs, no durable volumes, stop/resume bidding
interruption, fixed instance disk), while **SkyPilot is built around
managed clouds and clusters** (durable object storage and volumes, vanish-style
spot preemption, elastic storage). Read the [Substrate](#substrate-shapes-the-design)
section first; the rest reads as consequences of it.

Use **weft** when:
- You have on-prem GPU machines and want automatic placement across them
- You need hybrid local + cloud scheduling with cost-aware placement
- You want to ride the cheapest GPU marketplace (Vast.ai) and manage the
  resulting reliability variance with a survival model
- Your jobs recur and benefit from learned runtime prediction
- You want ML-artifact-aware data locality (HF model caches per host)

Use **SkyPilot** when:
- Your compute is cloud- or cluster-based (20+ clouds, Kubernetes, Slurm)
- You want managed spot with automatic preemption recovery and checkpoint resume
- You need cloud resource selection and broad provider breadth
- You don't manage on-prem hardware

## Substrate Shapes the Design

SkyPilot targets **managed infrastructure**: hyperscaler clouds (AWS/GCP/Azure),
Kubernetes, and Slurm, where durable object storage, attachable/resizable
volumes, and a well-defined spot-preemption contract are available primitives.
Weft targets the **Vast.ai marketplace** (and, secondarily, RunPod), which trades
those primitives away for substantially lower GPU prices. The substrate differs
on three axes that drive almost every downstream difference:

| SkyPilot's substrate assumes | Vast.ai gives weft | Consequence in weft |
|---|---|---|
| Durable bucket mounts / K8s persistent volumes for checkpoints | Local-only volumes that can't move between machines; container disk dies with the instance | Weft builds its **own** durable layer (Cloudflare R2) and streams `output/` to it during the run, instead of mounting a durable volume |
| Spot preemption = instance vanishes (with warning) → relaunch elsewhere | Interruptible = same-machine **stop/resume bidding**; outbid pauses the instance, disk retained, resumes when you re-win the bid | Weft models **pause → resume on the same instance**, not cross-region failover (the correct model for this substrate) |
| Storage is elastic; resize or attach more freely | Instance disk is **fixed at creation and not resizable**; overrun is fatal | Weft's heavy **disk estimation** (`--input hf:`, HF API size queries) is survival, not gold-plating |
| Reliability-first, priced accordingly | Cheapest marketplace, **variable per-host reliability** | Weft adds a **survival model** (Beta-Binomial risk per provider/SKU/region) to manage the reliability it's trading for price |

Weft is optimized for a cheaper, less-durable
substrate, and compensates with external durability (R2) and risk modeling rather
than leaning on durable cloud storage and a managed-failover controller.

(Vast.ai references: [Rental Types](https://vast.ai/article/Rental-Types),
[Volumes](https://docs.vast.ai/documentation/instances/storage/volumes),
[Storage Types](https://docs.vast.ai/documentation/instances/storage/types),
[Instances FAQ](https://docs.vast.ai/documentation/reference/faq/instances).)

## Why Weft Isn't Built on SkyPilot

SkyPilot is the most similar system in spirit. But building on it doesn't work:

**Language mismatch.** Weft is Go (Bubble Tea TUI, Cobra CLI, embedded agent
binary, native SSH); SkyPilot is Python. The compiled edge agent with autonomous
local scheduling has no equivalent in SkyPilot's architecture.

**Different abstraction level.** SkyPilot optimizes *resource selection*:
the cheapest instance across 20+ clouds, Kubernetes, and Slurm. It doesn't manage a
fixed on-prem cluster with persistent state (per-host HF model caches, historical
job data for prediction). Adding on-prem cluster management with edge agents would
fight SkyPilot's architecture.

**Different scheduling contract.** SkyPilot's
[policy engine](https://docs.skypilot.co/en/latest/reference/config.html) allows
custom placement logic but assumes SkyPilot owns the job lifecycle. Weft's edge
agents own execution autonomy. The scheduling contract differs; it is not a
plugin point.

**Substrate mismatch.** SkyPilot's managed-spot and checkpointing story assumes
durable bucket/volume mounts and vanish-style preemption. Weft's substrate
(Vast.ai) offers neither, so SkyPilot's most valuable cloud machinery doesn't map
cleanly onto it (see [Substrate](#substrate-shapes-the-design)).

**Novel parts don't need it.** The quantile-regression prediction, Monte Carlo
placement, survival modeling, and ML-artifact locality tracking are internal
scoring algorithms that don't benefit from SkyPilot's provider integrations.

## Feature Comparison

| Capability | Weft | SkyPilot |
|---|---|---|
| **Runtime prediction** | LightGBM quantile regression + roofline fallback | None |
| **Prediction uncertainty** | Per-quantile (p10/p50/p90) with Monte Carlo simulation | — |
| **Data locality** | Per-host HF model/dataset cache tracking | Cloud region colocation |
| **Transfer cost in placement** | Model size × bandwidth penalty | — |
| **Hybrid local + cloud** | Queue wait vs. setup + download + rental cost | Cloud-to-cloud only |
| **Heterogeneous workloads** | Training, eval, benchmarking, inference | General cloud jobs |
| **Heterogeneous GPU support** | Per-host GPU specs + roofline performance scaling | Instance type selection |
| **Cold-start handling** | Roofline estimates from hardware specs | — |
| **Compute substrate** | On-prem hosts + Vast.ai, RunPod (direct API) | 20+ clouds + Kubernetes + Slurm |
| **Spot / interruptible request** | Opt-in (`interruptible` tag); on-demand by default | `--use-spot`; can mix spot + on-demand (`any_of`) |
| **Interruption model** | Detects same-instance pause→resume (Vast.ai bidding) | Vanish-style preemption, relaunch elsewhere |
| **Preemption recovery** | Auto-relaunch on retryable termination (survival-gated, runaway breaker); fresh instance, job restarts | Managed-jobs controller: `RECOVERING` state, cross-region/cloud relaunch; also recovers node crashes, GPU failures, NCCL timeouts |
| **Reliability modeling** | Beta-Binomial survival model per provider/SKU/region | None (operational failover, no risk priors) |
| **Failure-tolerant session** | Grace period (R2 control messages) keeps a failed instance alive for resubmit/extend/release | None (jobs are restarted, not held) |
| **Durable storage** | External object store (R2); streams `output/` during run + drain-on-teardown | Cloud bucket mounts (`MOUNT`/`MOUNT_CACHED`) + K8s persistent volumes |
| **Checkpoint resume** | Checkpoints survive (R2 + retained Vast disk) but **not auto-staged to the resumed job** | App-driven, scaffolded by stable `$SKYPILOT_TASK_ID` + persistent `/checkpoint` mount |
| **Idle autostop** | None (one-job-per-ephemeral-instance; teardown on completion + bounded drain) | `autostop: idle_minutes` for interactive clusters; managed jobs self-clean |
| **Data pre-staging** | Donor/seed O(log N) fan-out (cloud) + host-to-host rsync (on-prem) | Per-instance download; `file_mounts` bucket mounts (7× faster mounting in v0.12) |
| **Artifact lineage** | Typed inputs/outputs, manifest store, producer→consumer edges | Storage mounts + managed pipelines (sequential multi-task) |
| **Operator UI** | Terminal-native TUIs (`instance`/`campaign watch`, dashboard); serverless | Web dashboard via API server; optional Prometheus/Grafana metrics |
| **Lifecycle hooks** | Built-in Slack notify (deployed at host setup) + DB lifecycle events | Generic `event_callback` (per-task state change) + autostop hooks |
| **Scripting / agent CLI** | First-class `--wait` / `watch` / `--json`, autopilot exit codes | `--output json`, `--async` + API request IDs, Python SDK; wrap `watch(1)` to poll |

## Key Architectural Differences

### Data Locality

Weft tracks ML-specific artifacts (HuggingFace models, datasets, Python virtual
environments) as cacheable assets with known sizes (queried from HF API). The
placement scorer rewards hosts that already have required inputs cached and
computes transfer time penalties from actual model sizes and measured bandwidth.

SkyPilot colocates jobs with cloud storage regions but doesn't track individual
model cache state per host. This is a reasonable design for ephemeral cloud
instances where caches don't persist, but weft's on-prem hosts maintain
persistent HF caches that make locality a meaningful placement factor.

### Hybrid Local + Cloud Placement

Weft's cost estimator compares local execution (free but queued behind other
jobs) against ephemeral cloud instances (setup overhead + source sync + model
download + rental rate × predicted runtime). This bridges a persistent on-prem
cluster and dynamically-selected cloud hosts with a unified cost model.

SkyPilot optimizes *cloud-to-cloud*, the cheapest region and instance type, but
doesn't model queue wait on a local cluster. It assumes all compute is cloud- or
cluster-based.

### Interruption and Recovery

**SkyPilot** treats spot preemption like any other failure: the managed-jobs
controller continuously monitors the cluster, and on preemption (or node crash,
GPU failure, NCCL timeout) the job enters a `RECOVERING` state and is relaunched
on newly-found capacity across regions and clouds. Application errors can also be
retried a configurable number of times. The model fits hyperscaler spot, where a
preempted instance disappears.

**Weft** is built for Vast.ai's interruptible *bidding* model, where being outbid
**stops** (pauses) the instance rather than destroying it, and the container's
local disk is retained while stopped. Accordingly, weft's reconciler
(`internal/campaign/instance_check.go`) distinguishes:

- `ActionPause`: the provider stopped the instance (Vast.ai reports interruptible
  preemption as `offline`); weft marks the launch `paused`, but only for statuses
  that can plausibly resume (`isRecoverablePausedProviderStatus`).
- `ActionResume`: a previously paused launch is running again at the provider;
  weft returns the launch to `running`. Weft detects a slept instance
  waking on the **same machine**.
- After `stalePauseTimeout` with no resume, weft gives up: it destroys the
  instance, marks the attempt preempted, and resets the jobs so they can relaunch
  elsewhere.

When relaunch is required, the autopilot/watch loop auto-relaunches on retryable
termination (`db.IsRetryableTermination`, `internal/ui/terminal/watch_tui_handlers.go`),
gated by a runaway breaker (`internal/campaign/relaunch.go`) and the survival
model. Separately, the **grace period** (R2-based control messages) keeps a
*failed* instance alive for a window so a job can be resubmitted, extended, or
released. This differs from preemption recovery.

Weft does not
**transport a checkpointed `output/` directory to a resumption process**. On the same instance, the on-disk checkpoints survive the
pause (Vast retains stopped-container disk), but weft doesn't hand them to the
restarted job; resume-from-checkpoint is left to the application. On relaunch to a
*different* machine, the local disk (and any Vast volume, which is machine-local)
is gone; only what was streamed to R2 survives, and weft doesn't auto-stage that
back as a checkpoint input to the new job.

### Durable Storage and Checkpointing

SkyPilot's checkpoint story leans on durable storage the cloud already provides:
a persistent `/checkpoint` bucket mount (`MOUNT_CACHED`) or a Kubernetes
persistent volume, plus a stable `$SKYPILOT_TASK_ID` that survives recoveries, so
an app that "loads latest checkpoint on every startup" resumes seamlessly. Vast.ai
offers no equivalent: volumes are local-only and non-portable, and instance disk
dies with the instance. So weft supplies durability externally: it streams
outputs/checkpoints written to `output/`/`outputs/` to R2 *during* the run
(`internal/runner/single.go`) and flushes the remainder during a bounded drain on
teardown. The result is comparable *durability* but a different *resume* contract:
SkyPilot makes app-side resume automatic across recoveries; weft makes checkpoints
durable but leaves the resume wiring (re-staging the last checkpoint into a new
attempt) unbuilt. Closing that gap (staging an R2-streamed `output/` back as a
`checkpoint:` input on relaunch) is the highest-value reliability improvement for
weft on this substrate.

### Artifact Management and Data Pipeline

The two systems model data movement at different altitudes.

**SkyPilot** exposes a storage-mount abstraction: `file_mounts` over cloud buckets
(S3/GCS/R2/Azure) with `MOUNT`, `MOUNT_CACHED`, and `COPY` modes (v0.12 added ~7×
faster data mounting), and managed pipelines that chain multiple tasks
sequentially. The cloud bucket *is* the durable layer; SkyPilot doesn't model
individual ML artifacts, per-host caches, or output lineage.

**Weft** models data as typed, sized assets and tracks lineage end to end:

- **Typed inputs** (`internal/dataloc`): `hf:` / `hf-dataset:` (HuggingFace, sizes
  from the HF API), `checkpoint:` / `corpus:` / `asset:` (named host-local
  assets), and `job-output:` (a prior job's outputs). Declarations feed disk
  estimation, download-time modeling, and placement locality.
- **Pre-staging**: donor/seed O(log N) fan-out across cloud instances and
  host-to-host rsync on-prem, rather than each instance pulling from source.
- **Convention-based output collection**: anything written to `output/`/`outputs/`
  is tracked and synced to R2 automatically.
- **Artifact store** (`internal/artifacts`, `cmd/artifact.go`): a per-job manifest
  (local `~/.config/weft/artifacts`, durable in R2) addressable by token
  (`weft artifact list/get/cat/sync/add`), with a sync orchestrator
  (`internal/syncorch`) that reconciles results from both cloud (R2) and on-prem
  hosts.
- **Producer → consumer pipelines**: a job declares `--output`; a downstream job
  consumes it with `--input job-output:` (pull via R2) or `--input checkpoint:`
  (placement hint pinning the consumer to a host that already has the bytes).

So weft's data layer is richer for *ML artifacts, hybrid on-prem caches, and
output lineage*; SkyPilot's is a cleaner *general storage-mount* abstraction
backed by genuinely durable cloud buckets. Weft has to *be* the durable artifact
layer because Vast.ai doesn't provide one to mount.

### Cloud Burst

Both systems can provision cloud GPU instances, but from different starting
points:

- **SkyPilot**: Infrastructure-native. Provisions across 20+ clouds plus
  Kubernetes and Slurm with managed spot, failover, and auto-recovery. This is its
  core competency.

- **Weft**: On-prem-first. Cloud burst is overflow for when local hosts are busy
  or lack required GPU capabilities. Weft talks to Vast.ai and RunPod directly
  with disk estimation (per-model download sizes from the HF API), survival-gated
  offer selection, and cost modeling (local queue wait vs. cloud rental) that
  SkyPilot doesn't replicate.

### Operator and Agent Surface

Both systems are driven by a single operator and, increasingly, by coding
agents, but they expose that surface differently.

**Weft** is terminal-native and serverless. Monitoring is a set of Bubble Tea
TUIs (`weft instance watch`, `weft campaign watch`, the dashboard, the grouped
jobs list), and the autopilot runs inside the TUI rather than as a separate
service. Scripting and agent control are built into the CLI: `weft status
--wait` blocks until jobs finish and exits non-zero if any fail, `weft instance
watch` streams until every instance reaches a terminal state, `--json` is
available across query commands, and `weft autopilot status --quiet` returns
distinct exit codes (idle / running / stale / paused). Lifecycle notifications
are a built-in Slack integration deployed during `weft host setup`, backed by
lifecycle events recorded in the DB.

**SkyPilot** routes the same needs through a client-server architecture. Its UI
is a web dashboard served by the API server, with optional Prometheus/Grafana
GPU metrics and W&B link detection, rather than an in-terminal TUI. Job-state
notifications use a generic `event_callback` script that runs on each task state
transition, plus autostop hooks that fire before a cluster stops; both can call
a webhook. For scripting, `sky jobs queue --output json` and `sky jobs logs
--follow` cover status and logs, `--async` with `sky api logs <request-id>`
covers fire-and-forget submission, and a Python SDK exposes the API server
directly. There is no first-class watch flag, so the docs wrap the CLI in the
Unix `watch` utility.

The split tracks the rest of the architecture: weft favors a single compiled
binary with agent-oriented exit codes and JSON in the CLI; SkyPilot favors a
server with a browser dashboard and a Python SDK. SkyPilot also ships an official
Agent Skill that teaches coding agents its CLI, where weft shapes the commands
themselves (blocking waits, success-coded exits) for agent use.

## Where SkyPilot Could Complement Weft

SkyPilot could serve as a cloud-provider abstraction layer alongside weft's direct
Vast.ai/RunPod calls in `internal/campaign/`, giving weft access to managed clouds,
Kubernetes, and Slurm, plus SkyPilot's managed-spot recovery. But the value is
asymmetric on weft's current substrate: Vast.ai is typically much cheaper than the
managed clouds SkyPilot targets, and weft already handles cloud provisioning
directly because it needs tight integration with disk estimation (required disk
from declared `--input hf:<model>` dependencies), per-model download modeling, the
survival model, and local-vs-cloud cost comparison. SkyPilot provides none of
these. Embedding SkyPilot *inside* weft also collides with weft's
lifecycle ownership (grace period, agent-over-SSH, R2 result collection,
per-instance cost) and adds a Go↔Python boundary; see
`docs/planning/skypilot-backend.md` for the open questions. SkyPilot is most useful
to a weft user *as a standalone tool* for one-off runs on clouds weft doesn't
reach, rather than as an embedded backend.
