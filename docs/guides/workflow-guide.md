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

## Source sync: the working tree, not commits

When you submit a job, weft tarballs the project's **working directory** and
ships it to the remote host. The sync reflects the *filesystem state* of your
working tree — **not** the committed state of the underlying git or
Jujutsu repository.

Three consequences worth internalizing:

- **Uncommitted edits are synced.** Edit a file, skip the commit, run
  `weft run` — the remote job sees the edit. There is no "commit before you
  submit" step.
- **A file that exists only in another commit/revision is not synced.** If a
  file is present in some branch or revision but not checked out into the
  current working tree, weft cannot see it. This bites hardest in Jujutsu,
  where every revision is its own filesystem snapshot and the working copy
  tracks whichever revision `@` points at: a `jj edit`, `jj new`, `jj abandon`,
  or `jj restore` that moves files in or out of the working copy changes what
  the next sync captures. The git equivalents are `git checkout`,
  `git stash`, and `git restore`.
- **Between two submissions, a VCS operation can change what gets synced.**
  For inventory hosts, weft re-syncs on every job, so the second job can see a
  different snapshot than the first. For cloud instances the tarball is built
  once at instance launch, but a resubmission onto a freshly launched instance
  picks up whatever the working tree contains *at resubmission time*.

The sync respects `.gitignore` (and `.weft.toml` `[sync] exclude_dirs`):
ignored paths such as `cache/`, `build/`, `__pycache__/`, and `.venv/` are
excluded from the tarball. On the remote host, extraction is additive —
files already present that are not in the tarball are left untouched, so
`.gitignore`'d caches written by prior jobs persist across syncs.

Run `weft sync inspect` to see exactly what the tarball will contain. When a
job fails with `bash: <script>: No such file or directory` or a
`ModuleNotFoundError` on a file you believe exists, check the working tree
(`ls`, `jj status`, `git status`) before suspecting a sync bug — the usual
cause is that the working copy was moved off the revision that holds the file.

## Declaring data dependencies

Jobs that need HuggingFace models, datasets, or other data assets should declare
them with `--input`. Weft uses these declarations to:

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

If the model is on atlas but not titan, Weft places the job on
atlas. If neither host has it, Weft can download it before the job
starts using `huggingface-cli`, after checking that the target HF cache volume
has enough free space. When you want to warm a cache ahead of time or ensure a
specific host has the asset, use `weft data fetch`; you do not need a separate
prefetch job.

For declared `hf:` / `hf-dataset:` inputs on cloud rentals, Weft's staging
download runs before the job command and is allowed to contact the Hub even if
the job itself requests offline runtime mode. Weft preserves credentials and
cache/proxy settings such as `HF_TOKEN`, `HF_HOME`, `HTTPS_PROXY`, and custom
certificate variables, but forces Hugging Face offline toggles off only for the
managed staging subprocess. The job command still receives the environment you
declared, so this pattern is portable across hosts with and without direct Hub
access:

```
laptop$ weft run \
  --input hf:gpt2 \
  --env HF_HUB_OFFLINE=1 \
  --env TRANSFORMERS_OFFLINE=1 \
  'uv run python train.py'
```

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

The "prior observed disk peaks" path reads `disk_used_bytes` from
`job_phase_timings` and the time-series peak from `job_timeseries`. The
estimator rejects any historical sample above a 2 TB plausibility bound
(`DiskTelemetryPlausibilityBytes`) — that range is reachable only when an
agent-side `statfs` probe misreports on an overlay/fuse container root, and
the estimator chooses to surface the bad sample (via `weft job anomalies`,
structured logs, and a one-line note prepended to `placement_reasons`)
rather than silently clamp it. If your job is being held up by an
"anomalous historical disk reading" note, run `weft job anomalies` to see
which prior job produced the bogus sample.

Weft sets `TMPDIR` to a writable per-job directory under `output/tmp` when the
environment does not already define it. Override it with `-e TMPDIR=...` only
when a job needs a different scratch location.

### Named assets — `asset:NAME`

