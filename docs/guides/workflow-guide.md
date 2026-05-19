# Weft Workflow Guide

This guide walks through common workflows with complete examples drawn from real
usage patterns. Each section shows how weft's features work together in practice.

## Quick orientation

Weft manages jobs on remote GPU hosts. You submit from your laptop, and the
remote queue runner handles execution even while your laptop sleeps.

```
laptop$ weft run titan 'uv run python train.py'
# Job 100 queued on titan
# Next: weft tui (or weft log 100)
```

If you omit the host, weft picks one automatically based on GPU requirements,
data locality, and current load:

```
laptop$ weft run --gpu-class a100 'uv run python train.py'
# Auto-placed on atlas (gpu_class match, 2 queued jobs)
```

## Declaring data dependencies

Jobs that need HuggingFace models, datasets, or other data assets should declare
them with `--input`. The coordinator uses these declarations to:

1. **Pick the best host** — prefer hosts that already have the data cached
2. **Pre-stage data** — rsync assets from another host if needed
3. **Download automatically** — fetch HF models/datasets onto the target
   on-prem host if no host has them yet

```
laptop$ weft run \
  --input hf:EleutherAI/pythia-160m \
  --input hf-dataset:wikitext \
  -m "EXP-010: Developmental dynamics (Pythia-160M)" \
  'uv run python scripts/developmental_dynamics.py \
    --checkpoints 64,512,4000,32000,143000 --device cuda'
# Auto-placed on atlas (pythia-160m cached there)
```

Use `hf:<repo>` for Hugging Face model repositories and
`hf-dataset:<repo>` for dataset repositories. If a dataset is declared with
`hf:`, Weft treats it as a model and the Hugging Face model lookup can fail
before staging starts.

If the model is on atlas but not titan, the coordinator places the job on
atlas. If neither host has it, the coordinator can download it before the job
starts using `huggingface-cli`, after checking that the target HF cache volume
has enough free space. When you want to warm a cache ahead of time or ensure a
specific host has the asset, use `weft data fetch`; you do not need a separate
prefetch job.

On shared filesystems, Hugging Face cache directories can be owned by a
different UID than the job process. If cache writes fail with `PermissionError`,
use a project-local cache:

```
laptop$ weft run \
  --env HF_HOME=/home/oliver/code/research/my-project/cache/hf \
  --input hf:gpt2 \
  'uv run python train.py'
```

For scripts that call Hugging Face APIs at runtime, make sure host-level offline
settings are not inherited unintentionally:

```
laptop$ weft run \
  --env HF_HUB_OFFLINE=0 \
  --env TRANSFORMERS_OFFLINE=0 \
  'uv run python train.py'
```

**Declare all models your job downloads**, not just the primary one. If your
script uses `AutoTokenizer.from_pretrained("bert-base-uncased")` in addition to
the main model, declare both:

```
laptop$ weft run \
  --input hf:meta-llama/Llama-3.1-8B \
  --input hf:bert-base-uncased \
  'uv run python scripts/collect_all.sh'
```

This matters because weft uses input declarations to:
- **Estimate disk space** for cloud instances (undeclared models can cause disk-full failures)
- **Score host placement** based on data locality
- **Pre-stage data** to avoid download delays during job execution

For runtime setup that is not an input asset, such as `uv sync`, pip wheel
caches, CUDA wheels, vLLM, or temporary build trees, add explicit scratch
headroom:

```
laptop$ weft run \
  --input hf:meta-llama/Llama-3.1-8B \
  --runtime-disk 24 \
  'uv sync --project scripts/vllm-profiling && python scripts/profile.py'
```

Use `--disk N` when you want to force the total rental disk floor. Weft sizes
declared inputs, cached uv-lock estimates, Docker image overhead, and prior
observed disk peaks when available, but runtime setup caches are explicit:
add `--runtime-disk` when command startup needs extra scratch space.

### Declaring outputs

Use `--output` to declare what a job produces. This lets downstream jobs find
the data and lets the coordinator pre-stage it to the right host:

```
laptop$ weft run titan \
  --output file:~/outputs/traces/entropy-sweep/ \
  -m "Generate entropy traces" \
  'uv run python scripts/generate_traces.py --output ~/outputs/traces/entropy-sweep/'
# Job 4674 queued

laptop$ weft run atlas \
  --after 4674 \
  --input file:~/outputs/traces/entropy-sweep/ \
  -m "Analyze traces on A100" \
  'uv run python scripts/analyze_traces.py --input ~/outputs/traces/entropy-sweep/'
# Job 4675 queued (depends on 4674)
# Coordinator rsyncs traces from titan → atlas before starting
```

