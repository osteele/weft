# Placement

Weft placement decides where a job should run when you omit the host:

```bash
weft run --gpu a100 --input hf:meta-llama/Meta-Llama-3-8B 'python train.py'
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

Queued placement states are not, by themselves, evidence of a stall. Rental
placement and new-instance startup commonly take 5-40 minutes and may retry
several provider offers or launches as the market changes. Use job-level
monitoring (`weft status <job> --wait`, `weft info <job>`, `weft log <job>`)
unless Weft reports a concrete blocker or terminal failure; avoid killing or
manually relaunching jobs just because placement is still retrying.

## Steering Placement

Use `--gpu`, `--gpu-class`, and `--gpu-mem` to describe required hardware:

```bash
weft run --gpu nvidia 'python train.py'
weft run --gpu 'nvidia>=24GB' 'python train.py'
weft run --gpu ampere+ 'python train.py'
weft run --gpu-class a100 --gpu-mem 60 'python train.py'
weft run --gpu h100 --gpus 4 --gpu-mem 80 'python train.py'
weft run --gpu h100-pcie 'python train.py'
weft run --gpu h100-hbm3 'python train.py'
```

GPU class matching supports exact models (`a100`, `rtx3090`, `gh200`),
generations (`ampere`, `hopper`), minimum generations (`ampere+`), and
families (`nvidia`, `apple`). Bare RTX model numbers such as `3090` are
normalized to `rtx3090`. Use `gh200` when the job needs a Grace Hopper
superchip specifically; `hopper` remains a broad generation constraint that
can also match H100 and H200.

Some data-center GPU families expose distinct package or memory variants.
For H100, `--gpu h100` remains broad and can match PCIe, SXM/HBM3, or NVL
offers. Use `--gpu h100-pcie`, `--gpu h100-sxm`, `--gpu h100-hbm3`, or
`--gpu h100-nvl` to pin the variant. `h100-hbm3` is an alias for the
SXM/HBM3 class and excludes H100 PCIe offers.

### Variant and memory are separate axes

A GPU class constrains the **variant** by filtering the provider's `gpu_name`
values, so `--gpu a100-sxm4` excludes A100 PCIe offers. It does **not**
constrain memory, and a trailing memory token in the class name binds nothing:

```bash
weft run --gpu a100-sxm4-80gb 'python bench.py'   # SXM4, ANY memory (40 or 80GB)
weft run --gpu 'a100-sxm4>=80GB' 'python bench.py' # SXM4 AND at least 80GB
```

The suffix is stripped before matching because provider `gpu_name` values carry
no memory component. Vast.ai calls both the 40GB and the 80GB part `A100 SXM4`,
so there is nothing for `-80gb` to match against. Memory can only be
expressed as a `>=NGB` predicate, which becomes a `gpu_ram>=` filter.

This matters when the GPU variant is itself an experimental variable: an
unbound memory axis means a bandwidth or capacity sweep can silently receive
the wrong part. `--gpu a100-sxm4-80gb` has been served by an A100 SXM4 40GB
(2039 GB/s requested, 1555 GB/s delivered). Prefer `--gpu "family-variant>=NNGB"`
whenever the hardware is the independent variable.

`weft info` reports the hardware a rental actually provided on a `Delivered:`
line, and warns when a class carried a memory suffix the delivered card does
not satisfy.

Note that `--gpu a100>=80GB` binds memory but *not* the variant, so it can
return either A100 SXM4 80GB (2039 GB/s) or A100 PCIe 80GB (1935 GB/s). Name
the variant too when the distinction matters.

### Memory floors and headroom

The `>=NGB` form on `--gpu` is a hardware capacity floor. For example,
`--gpu a100>=80GB` matches 80GB A100 offers exactly; it does not add the
`+2GB` workload headroom used by separate `--gpu-mem` reservations.

The separate `--gpu-class` + `--gpu-mem` form **also skips the +2GB headroom
when `--gpu-mem` matches the model's actual hardware ceiling**. So
`--gpu-class a100 --gpu-mem 80` and `--gpu a100>=80GB` produce the same
search (`gpu_ram>=80`) and match the same A100 80GB offers. Hardware
ceilings are recognized per model, including A100 40GB/80GB, H100 80GB, H200 141GB,
RTX 4090 24GB, RTX 3090 24GB, etc. (see `internal/vastai/hardware_memory.go`
for the full table). For non-ceiling values like `--gpu-mem 60`, the +2GB
headroom is still applied (effective floor 62GB), as before.

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

`--interconnect any` is the default for multi-GPU requests. Provider-measured
NVLink bandwidth takes precedence when available: a positive value satisfies
`nvlink`, while a reported zero satisfies `pcie`. If the provider reports no
measurement, Weft falls back to explicit NVLink/SXM signals in the offer name;
it does not assume that an unknown multi-GPU offer has NVLink. Use `--cpu-cores
N` to require a minimum effective CPU core/vCPU count on rental offers;
`cpu-intensive` still uses `WEFT_COMPUTE_CPU_CORES` as its default floor.

Use `--input` to declare data the job needs:

```bash
weft run \
  --gpu a100 \
  --input hf:meta-llama/Meta-Llama-3-8B \
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