When a file needs to be available to jobs on any host but isn't on Hugging
Face Hub and wasn't produced by another weft job (e.g. a hand-curated
evaluation corpus, a preprocessed pickle on your laptop), publish it as a
**named asset**. The file is uploaded to R2 once and staged onto whichever
host the consumer lands on.

```
# Publish a local file under a stable name:
laptop$ weft data publish output/exp207_eval_texts.pkl \
  --name exp207-eval-llama8b
# Published asset:exp207-eval-llama8b — uploaded, 187 MB (target path output/exp207_eval_texts.pkl)

# Publish a file that currently lives on an on-prem host:
laptop$ weft data publish --host cool30 /data/traces/toolagent.jsonl \
  --name toolagent-trace \
  --target-path data/traces/toolagent.jsonl

# Consume from any cloud rental or on-prem host with R2 access:
laptop$ weft run --gpu a100 \
  --input asset:exp207-eval-llama8b \
  -m "EXP-207 Phase 2" \
  'uv run python scripts/exp207_phase2_slicegpt_only.py'
```

At launch time weft stages the asset into the consumer's working directory
at the path recorded when you published (or the path given by
`--target-path`). The consumer script reads it the same way it would in the
producer's workspace.

**`asset:NAME` vs `checkpoint:NAME`.** `checkpoint:` is a placement-scoring
hint that pins the consumer to a host where the file is already present
(registered via `weft data add`). It does not transport bytes. `asset:`
transports the bytes through R2 and works from any host. Use `checkpoint:`
when the file is already on a specific GPU host (e.g. a fine-tuned model on
cool100) and you want to avoid re-copying it. Use `asset:` when the file is
on your laptop, on a remote host but should become portable, or you want
any-host availability.

**`asset:NAME` vs `--needs path:<job-id>`.** Use `--needs` when the file is
the output of another weft job — weft already auto-tracks files in
`output/` and pulls them from the producer's R2 artifacts. Use `asset:` for
files you publish explicitly (no producer job).

For multi-step data pipelines, choose the edge type by how the bytes should
move:

- Use `--produces`/`--needs` for files produced by an upstream weft job.
- Use `weft data publish --host HOST PATH --name NAME --target-path PATH` and
  `--input asset:NAME` for a host-local file that should become portable.
- Use `weft data add --host HOST PATH --name NAME` and
  `--input checkpoint:NAME` only when the consumer must run on a host that
  already has that file or directory.

Republishing under the same name overwrites the name → hash mapping.
Identical bytes skip the upload (content-addressed). v1 is single-file only;
directory publishing is planned. R2 must be configured.

### Declaring outputs

Use `--output` to declare what a job produces. This lets downstream jobs find
the data and lets Weft pre-stage it to the right host:

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
# Weft rsyncs traces from titan → atlas before starting
```

Weft handles the cross-host rsync automatically. You declare what the
job needs and what it produces; Weft figures out the rest.

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
# gpu = "h100"
# gpus = 4
# gpu-mem = 40
# inputs = ["hf:gpt2"]
# ///
```

Supported keys (all optional):

| Key         | Type             | Equivalent CLI flag |
|-------------|------------------|---------------------|
| `gpu`       | string           | `--gpu`             |
| `gpu-class` | string           | `--gpu-class`       |
| `gpus` / `gpu-count` | int    | `--gpus`            |
| `gpu-mem`   | int or `">=NGB"` | `--gpu-mem`         |
| `gpu-mem-strict` | bool       | `--gpu-mem-strict`  |
| `interconnect` | string (`any`, `pcie`, `nvlink`) | `--interconnect` |
| `cpu-cores` | int             | `--cpu-cores`       |
| `disk` / `disk-gb` | int or `"NGB"` | `--disk`      |
| `runtime-disk` / `runtime-disk-gb` | int or `"NGB"` | `--runtime-disk` |
| `gpu-arch-max`   | string      | *(no CLI flag — overrides auto-inferred GPU arch upper bound; see below)* |
| `inputs`    | list of strings  | `--input`           |
| `outputs`   | list of strings  | `--output`          |
| `tags`      | list of strings  | `--tag`             |
| `interruptible` | bool        | `--tag interruptible` |
| `image`     | string           | `.weft.toml [cloud] image` |
| `min-driver` / `min_driver` | string or int | `.weft.toml [cloud] min_driver` |
| `cuda-driver-min` (or legacy `min-cuda` / `min_cuda`) | string | `--cuda-driver-min` |
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