The coordinator handles the cross-host rsync automatically. You declare what the
job needs and what it produces; the coordinator figures out the rest.

### Script metadata

Instead of passing `--gpu`, `--gpu-mem`, `--input`, etc. on every invocation,
you can declare requirements inside the script using a
[PEP 723](https://peps.python.org/pep-0723/) inline metadata block with a
`[tool.weft]` table:

```python
# /// script
# requires-python = ">=3.10"
# dependencies = ["torch", "transformer_lens"]
#
# [tool.weft]
# gpu-mem = 40
# inputs = ["hf:gpt2"]
# ///
```

Supported keys (all optional):

| Key         | Type             | Equivalent CLI flag |
|-------------|------------------|---------------------|
| `gpu`       | string           | `--gpu`             |
| `gpu-class` | string           | `--gpu-class`       |
| `gpu-mem`   | int or `">=NGB"` | `--gpu-mem`         |
| `gpu-mem-strict` | bool       | `--gpu-mem-strict`  |
| `disk` / `disk-gb` | int or `"NGB"` | `--disk`      |
| `runtime-disk` / `runtime-disk-gb` | int or `"NGB"` | `--runtime-disk` |
| `gpu-arch-max`   | string      | *(no CLI flag — overrides auto-inferred GPU arch upper bound; see below)* |
| `inputs`    | list of strings  | `--input`           |
| `outputs`   | list of strings  | `--output`          |
| `tags`      | list of strings  | `--tag`             |
| `interruptible` | bool        | `--tag interruptible` |
| `image`     | string           | `.weft.toml [cloud] image` |
| `min-driver` / `min_driver` | string or int | `.weft.toml [cloud] min_driver` |
| `min-cuda` / `min_cuda` | string | `.weft.toml [cloud] min_cuda` |
| `image-pull-secret` / `image_pull_secret` | string | `.weft.toml [cloud] image_pull_secret` |
| `vast-cap-add` | list of strings | Vast.ai `--cap-add` (cloud instance launch) |
| `uv-args`   | list of strings  | *(injected into `uv run`)* |
| `env`        | table of strings | `--env` (merged)           |
| `pre-install` | string          | *(prepended to command)*   |

CLI flags always override script metadata. Tags are additive (merged from both
sources). This format is compatible with `uv`'s own PEP 723 support — you can
declare both Python dependencies and weft resource requirements in the same
block.

Malformed PEP 723 metadata is a submission error. Weft does not ignore a broken
`[tool.weft]` table, because doing so would silently drop resource constraints,
inputs, and environment defaults.

The `uv-args` key injects extra arguments into `uv run` commands. For example,
`uv-args = ["--system"]` rewrites `uv run script.py` to
`uv run --system script.py`. For common direct Python invocations like
`python script.py` and `python3 script.py`, weft also rewrites to `uv run`
before applying `uv-args`.

The `env` key sets environment variables for the job. Declare it as a TOML
sub-table:

```toml
[tool.weft.env]
UV_SYSTEM_PYTHON = "1"
CUDA_HOME = "/usr/local/cuda"
```

This is equivalent to passing `--env UV_SYSTEM_PYTHON=1 --env CUDA_HOME=/usr/local/cuda`.
Env vars from script metadata are merged with `--env` flags (both sources are
kept; `--env` flags take precedence for duplicate keys at the runner level).
This is useful for container-based jobs that need specific environment
configuration, such as `UV_SYSTEM_PYTHON=1` for scripts that import packages
from the container's system Python.

The `pre-install` key specifies a shell command to run before the job command.
It is prepended to the command with `&&`. This is useful for installing system
packages on cloud instances:

```toml
[tool.weft]
pre-install = "apt-get update && apt-get install -y libnuma-dev"
```

The `interruptible` key opts the job into interruptible cloud placement:

```toml
[tool.weft]
interruptible = true
```

Use this only for jobs that are safe to resume/retry (for example, periodic
checkpoint writes to declared output directories). The key `preemptible` and
tag `preemptible` are accepted as synonyms.

#### `[tool.uv]` index settings

Weft also reads `[tool.uv]` settings from the PEP 723 metadata block. Since
`uv run` does not natively read `[tool.uv]` from inline script metadata, weft
extracts `index-url` and `extra-index-url` and converts them to `UV_INDEX_URL`
and `UV_EXTRA_INDEX_URL` environment variables. This is useful for scripts that
need packages from custom PyPI indexes (e.g., CUDA-specific wheels):

```python
# /// script
# dependencies = ["sglang[srt]>=0.4", "pynvml>=12.0"]
# [tool.uv]
# index-url = "https://docs.sglang.ai/whl/cu124"
# extra-index-url = ["https://pypi.org/simple", "https://flashinfer.ai/whl/cu124/torch2.5"]
# [tool.weft]
# gpu = "nvidia>=20GB"
# image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"
# pre-install = "apt-get update && apt-get install -y libnuma-dev"
# ///
```

Explicit `[tool.weft.env]` entries for `UV_INDEX_URL` or `UV_EXTRA_INDEX_URL`
take precedence over `[tool.uv]` values.

The `image` key specifies a Docker image for cloud execution. Image precedence
(highest to lowest): `.weft.toml [cloud] image` > script `image` > auto-selected
PyTorch image > global default. Script metadata works with any command that
references a `.py` file, including `uv run script.py`, `python script.py`, and
compound commands like `pip install foo && python script.py`.

For custom or vendor-curated images, weft reads OCI image metadata and applies
NVIDIA `NVIDIA_REQUIRE_CUDA` constraints during cloud placement when it can
fetch the image config. You can also declare explicit floors:

```python
# /// script
# [tool.weft]
# image = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"
# min-driver = "535"
# min-cuda = "12.9"
# image-pull-secret = "ghcr.io"
# ///
```

`min-driver` filters Vast.ai offers by NVIDIA driver version. RunPod exposes a
CUDA compatibility filter instead, so weft passes `min-cuda` to RunPod pod
creation. `image-pull-secret` names a configured `[registry]` entry; if it is
omitted, weft matches by the image registry hostname.

The `vast-cap-add` key requests extra Linux capabilities on Vast.ai
instances. Example:

```python
# /// script
# [tool.weft]
# gpu = "nvidia>=24GB"
# vast-cap-add = ["SYS_ADMIN"]
# ///
```

This maps to `vastai create instance ... --cap-add SYS_ADMIN` for any launch
group containing that job. If a grouped launch contains mixed jobs and any job
sets `vast-cap-add`, the new instance is launched with the union of requested
capabilities. For safety, groups that request `vast-cap-add` are not placed on
existing reusable instances with unknown launch capabilities.

`[tool.weft]` metadata is applied when submitting a new job (`weft run`). During
`weft retry`, weft re-reads script metadata and refreshes GPU defaults from it.
Use `weft retry --gpu/--gpu-class/--gpu-mem/--gpu-mem-strict` when you want explicit overrides.

**Setup phase skipping:** Add `isolated = true` to the `[tool.weft]` table
to skip the `uv sync` setup phase. Use this only for truly self-contained
scripts whose PEP 723 `dependencies` list everything they need — `uv run`
creates an isolated environment that does **not** include the project's
packages. Scripts that import from the project package must not set
`isolated = true`.

### GPU architecture upper bound (auto-inferred from torch pin)

Pinned PyTorch wheels target a fixed set of CUDA compute capabilities. Running
on a newer GPU than the wheel was built for fails at kernel-launch time —
sometimes with a clear "no kernel image is available for execution" error,
sometimes with a confusing illegal-memory-access. To prevent this, weft infers
an upper bound on GPU compute capability from the project's torch pin and
filters out incompatible hosts and cloud offers.

How it works:

- At submission time, weft reads `uv.lock` (preferred) or `pyproject.toml`,
  finds the pinned torch version and CUDA wheel variant (e.g. `cu121`, `cu128`),
  and looks up the highest compute capability supported by those wheels.
- That cap becomes a **hard filter**: hosts and cloud offers whose GPUs
  exceed the cap are excluded. GPUs with unknown caps are accepted.
- Weft prints the inferred ceiling once at submission, e.g.
  `Inferred GPU arch ceiling: compute cap <= 9.0`.

To override, set `gpu-arch-max` in `[tool.weft]`:

| Value          | Effect                                                     |
|----------------|------------------------------------------------------------|
| `"any"`        | Disable filtering (use when wheels are source-built/nightly with broader arch coverage). |
| `"hopper"`     | Cap at the highest cap for that generation (`9.0`).         |
| `"9.0"`        | Cap at exactly that compute capability.                     |
| `""` *(default)* | Auto-infer from `uv.lock` / `pyproject.toml`.            |

Example: a project pinned to `torch==2.4.1+cu121` only ships kernels up to
`sm_9.0`. Without this filter, a Vast.ai offer for an `RTX PRO 4500 Blackwell`
(`sm_12.0`) would be picked, the instance would launch, and the job would die
on first kernel call. With the filter, the offer is rejected before launch.

If you've upgraded to wheels that *do* include newer arches but `uv.lock` hasn't
been refreshed, set `gpu-arch-max = "any"` for that script as a temporary
escape hatch.

### Avoiding GPU over-provisioning

The `gpu-mem` value is a **requested floor**. By default, weft adds a `+2GB`
safety margin for placement and queue admission checks. For example,
`gpu-mem = 8` is treated as an effective `10GB` requirement. Use strict mode
(`gpu-mem-strict = true` or `--gpu-mem-strict`) to keep exact matching.

Memory inside a GPU selector is different. `gpu = "a100>=80GB"` or
`--gpu a100>=80GB` means "use an A100-class GPU whose advertised capacity is at
least 80GB"; weft does not add `+2GB` to that hardware floor. Use separate
`gpu-mem` / `--gpu-mem` when the number is the workload's expected VRAM use.

The default floor is 20GB when any GPU flag is used and no explicit `gpu-mem`
is provided. For lightweight workloads, this default causes the bidding system
to consider all GPU tiers including expensive H100 and H200 instances that
provide no speedup.

Set `gpu-mem` to actual peak VRAM usage (with headroom) to keep the floor low.
A lower floor means the bidding system can select cheaper, smaller GPUs that are
just as fast for the workload:

```python
# /// script
# [tool.weft]
# gpu = "nvidia"
# gpu-mem = 8
# inputs = ["hf:gpt2", "hf-dataset:wikitext"]
# ///
```

For example, GPT-2 small (124M params) training uses ~3GB of VRAM. Declaring
`gpu-mem = 8` gives an effective `10GB` floor by default, which still allows
placement on GPUs like the T4 (16GB), while avoiding exact-capacity 8GB cards.
If you need exact 8GB matching, set `gpu-mem-strict = true`.

### Project-relative data inputs

Use the `local:` prefix to declare project-relative directories that should be
synced to the remote host before the job runs:

```python
# /// script
# [tool.weft]
# inputs = ["local:data/conllu/", "hf:bert-base-uncased"]
# outputs = ["local:cache/representations/"]
# ///
```

Or via CLI:

```
weft run --input local:data/conllu/ -- uv run python src/extract_representations.py
```

`local:` paths are resolved relative to the project directory and synced via
rsync as extra paths — they bypass default sync excludes (like `cache/`). Use
subdirectories rather than syncing an entire large `data/` tree.

On restart (`weft job restart`), weft re-scans input/output metadata so updated
`local:` declarations take effect without manual `--input` flags. GPU keys from
`[tool.weft]` are also re-applied, and explicit retry flags still win.

### Checking data locality

See what's cached where:

```
laptop$ weft data where hf:meta-llama/Llama-3-8B
HOST     SIZE     LAST SEEN   PATH
titan    15.2GB   3m ago      /home/oliver/.cache/huggingface/hub/models--meta-llama--Llama-3-8B
atlas    15.2GB   8m ago      /home/oliver/.cache/huggingface/hub/models--meta-llama--Llama-3-8B
```

To refresh inventory from an existing cache:

```
laptop$ weft host data atlas --scan
```

To request that a host download a model or dataset now:

```
laptop$ weft data fetch hf:meta-llama/Llama-3-8B --host atlas
# Request 17 completed

laptop$ weft data requests --host atlas
ID  HOST   ASSET                           REVISION  STATUS     REQUESTED  SIZE
17  atlas  hf:meta-llama/Llama-3-8B       main      completed  0s ago     15.2GB
```

`localhost` is also supported as a fetch target when you want to populate the
local machine's Hugging Face cache without going through SSH:

```
laptop$ weft data fetch hf:meta-llama/Llama-3-8B --host localhost
# Request 18 completed
```

### Registering research checkpoints

For non-HuggingFace data (model checkpoints, gradient snapshots, etc.), register
them manually so weft can use them for placement scoring:

```
laptop$ weft data add ~/code/research/LM2/runs/gpt2-ft-v1
# Registered checkpoint:LM2/runs/gpt2-ft-v1 on studio

laptop$ weft data add ~/code/research/LM2/runs/gpt2-ft-v1 --host cool100
# Registered checkpoint:LM2/runs/gpt2-ft-v1 on cool100

laptop$ weft data where checkpoint:LM2/runs/gpt2-ft-v1
HOST     SIZE   LAST SEEN   PATH
studio   -      0s ago      ~/code/research/LM2/runs/gpt2-ft-v1
cool100  -      0s ago      ~/code/research/LM2/runs/gpt2-ft-v1
```

For data outside a repository, use `--name` to set an explicit asset ID:

```
laptop$ weft data add ~/research/data/gradient-datasets/gpt2-grads \
  --host studio --name gpt2-grads-wikitext
```

Then use `--input checkpoint:<name>` for auto-placement:

```
laptop$ weft run --input checkpoint:LM2/runs/gpt2-ft-v1 'uv run python eval.py'
# Auto-placed on studio or cool100 (whichever has the checkpoint)
```

## Ablation sweep with a fan-out dependency chain

A common ML research pattern: run one baseline configuration, then fan out
multiple ablation variants that each depend on the baseline completing first
(e.g., to reuse a shared cache, confirm the pipeline works, or serialize GPU
access).

### Baseline first

```
laptop$ weft run atlas --gpu 1 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: Unpruned + LoRA control (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion unpruned'
# Job 4823 queued
```

### Fan out ablations with `--after`

```
laptop$ weft run atlas --gpu 1 --after 4823 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: Reverse L1 (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion reverse_l1'
# Job 4828 queued (depends on 4823)

laptop$ weft run atlas --gpu 1 --after 4828 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: Gamma + Alpaca (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion gamma'
# Job 4829 queued (depends on 4828)

laptop$ weft run atlas --gpu 1 --after 4829 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: L1-norm + Alpaca (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion l1_norm'
# Job 4830 queued (depends on 4829)
```

The whole chain runs sequentially on GPU 1. If any step fails, downstream jobs
don't start, so you can fix the issue and retry from that point.

### Multiple random seeds as a sub-chain

```
for seed in 0 1; do
  weft run atlas --gpu 1 --after 4830 \
    --tag markov-attention --tag EXP-112 \
    -m "EXP-112: Random seed=$seed + Alpaca (GPU 1)" \
    "uv run python -u scripts/exp112_pruning_lora_recovery.py \
      --device cuda --criterion random --seed $seed"
done
# Job 4832 queued (depends on 4830)
# Job 4833 queued (depends on 4832)
```

### Use `--after-any` for jobs that should run regardless

If you want a cleanup or summary job to run even if a step fails:

```
laptop$ weft run atlas --after-any 4830 \
  -m "Summarize EXP-112 results" \
  'uv run python scripts/summarize_exp112.py'
```

## Running parallel experiments across GPUs

On a multi-GPU host like atlas (2x A100 + 8x RTX 2080 Ti), you can run
independent experiments in parallel by pinning them to different GPUs.

### Two independent signal sweeps, one per A100

```
# GPU 0: entropy-based signals
laptop$ weft run atlas --gpu 0 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: entropy signal (A100 GPU 0)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal entropy --dtype bfloat16 --resume'
# Job 4822 queued

laptop$ weft run atlas --gpu 0 --after 4822 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: entropy_delta signal (A100 GPU 0)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal entropy_delta --dtype bfloat16 --resume'
# Job 4825 queued (depends on 4822)

# GPU 1: different signals in parallel
laptop$ weft run atlas --gpu 1 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: random signal baseline (A100 GPU 1)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal random --dtype bfloat16 --resume'
# Job 4834 queued (runs simultaneously with 4822)

laptop$ weft run atlas --gpu 1 --after 4834 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: varentropy signal (A100 GPU 1)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal varentropy --dtype bfloat16 --resume'
# Job 4836 queued (depends on 4834)
```

GPU 0 and GPU 1 each have their own sequential chain, but the two chains run
concurrently. This doubles throughput for independent experiments.

## Combinatorial grid: every combination of parameters

When you need to run the same tool across every combination of inputs and
settings — e.g., measuring compression across datasets, data types, and codecs:

```
for dataset in gpt2-baseline gpt2-gct gpt2-backslash gpt2-gct-backslash; do
  for dtype in gradients weights; do
    for granularity in whole-file per-layer; do
      weft run titan \
        --tag compression-lab --tag reg-matrix-ans \
        -m "sparse-ans $dataset $dtype $granularity" \
        "uv run compression-lab measure \
          --data ~/code/research/data/regularization-matrix/$dataset/${dtype}_*.safetensors \
          --codec sparse-ans --granularity $granularity"
    done
  done
done
```

This produces 16 jobs (4 datasets x 2 data types x 2 granularities). Tags let
you filter them as a group in the TUI or when listing:

```
laptop$ weft list --tag reg-matrix-ans --status completed
```

Run a second codec variant by changing the tag and codec:

```
# Same grid, different codec
for dataset in gpt2-baseline gpt2-gct gpt2-backslash gpt2-gct-backslash; do
  for dtype in gradients weights; do
    for granularity in whole-file per-layer; do
      weft run titan \
        --tag compression-lab --tag reg-matrix-bitcast \
        -m "sparse-ans $dataset $dtype $granularity bitcast16" \
        "uv run compression-lab measure \
          --data ~/code/research/data/regularization-matrix/$dataset/${dtype}_*.safetensors \
          --codec sparse-ans --granularity $granularity --bitcast float16"
    done
  done
done
```

## Validate on a small host, then run the full sweep

When developing a new experiment script, iterate on a cheap/fast host first,
then move to the big GPU for the full run.

### Iterate on titan (RTX 3090)

```
laptop$ weft run titan \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation: single config (12h, 100 GFLOPs) to test pipeline" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 100 --device cuda'
# Job 4768 completed (exit 1 — found a bug)

laptop$ weft run titan --after 4768 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation v2: 12h, 500 GFLOPs, attentions fix" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 500 --device cuda'
# Job 4773 completed (exit 1 — another issue)

laptop$ weft run titan --after 4773 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation v3: fingerprint fix" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 500 --device cuda'
# Job 4777 completed (exit 1 — still not right)
```

### Once the pipeline works, move to atlas for the full sweep

```
laptop$ weft run atlas --gpu 1 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation v4: head_count_sweep (atlas GPU 1)" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 500 --device cuda'
# Job 4784 completed (exit 0 — success!)

laptop$ weft run atlas \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 full sweep Design A: 4,8,12,24,48 heads" \
  'uv run python scripts/head_count_sweep.py --design A --device cuda'
# Job 4798 queued

laptop$ weft run atlas \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 full sweep Design B: 4,8,12,24 heads (fixed d_head=64)" \
  'uv run python scripts/head_count_sweep.py --design B --device cuda'
# Job 4799 queued
```

The description versioning (v1, v2, v3...) makes it easy to see the iteration
history in `weft list` or the TUI.

## Bursting to cloud GPUs

When local GPUs are busy or no local host has the right hardware, weft can run
jobs on Vast.ai cloud instances.

### Automatic acceptance for unplaceable jobs

If you request a GPU that no local host has, weft accepts the job instead of
rejecting it:

```
laptop$ weft run --gpu-class hopper+ 'python train.py'
No local host matches constraints: gpu-class=hopper+
Job #4820 accepted (needs rental host)
Use 'weft place' or press 'c' in the TUI to launch on a rental GPU.
```

The job appears in the TUI with status `$ needs rental`. From there, press `c`
to open the rental menu for a single job, or use `weft place` to batch-
launch all unplaced jobs at once.

### Prerequisites

```
pip install vastai
vastai set api-key YOUR_API_KEY
```

### Using `weft start instance`

> **Check the autopilot first.** Run `weft autopilot status` — if it's
> `running`, you usually don't need to launch by hand: the autopilot will
> pick up unplaced jobs on its own. Manual launching is not faster, just
> more controllable (cost limits, offer selection, parallelism). See
> [Coordinating with the autopilot](instances.md#coordinating-with-the-autopilot).

`weft start instance` (also `weft start instances` and `weft instance
launch`) groups unplaced jobs by GPU requirements, searches for Vast.ai
offers in parallel, and launches instances concurrently:

```
laptop$ weft start instance
```

The interactive TUI shows jobs grouped by GPU class with checkboxes.
Deselect jobs you don't want to launch, review cost estimates, and press
Enter. Weft provisions one instance per GPU group **in parallel** (recorded
together as a campaign batch), then segues into watch mode.

```
laptop$ weft start instance --dry-run    # Preview without launching
laptop$ weft start instance --watch      # Explicitly enter watch mode after launch
laptop$ weft start instance --no-watch   # Launch and exit immediately
laptop$ weft start instance --project X  # Only jobs from project X
laptop$ weft start instance --jobs wj42,wj43  # Only these specific jobs
laptop$ weft start instance --yes        # Non-interactive batch of everything unplaced
```

To launch only jobs for the current directory's project:

```
laptop$ weft project launch --yes --watch # Non-interactive, watch progress
laptop$ weft project launch --dry-run     # Preview for this project only
```

`weft place` and `weft campaign launch` remain as compatibility aliases.

After launch, monitor and manage:

```
laptop$ weft instance watch               # Live status of active instances
laptop$ weft instance list                # List instances
laptop$ weft instance ssh <id>            # SSH into an instance
laptop$ weft instance terminate <id>      # Destroy a single instance
laptop$ weft campaign watch <id>          # Watch a specific batch (and its relaunches)
laptop$ weft campaign terminate <id>      # Terminate every instance in the batch
```

See [Cloud GPU Instances](instances.md) for the full instance guide
(launching, monitoring, grace periods, configuration, lifecycle), and
[Campaigns](campaigns.md) for the batching concept.

### Using the TUI rental menu (single job)

For launching a single job from the TUI:

1. Open the TUI: `weft tui`
2. Navigate to a **queued** or **needs rental** job
3. Press `c` to open the rental GPU menu

```
┌─ Send to Rental GPU ────────────────────────────────────────┐
│                                                              │
│  > Free: wait ~25 min (queue: ~15 min + run: ~10 min)        │
│    Vast.ai RTX 4090 24GB: ~$0.45 (~2m setup + ~15m run)      │
│    Vast.ai A100 80GB:     ~$1.20 (~3m setup + ~10m run)       │
│                                                              │
│  ↑/↓ navigate  Enter select  Esc cancel                      │
└──────────────────────────────────────────────────────────────┘
```

For **queued** jobs, the **Free** option keeps the job in the local queue.
For **needs rental** jobs, the local option is omitted since no local host
can run the job. Vast.ai options show estimated cost and time. Select with
Enter, confirm the cost, and weft handles the full lifecycle: instance creation,
rsync, `uv sync`, job execution, output collection, and teardown.

If the job has `--gpu-class` or `--gpu-mem` constraints, the cloud search
respects them — only matching offers appear.

## Exclusive jobs for GPU-hungry workloads

Some jobs need exclusive access to all GPU memory. Tag them `exclusive` so the
queue runner runs them alone:

```
laptop$ weft run atlas \
  --tag exclusive \
  -m "CUDA multistream ANS (actual size)" \
  'uv run compression-lab gpu-bench --codec ans --multistream'
```

The queue runner waits until all other jobs finish, runs the exclusive job alone,
then resumes normal scheduling.

`exclusive` and `benchmark` are part of the broader catalog of tags the
scheduler treats specially. See the
[Placement guide § Reserved tags](placement.md#reserved-tags) for the full
list and effects.

For reproducible benchmarking, the `benchmark` tag goes further — it waits for
the whole system (CPU, RAM, GPU, VRAM) to be idle before starting:

```
laptop$ weft run atlas \
  --tag benchmark \
  -m "Measure throughput at batch_size=64" \
  'uv run python benchmark.py --batch 64'
```

Use the `rental` tag when a job should skip local placement and go straight to
the rental-GPU workflow:

```bash
laptop$ weft run \
  --tag rental \
  --gpu a100 \
  -m "Run on rental GPU" \
  'uv run python train.py'
```

Use `inventory` for the opposite behavior: the job may wait unplaced, but it
will not launch on rental GPUs.

```bash
laptop$ weft run \
  --tag inventory \
  --gpu a100 \
  -m "Wait for inventory A100 capacity" \
  'uv run python train.py'
```

### Chaining jobs after rental runs

You can chain downstream jobs to rental/ephemeral producers with `--after`,
`--after-any`, and `--needs`.

For rentals, `--after` acts as a **placement gate** rather than a co-location
constraint: the downstream job is held back from rental launch until its
upstream succeeds (or, for `--after-any`, terminates). Dependent jobs may
still land on different instances with different GPU specs — if you want
them on the same instance, declare the data edge with `--input`/`--output`
or `--needs`/`--produces` so the launcher groups them by affinity.

```bash
# Producer on a rental instance
laptop$ weft run \
  --tag rental --gpu nvidia \
  --produces output/model.pt \
  -m "Train on rental GPU" \
  'uv run python train.py'
# Job 1046 queued

# Downstream consumer
laptop$ weft run \
  --after 1046 \
  --needs output/model.pt:1046 \
  --gpu nvidia \
  -m "Evaluate checkpoint" \
  'uv run python eval.py'
```

For rental producers, weft waits for completion and artifact upload, then
stages needed files from cloud artifact storage before the consumer starts.

`--needs` resolves against the producer's recorded artifacts, not its live
state. A rental producer that already completed (hours or days ago) works the
same as one still running — weft looks up the job in the DB and stages the
needed files from R2 before the consumer starts. You do **not** need
`weft artifact get` + `--input local:` for this; reference the completed job
by ID with `--needs path:<job-id>`.

Caveat for on-prem (inventory) producers: staging reads from the producer
host's filesystem, not R2. If the producer's on-disk artifacts have been
cleaned up, a later `--needs` consumer can't find them. Run
`weft artifact sync <job-id>` to restore them, or fall back to
`weft artifact get` + `--input local:`.

## Auto-placement across hosts

When you don't specify a host, weft picks the best one. The scoring considers:

- **GPU constraints**: `--gpu-class` and `--gpu-mem` filter out hosts without
  the right hardware
- **Data locality**: `--input` assets on a host earn a bonus; missing data
  incurs a transfer penalty
- **Current load**: hosts with lower GPU/CPU utilization and shorter queues
  score higher

```
laptop$ weft run \
  --gpu-class a100 \
  --input hf:meta-llama/Llama-3-8B \
  -m "Llama inference on A100" \
  'uv run python inference.py'
# Auto-placed on atlas (gpu_class: a100, data: local, queue: 2 jobs)
```

See what weft knows about your hosts:

```
laptop$ weft host list
laptop$ weft host data
```

## Draft jobs: plan now, run later

When iterating on a command and not ready to submit:

```
laptop$ weft run --draft titan \
  -m "WIP: trying new loss function" \
  'uv run python train.py --loss focal'
# Job 500 created as draft (not synced to host)
```

Edit, tweak, then promote:

```
laptop$ weft edit 500 --command 'uv run python train.py --loss focal --gamma 2.0'
laptop$ weft job draft 500
# Job 500 status changed: draft → queued
```

Or from the TUI: navigate to the draft job, press `g` to run it.

## Working offline

weft is designed for laptops that come and go. If the remote host is unreachable:

```
laptop$ weft run titan 'uv run python train.py'
# Job 600 created (host offline — will sync when reachable)
```

The job is saved locally and pushed to the remote queue on the next sync.
The TUI shows stale data with a visual indicator when a host is unreachable.

## TUI keyboard reference

### Jobs view

| Key | Action |
|-----|--------|
| `↑/↓` | Navigate job list |
| `Space` | Page down |
| `b` | Page up |
| `t` | Jump to top |
| `l` | Toggle log view |
| `Tab`/`Shift+Tab` | Cycle detail tabs (Details / Logs / CPU) |
| `f` | Cycle job filters (Recent / All / Active / Succeeded / Failed) |
| `H` | Cycle host filter |
| `o` | Cycle sort order |
| `n` | New job |
| `e` | Edit queued job |
| `E` | Edit & restart (new job form pre-populated from selected job) |
| `r` | Refresh / sync |
| `g` | Start queued job now / resume paused |
| `p` | Pause running job |
| `k` | Kill running / cancel queued or needs-rental |
| `d` | Toggle draft / queued status |
| `c` | Cloud GPU options (queued or needs-rental jobs) |
| `F` | Move job to front of queue |
| `G` | Generate AI description |
| `R` | Restart job |
| `y` | Retry job |
| `x` | Remove job |
| `P` | Prune old jobs |
| `←/→` | Switch between Jobs and Hosts views |
| `?` | Help overlay |
| `Ctrl+Z` | Suspend (return to shell, resume with `fg`) |
| `q` | Quit |

### Hosts view

| Key | Action |
|-----|--------|
| `↑/↓` | Navigate host list |
| `i` | Info tab |
| `G` | GPU summary tab |
| `D` | Toggle AI host summaries |
| `S` | Start queue runner on selected host |
| `0`–`9` | Select GPU tab by hardware index |
| `←/→` | Switch between Jobs and Hosts views |
| `q` | Quit |
