# Placement

Weft placement decides where a job should run when you omit the host:

```bash
weft run --gpu a100 --input hf:meta-llama/Llama-3-8B 'python train.py'
```

The placement engine uses hard constraints first, then ranks eligible
destinations by estimated completion time, data locality, queue state, and the
active autopilot strategy.

## When Placement Engages

Automatic placement engages when a job has no explicit host. These forms use
placement:

```bash
weft run 'python train.py'
weft run --gpu ampere+ 'python train.py'
weft run --tag rental --gpu a100 'python train.py'
```

Hostless `weft run` records the job locally first. If recent host-state
observations are available, it can select an on-prem host from the database
without probing SSH; otherwise it reports that placement is pending. The
daemon/autopilot performs live host probes, reuses existing rental instances,
or launches new instances after submission.

These forms bypass placement and target the named host directly:

```bash
weft run cool30 'python train.py'
weft run --host cool30 'python train.py'
```

When placement is active, Weft considers destinations in this order:

1. Inventory hosts, using current GPU utilization, queue depth, memory, and
   data locality.
2. Existing cloud instances, with grace-period instances preferred because
   they can be reused before more rental time is purchased.
3. New cloud instances, scored by the active strategy profile and provider
   offers.

If no inventory host fits, a rental-eligible job can remain queued as
unplaced until the autopilot or a manual launch assigns it to an instance.

## Steering Placement

Use `--gpu`, `--gpu-class`, and `--gpu-mem` to describe required hardware:

```bash
weft run --gpu nvidia 'python train.py'
weft run --gpu 'nvidia>=24GB' 'python train.py'
weft run --gpu ampere+ 'python train.py'
weft run --gpu-class a100 --gpu-mem 60 'python train.py'
weft run --gpu h100 --gpus 4 --gpu-mem 80 'python train.py'
```

GPU class matching supports exact models (`a100`, `rtx3090`, `gh200`),
generations (`ampere`, `hopper`), minimum generations (`ampere+`), and
families (`nvidia`, `apple`). Bare RTX model numbers such as `3090` are
normalized to `rtx3090`. Use `gh200` when the job needs a Grace Hopper
superchip specifically; `hopper` remains a broad generation constraint that
can also match H100 and H200.

The `>=NGB` form on `--gpu` is a hardware capacity floor. For example,
`--gpu a100>=80GB` matches 80GB A100 offers exactly; it does not add the
`+2GB` workload headroom used by separate `--gpu-mem` reservations.

The separate `--gpu-class` + `--gpu-mem` form **also skips the +2GB headroom
when `--gpu-mem` matches the model's actual hardware ceiling**. So
`--gpu-class a100 --gpu-mem 80` and `--gpu a100>=80GB` produce the same
search (`gpu_ram>=80`) and match the same A100 80GB offers. Hardware
ceilings are recognised per model — A100 40GB/80GB, H100 80GB, H200 141GB,
RTX 4090 24GB, RTX 3090 24GB, etc. (see `internal/vastai/hardware_memory.go`
for the full table). For non-ceiling values like `--gpu-mem 60`, the +2GB
headroom is still applied (effective floor 62GB) — the same behaviour as
before.

Use `--gpus N` when the job needs multiple GPUs visible on the same host or
rental instance. The count is an exact single-host shape: `--gpus 4` searches
for one target with four GPUs, not four separate one-GPU targets. On rentals,
Weft passes the count to provider offer search and instance creation. On
hosts, the runner exposes the selected devices through `CUDA_VISIBLE_DEVICES`.

```bash
weft run --tag rental --gpu h100 --gpus 4 --gpu-mem 80 \
  -m "TP=4 vLLM run" \
  'uv run experiments/multigpu.py --tp 4'
```

Topology constraints are optional:

```bash
weft run --gpu h100 --gpus 4 --interconnect nvlink 'python train.py'
weft run --gpu h100 --gpus 4 --nvlink-required 'python train.py'
```

`--interconnect any` is the default for multi-GPU requests. `nvlink` is a hard
filter when provider metadata or the offer name contains an explicit NVLink/SXM
signal; Weft does not assume that an unknown multi-GPU offer has NVLink. `pcie`
rejects offers with explicit NVLink/SXM signals. Use `--cpu-cores N` to require
a minimum effective CPU core/vCPU count on rental offers; `cpu-intensive` still
uses `WEFT_COMPUTE_CPU_CORES` as its default floor.

Use `--input` to declare data the job needs:

```bash
weft run \
  --gpu a100 \
  --input hf:meta-llama/Llama-3-8B \
  --input hf-dataset:allenai/c4 \
  'python train.py'
```

Hugging Face inputs influence score reasons and placement ranking. A host that
already has the asset local avoids transfer time and ranks better. `checkpoint:`
and `corpus:` inputs are host-local assets: placement requires a host that
already has each declared asset. `local:` inputs are synced before execution but
are not scored as reusable data assets.

Use tags when you need to change the placement domain:

| Tag | Effect |
| --- | --- |
| `rental` | Skip inventory placement and send the job toward rental GPU workflows. Alias: `cloud`. |
| `inventory` | Keep the job on inventory hosts only; do not launch it on rental GPUs. Alias: `on-prem`. |
| `provider:<name>` | Pin rental placement to a provider such as `provider:vastai` or `provider:runpod`. |
| `interruptible` | Allow interruptible cloud offers for this job. Alias: `preemptible`. |