#### vLLM and SGLang

For vLLM, keep the dependency in PEP 723 or `pyproject.toml` and run through
`uv`. Weft treats `vllm` as a CUDA-heavy dependency, adds disk headroom, and can
select a PyTorch CUDA image when no image is configured:

```python
# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = ["vllm>=0.6", "transformers"]
# [tool.weft]
# gpu = "ampere+>=24GB"
# inputs = ["hf:gpt2"]
# ///
```

For serving jobs that need a pinned CUDA base image, keep the vLLM and
Transformers versions matched. This pin set is known to work on CUDA 12.4:

```python
# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = [
#   "vllm==0.8.5",
#   "transformers==4.51.3",
# ]
# [tool.weft]
# gpu = "ampere+>=24GB"
# image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"
# inputs = ["hf:gpt2"]
# ///
```

Invoke PEP 723 scripts as `uv run script.py`, not `uv run python script.py`.
The latter runs Python without installing the script's inline dependencies, so
console scripts such as `vllm` may be missing even though they are declared in
the script metadata.

For SGLang, use the project runtime image path unless you have a known-good
custom image. Weft infers this image for commands whose script name clearly
contains `sglang`, but declaring it in script metadata is more robust:

```python
# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = ["sglang[srt]>=0.4", "pynvml>=12.0"]
# [tool.weft]
# gpu = "ampere+>=24GB"
# image = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"
# min-driver = "535"
# min-cuda = "12.9"
# ///
```

If a vLLM or SGLang job fails with a missing framework package, missing
`libnuma`, or missing `flashinfer`, `weft status` and `weft info` include the
diagnosis and the runtime path to use on retry.

The `image` key specifies a Docker image for cloud execution. Image precedence
(highest to lowest): script `image` > matching `.weft.toml [cloud.image-overrides]`
entry > `.weft.toml [cloud] image` > auto-selected PyTorch image > global
default. Script metadata works with any command that references a `.py` file,
including `uv run script.py`, `python script.py`, and compound commands like
`pip install foo && python script.py`.

For custom or vendor-curated images, weft reads OCI image metadata and applies
NVIDIA `NVIDIA_REQUIRE_CUDA` constraints during cloud placement when it can
fetch the image config. You can also declare explicit floors:

```python
# /// script
# [tool.weft]
# image = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"
# min-driver = "535"
# cuda-driver-min = "12.9"
# image-pull-secret = "ghcr.io"
# ///
```

`min-driver` filters Vast.ai offers by NVIDIA driver version. `cuda-driver-min`
filters providers that report CUDA compatibility (`cuda_vers>=X.Y` on Vast.ai);
providers that do not expose CUDA/driver compatibility are treated as unknown
and lose to any known-compatible offer. If every available offer is unknown,
weft may still use one and will diagnose driver/runtime incompatibility if the
job fails. `image-pull-secret` names a configured `[registry]` entry; if it is
omitted, weft matches by the image registry hostname.

`cuda-driver-min` accepts a CUDA version (`"12.9"`), a torch wheel tag
(`"cu128"`), or a generation name (`"hopper"`, `"blackwell"`). Legacy key names
`min-cuda` / `min_cuda` remain accepted as synonyms; the equivalent CLI flag is
`--cuda-driver-min` on `weft run`.

When `uv.lock` or `pyproject.toml` pins torch/CUDA wheels, weft also infers a
provider CUDA floor from the wheel variant and NVIDIA CUDA package versions
(for example `nvidia-cusparse-cu12==12.8.x` implies `cuda-driver-min = "12.8"`).
Explicit `cuda-driver-min` values are still useful when the lockfile is
unavailable, when a script orchestrates an isolated venv that resolves torch on
the rental, or when custom runtime packages require a stricter floor. Prefer
the PEP 723 declaration over `--cuda-driver-min` on the CLI when the floor is
a permanent property of the script.

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