`cpu-intensive` does **not** exclude offers that have a GPU. Vast.ai is a
GPU marketplace, and every offer ships with at least one card. A CPU-only
script that lands on a rental still gets a GPU attached, and a script that
auto-detects CUDA will use it. To pin a CPU-only script to CPU regardless
of the rental's hardware, set `CUDA_VISIBLE_DEVICES = ""` in the script's
PEP 723 `[tool.weft.env]` (or pass `--env CUDA_VISIBLE_DEVICES=`). See
[Running CPU-only scripts on cloud rentals](workflow-guide.md#running-cpu-only-scripts-on-cloud-rentals).

## Reading Score Reasons

Use `--dry-run` to inspect placement without creating a job:

```bash
weft run --dry-run --gpu ampere+ --input hf:meta-llama/Meta-Llama-3-8B 'python train.py'
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

## Why Are My Jobs on Separate Instances?

A rental instance runs one GPU job per GPU at a time. Most rentals are
single-GPU, so a job placed on a busy 1-GPU instance waits in that instance's
queue and runs only after the current job finishes. When compatible
single-GPU jobs can use the same GPU shape and a matching multi-GPU rental is
available, Weft may instead choose a packed candidate: one multi-GPU rental,
one job per GPU, each job pinned with its own `CUDA_VISIBLE_DEVICES` value.

Separate instances are still common. The autopilot may buy parallelism with
separate rentals when that scores better than packing, when only single-GPU
offers are available, or when jobs are not safe to co-run. Jobs tagged
`benchmark-isolation` are not packed with other jobs; they keep the idle-host
benchmark protections.

Two related surprises:

- **The instances are different sizes (32 / 48 / 80 GB).** A job with a
  `--gpu-mem` floor accepts any offer at or above that floor. Each instance is
  provisioned independently against whichever provider offer was cheapest and
  available on that pass, so a 24 GB-floor job can land on an 80 GB instance.
  There is no normalization to a single GPU size.
- **A stricter job stays on its own instance.** A job with a tighter memory
  floor or a lower architecture cap (e.g. `gpu-arch-max`) will not share an
  instance whose GPU it cannot use, even if a looser job could.

To see the concurrency limit directly, read `weft instance list`: the `GPUS`
column is the per-instance GPU count and `JOBS` is how many jobs are assigned.
`GPUS 1` with `JOBS 3` means one job runs and two wait; `GPUS 2` with two
compatible packed jobs can run both jobs at once. For a single job, ask why it
landed where it did:

```bash
weft job diagnose wj123   # the `placed:` line explains reuse vs. new launch
```

## Ground Truth

This guide is the operator view. The formal placement model lives in
[`specs/inventory-placement.allium`](../../specs/inventory-placement.allium).
Related behavior for job moves and status synchronization is covered by the
Allium specs in [`specs/`](../../specs/).

## Providers honor constraints differently

The same job does not mean the same thing to every provider. Weft preserves
those differences because normalizing them would require values it cannot
observe. Run:

```bash
weft provider constraints
```

for the per-provider table. The differences that bite most often:

- **Disk.** Vast.ai selects offers by available disk; RunPod sizes container
  disk at creation, so a disk floor shapes provisioning rather than selection.
- **Reliability.** Vast.ai publishes a 0-1 score. RunPod publishes none, but
  distinguishes Secure Cloud (vetted datacenter partners) from Community Cloud
  (peer hosts); weft translates that binary onto the same scale so a
  `--reliability` floor still selects. The mapping is a deliberate
  approximation. Prefer the survival model for quantitative analysis.
- **Driver and CUDA.** Vast.ai publishes driver version, so a floor filters at
  search. RunPod does not, so compatibility is probed *after* launch and an
  incompatible instance is destroyed. A driver pin therefore narrows the pool
  on Vast.ai and burns launch cycles on RunPod.
- **Geo.** `--exclude-geo` is currently honored on Vast.ai only. RunPod
  publishes a datacenter id in a different form than weft parses, so the
  constraint is accepted and **not** enforced there. The table marks this
  `NOT ENFORCED` rather than hiding it.

An axis marked `NOT ENFORCED` is accepted from you and honored by nobody. That
is a known hole, distinct from `not applicable`, which means the provider has no
such concept.

## Pinning a job to a physical machine

`--affinity` requires placement on a specific Vast.ai physical machine, named by
machine ID, instance (`wi...`), or job (`wj...`):

```bash
weft run --affinity wj5504 'python probe.py'      # same machine as that job ran on
weft run --affinity 49863 'python probe.py'       # named directly
weft start instance --affinity wj789 --jobs wj790 # at launch time instead
```

The job form is usually what you want: retroactive investigation starts from a
job whose result looked wrong, not from a machine number.

Use it when the machine is the variable: re-probing disputed vendor specs,
checking whether an anomalous benchmark row came from a noisy neighbor, or
reproducing an environmental outlier. `launches.machine_id`
records the machine for past runs, so "which physical machine produced this row"
is answerable after the fact.

Two things to expect:

- **It may wait.** Vast.ai's offer search has no machine filter, so weft polls
  and matches client-side. A machine that is currently rented, or simply absent
  from the market, leaves the job unplaced until it reappears. The autopilot
  carries the pin across planning ticks instead of failing once. Machines
  rotate in and out over weeks, so a pinned job can sit for a long time. That is
  the design, not a stall.
- **A machine is not a GPU.** Pinning gets you the same physical box. If it
  hosts several cards you may get a different one, and weft cannot tell you
  which card produced an earlier run.
- **An unidentified machine is skipped, not gambled on.** An offer or running
  instance that reports no machine ID is not confirmed to be the pinned one, so
  a pinned job passes it over. This mainly affects providers without per-offer
  machine identity. A pinned job will not reuse a RunPod instance whose machine
  weft cannot name, even when it otherwise fits.
- **On-prem hosts never satisfy a pin.** A pin names a provider physical
  machine, which no inventory host is, so a pinned job is excluded from
  on-prem placement even when its other constraints would fit an idle local
  GPU. Drop the pin if you want the job to float back on-prem.

`--affinity` on a job and on a launch intersect: each narrows the machines under
consideration, so a job pinned outside its launch's set is unplaceable rather
than silently honoring one of the two.