`rental` and `inventory` are mutually exclusive. A provider tag also skips
local placement because it names a rental provider directly.

For benchmark coverage runs, `weft start instance --distinct-machines` changes
new-instance eligibility so selected jobs land on different provider physical
machines. See [Distinct physical machines](instances.md#distinct-physical-machines)
for launch syntax, `--avoid`, and `--affinity`.

## Reserved Tags

Most tags are ordinary labels for grouping jobs. These tags have scheduler or
runner behavior:

| Tag | Behavior |
| --- | --- |
| `processed` | Marks a job as processed; filtered by `--processed` / `--unprocessed` on `weft jobs list`. Set with `weft mark-processed`. |
| `exclusive` | Requires the host to be idle while the job runs. |
| `benchmark-isolation` | Requires an idle host and enables benchmark protections. Auto-placement skips hosts marked `shared = true`, but an explicit host still runs there. |
| `cpu-intensive` | Declares that the job is CPU-heavy. Skips the queue-contention penalty in run-time estimation, adds a placement bonus proportional to free CPU capacity, requires rental instances to expose at least `WEFT_COMPUTE_CPU_CORES` effective CPU cores (default `16`), and prefers on-prem placement unless a rental is estimated at least 30 minutes faster. Alias: `compute-intensive` (deprecated). |
| `rental` | Bind the job to cloud rental placement. Alias: `cloud`. |
| `inventory` | Bind the job to on-prem inventory placement. Alias: `on-prem`. |
| `interruptible` | Mark the job as safe for interruptible / spot cloud offers. Alias: `preemptible`. |
| `provider:<name>` | Pin rental placement to a specific cloud provider. |

Benchmark jobs normally avoid shared inventory hosts. Adding `inventory`
keeps the job on inventory hosts and allows benchmark placement on hosts marked
`shared = true`.

CPU-intensive jobs can still run on rentals, but both new offers and
already-running instances must meet the CPU floor. Existing rental instances
with unknown CPU metadata are skipped for cpu-intensive reuse and rebalance.
Set `WEFT_COMPUTE_CPU_CORES` to raise or lower the default 16-core floor.

`cpu-intensive` does **not** exclude offers that have a GPU — Vast.ai is a
GPU marketplace and every offer ships with at least one card. A CPU-only
script that lands on a rental still gets a GPU attached, and a script that
auto-detects CUDA will use it. To pin a CPU-only script to CPU regardless
of the rental's hardware, set `CUDA_VISIBLE_DEVICES = ""` in the script's
PEP 723 `[tool.weft.env]` (or pass `--env CUDA_VISIBLE_DEVICES=`). See
[Running CPU-only scripts on cloud rentals](workflow-guide.md#running-cpu-only-scripts-on-cloud-rentals).

## Reading Score Reasons

Use `--dry-run` to inspect placement without creating a job:

```bash
weft run --dry-run --gpu ampere+ --input hf:meta-llama/Llama-3-8B 'python train.py'
```

Placement reasons are short fragments attached to each candidate. Common
fragments include:

| Reason | Meaning |
| --- | --- |
| `has NVIDIA A100 GPU` | The host passed the GPU class constraint. |
| `no GPU with >=24GB` | The host failed the GPU memory constraint. |
| `no ampere+ GPU` | The host failed the GPU class or generation constraint. |
| `host is opt-in only (specify with --host)` | The host is excluded from automatic placement unless explicitly named. |
| `shared host excluded for benchmark-isolation auto-placement` | A benchmark job skipped a shared host. |
| `3/3 inputs local` | All declared data assets are already present on the host. |
| `~20m transfer for 1 missing inputs (learned, n=4)` | Weft estimated transfer time from observed or static bandwidth. |
| `2 jobs queued (~60m drain)` | Queue depth contributes estimated wait time. |
| `25% contention overhead` | Current GPU occupancy inflated the run-time estimate. |
| `GPU perf 1.4x` | Host performance metadata improved the estimate. |
| `est. ~45m total (30m queue + 0m transfer + 15m run)` | The final completion estimate used for ranking. |

If no inventory host matches, unplaced job output groups the strongest
rejection reasons, for example:

```text
no local host matched gpu=ampere+, inputs=1
3 hosts: no ampere+ GPU
1 host: host is opt-in only (specify with --host)
```

For unplaced rental jobs, use the autopilot diagnostics:

```bash
weft autopilot status
weft autopilot blocked
```

See [Autopilot](autopilot.md) for blocked reasons, runaway-breaker trips,
pause/resume, and unattended auto-launch behavior.

## Strategy

Autopilot uses the campaign objective to choose between eligible cloud offers:

```toml
[campaign]
auto_objective = "cost_first" # cost_first | balanced | time_first
```

`cost_first` is the default. `balanced` gives cost and completion time similar
weight, and `time_first` spends more readily to reduce completion time.

## Ground Truth

This guide is the operator view. The formal placement model lives in
[`specs/inventory-placement.allium`](../../specs/inventory-placement.allium).
Related behavior for job moves and status synchronization is covered by the
Allium specs in [`specs/`](../../specs/).