For cloud auto-placement, a pyproject range such as `torch>=2.5` is not enough
for compatibility inference unless it has been resolved into `uv.lock`.
Otherwise the rental can resolve a newer CUDA wheel than the lower bound
suggests. Run `uv lock`, use an exact torch dependency plus a CUDA-specific
`[tool.uv]` index or `cuda-driver-min`, or target an explicit `--host`.

If you've upgraded to wheels that *do* include newer arches but `uv.lock` hasn't
been refreshed, set `gpu-arch-max = "any"` for that script as a temporary
escape hatch.

### Avoiding GPU over-provisioning

The `gpu-mem` value is a **requested floor**. By default, weft adds a `+2GB`
safety margin for placement and queue admission checks. For example,
`gpu-mem = 8` is treated as an effective `10GB` requirement. Use strict mode
(`gpu-mem-strict = true` or `--gpu-mem-strict`) to keep exact matching.

**Hardware-ceiling values skip the +2GB headroom automatically.** When
`gpu-class`/`gpu` names a specific model and `gpu-mem` matches that model's
actual capacity (A100 80GB, H100 80GB, H200 141GB, RTX 4090 24GB, and other
known sizes), weft recognises the request as "I want this exact hardware"
and does not inflate the filter above the model's `gpu_ram`. So
`gpu-class = "a100"` + `gpu-mem = 80` and `gpu = "a100>=80GB"` produce the
same search — both target `gpu_ram>=80` and match A100 80GB offers. Without
this, adding the headroom would yield `gpu_ram>=82`, which excludes every
A100 80GB host because their `gpu_ram` reports exactly 80GB. The known-
hardware table lives in `internal/vastai/hardware_memory.go`.

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

### Running CPU-only scripts on cloud rentals

Vast.ai is a GPU marketplace — every offer ships with at least one GPU
attached. A "CPU-only" job that spills to a rental will still land on a
machine with a GPU, and a script that auto-detects CUDA
(`device = "cuda" if torch.cuda.is_available() else "cpu"` is the common
idiom) will happily start using it. Three failure modes follow:

1. **Old GPU rejects the workload.** Cheap offers often have Pascal (sm_61)
   or Volta (sm_70) cards. Recent PyTorch wheels (2.5+) drop support for
   sm_60/sm_61 and surface as
   `CUDA error: no kernel image is available for execution on the device`
   on the first tensor allocation. The `gpu-arch-max` weft auto-infers from
   your torch pin is an *upper* bound; it does not exclude too-old offers.
2. **CUDA wheel download eats the budget.** Linux torch wheels from PyPI
   include CUDA runtime and weigh ~2.5 GB. On a slow rental (Vietnam,
   intermittent peering) `uv sync` can blow past your max-time before the
   script ever starts. The job exits with code 124 and you pay for nothing.
3. **GPU rental wasted on CPU work.** Even when the script runs, you are
   paying GPU `$/hr` for code that never touches the GPU.

The `cpu-intensive` tag does NOT prevent GPU placement on rentals — it only
filters offers for adequate CPU cores (see [placement.md](placement.md)). To
actually run on CPU, hide the GPU from the script:

```python
# /// script
# [tool.weft]
# tags = ["cpu-intensive"]
# [tool.weft.env]
# CUDA_VISIBLE_DEVICES = ""
# ///
```

An empty `CUDA_VISIBLE_DEVICES` makes `torch.cuda.is_available()` return
`False`, so the standard auto-detect idiom falls through to CPU. This works
regardless of which GPU the rental ships with — Pascal, Volta, or H100. The
equivalent on the CLI is `--env CUDA_VISIBLE_DEVICES=`.

For a *one-off* CPU run inside a normally-GPU project, the CLI form is fine.
For a script whose intent is permanently CPU-only, put it in PEP 723 so
retries and re-submissions inherit it. If you want to be doubly explicit,
also override the device in the script itself:

```python
import os
os.environ.setdefault("CUDA_VISIBLE_DEVICES", "")
import torch
DEVICE = torch.device("cpu")
```

This still pays the CUDA wheel download cost. To avoid that as well, pin
torch to the CPU index in `pyproject.toml`:

```toml
[tool.uv.sources]
torch = [{ index = "pytorch-cpu" }]

[[tool.uv.index]]
name = "pytorch-cpu"
url = "https://download.pytorch.org/whl/cpu"
explicit = true
```

That cuts the wheel from ~2.5 GB to ~200 MB, which is what makes the slow-
rental scenario tolerable. It changes the lockfile though — if other
collaborators or environments need CUDA torch, isolate this in a CPU-only
lock or a uv group. `UV_TORCH_BACKEND=cpu` alone is **not** enough: uv
honors the lockfile over the env var, so a CUDA-pinned `uv.lock` will still
download the CUDA wheel.

**Steering placement toward a small instance.** The submission path will
reuse any rental whose GPU satisfies the job's constraints. A CPU-only job
with no GPU flag will happily queue behind big jobs on an existing A100.
Either let the autopilot pick this up (the cheapest instance wins on
$/hr), or pass `--gpu rtx_3090 --gpu-mem 4 --gpu-mem-strict` (or another
cheap class) plus `--tag rental` to force `weft start instance` onto a
fresh small box. Pick whichever cheap GPU class your provider has plenty
of offers for; the constraint exists only to lose the placement competition
with an expensive idle instance.

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
# Vast.ai
pip install vastai
vastai set api-key YOUR_API_KEY

# RunPod
brew install runpodctl
runpodctl doctor
weft runpod setup
```

### Using `weft start instance`

> **Check the autopilot first.** Run `weft autopilot status` — if it's
> `running`, you usually don't need to launch by hand: the autopilot will
> pick up unplaced jobs on its own. Manual launching is not faster, just
> more controllable (cost limits, offer selection, parallelism). See
> [Coordinating with the autopilot](instances.md#coordinating-with-the-autopilot).

`weft start instance` (also `weft start instances` and `weft instance
launch`) groups unplaced jobs by GPU requirements, searches enabled cloud
providers in parallel, and launches instances concurrently:

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
laptop$ weft start instance --runpod-cloud-type secure  # RunPod secure cloud for this launch
```

On a fresh config, Weft searches Vast.ai only. Enable RunPod with
`weft runpod setup` or `weft provider enable runpod`. RunPod launches use
community cloud by default; set `[runpod] cloud_type = "secure"` in
`~/.config/weft/config.toml` to make secure cloud the default.

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

`exclusive` and `benchmark-isolation` are part of the broader catalog of tags the
scheduler treats specially. See the
[Placement guide § Reserved tags](placement.md#reserved-tags) for the full
list and effects.

For reproducible benchmarking, the `benchmark-isolation` tag goes further — it waits for
the whole system (CPU, RAM, GPU, VRAM) to be idle before starting:

```
laptop$ weft run atlas \
  --tag benchmark-isolation \
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

### Mirroring SkyPilot jobs

Use `weft sky` when SkyPilot should own cloud resource selection and cluster
lifecycle, but you still want Weft's local job ledger, project views,
`--unprocessed` filtering, logs, cancel surface, and processed bookkeeping.

```bash
laptop$ weft sky submit --gpu a100 -m "SkyPilot training" 'python train.py'
# Job wj123 submitted to SkyPilot as 42

laptop$ weft sky sync
laptop$ weft job list --unprocessed
laptop$ weft log wj123
laptop$ weft job mark-processed wj123
```

To mirror a job that was submitted outside Weft:

```bash
laptop$ weft sky import --project calibration 42
```

SkyPilot jobs are external-executor jobs, not Weft rental instances. Weft does
not create `wi...` instance rows for them, run the Weft cloud agent, collect R2
outputs, apply grace periods, or report direct rental costs unless a future
adapter imports those signals explicitly.

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
`--produces` paths are uploaded to R2 with no size cap, so multi-GB
checkpoints, representation pkls, etc. are supported as artifact edges.

Each `--needs` spec names a single file (`path:<job-id>`), not a directory.
Directory targets such as `--needs output/:1046` are not supported — give each
file its own `--needs` flag.

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
- **Data locality**: HF inputs already on a host earn a bonus; missing HF data
  incurs a transfer penalty. `checkpoint:` and `corpus:` inputs require a host
  that already has the asset.
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
