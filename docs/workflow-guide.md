# Weft Workflow Guide

This guide walks through common workflows with complete examples drawn from real
usage patterns. Each section shows how weft's features work together in practice.

## Quick orientation

Weft manages jobs on remote GPU hosts. You submit from your laptop, and the
remote queue runner handles execution even while your laptop sleeps.

```
laptop$ weft run cool30 'uv run python train.py'
# Job 100 queued on cool30
# Next: weft tui (or weft log 100)
```

If you omit the host, weft picks one automatically based on GPU requirements,
data locality, and current load:

```
laptop$ weft run --gpu-class a100 'uv run python train.py'
# Auto-placed on cool100 (gpu_class match, 2 queued jobs)
```

## Declaring data dependencies

Jobs that need HuggingFace models, datasets, or other data assets should declare
them with `--input`. The coordinator uses these declarations to:

1. **Pick the best host** — prefer hosts that already have the data cached
2. **Pre-stage data** — rsync assets from another host if needed
3. **Download automatically** — fetch HF models/datasets if no host has them yet

```
laptop$ weft run \
  --input hf:EleutherAI/pythia-160m \
  --input hf-dataset:wikitext \
  -m "EXP-010: Developmental dynamics (Pythia-160M)" \
  'uv run python scripts/developmental_dynamics.py \
    --checkpoints 64,512,4000,32000,143000 --device cuda'
# Auto-placed on cool100 (pythia-160m cached there)
```

If the model is on cool100 but not cool30, the coordinator places the job on
cool100. If neither host has it, the coordinator downloads it before the job
starts. You never need a separate prefetch job.

### Declaring outputs

Use `--output` to declare what a job produces. This lets downstream jobs find
the data and lets the coordinator pre-stage it to the right host:

```
laptop$ weft run cool30 \
  --output file:~/outputs/traces/entropy-sweep/ \
  -m "Generate entropy traces" \
  'uv run python scripts/generate_traces.py --output ~/outputs/traces/entropy-sweep/'
# Job 4674 queued

laptop$ weft run cool100 \
  --after 4674 \
  --input file:~/outputs/traces/entropy-sweep/ \
  -m "Analyze traces on A100" \
  'uv run python scripts/analyze_traces.py --input ~/outputs/traces/entropy-sweep/'
# Job 4675 queued (depends on 4674)
# Coordinator rsyncs traces from cool30 → cool100 before starting
```

The coordinator handles the cross-host rsync automatically. You declare what the
job needs and what it produces; the coordinator figures out the rest.

### Checking data locality

See what's cached where:

```
laptop$ weft host data
HOST     ASSET                           SIZE
cool30   hf:meta-llama/Llama-3-8B        15.2 GB
cool100  hf:meta-llama/Llama-3-8B        15.2 GB
cool100  hf:EleutherAI/pythia-160m       312 MB
cool100  hf-dataset:wikitext             512 MB
```

## Ablation sweep with a fan-out dependency chain

A common ML research pattern: run one baseline configuration, then fan out
multiple ablation variants that each depend on the baseline completing first
(e.g., to reuse a shared cache, confirm the pipeline works, or serialize GPU
access).

### Baseline first

```
laptop$ weft run cool100 --gpu 1 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: Unpruned + LoRA control (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion unpruned'
# Job 4823 queued
```

### Fan out ablations with `--after`

```
laptop$ weft run cool100 --gpu 1 --after 4823 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: Reverse L1 (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion reverse_l1'
# Job 4828 queued (depends on 4823)

laptop$ weft run cool100 --gpu 1 --after 4828 \
  --tag markov-attention --tag EXP-112 \
  -m "EXP-112: Gamma + Alpaca (GPU 1)" \
  'uv run python -u scripts/exp112_pruning_lora_recovery.py \
    --device cuda --criterion gamma'
# Job 4829 queued (depends on 4828)

laptop$ weft run cool100 --gpu 1 --after 4829 \
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
  weft run cool100 --gpu 1 --after 4830 \
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
laptop$ weft run cool100 --after-any 4830 \
  -m "Summarize EXP-112 results" \
  'uv run python scripts/summarize_exp112.py'
```

## Running parallel experiments across GPUs

On a multi-GPU host like cool100 (2x A100 + 8x RTX 2080 Ti), you can run
independent experiments in parallel by pinning them to different GPUs.

### Two independent signal sweeps, one per A100

```
# GPU 0: entropy-based signals
laptop$ weft run cool100 --gpu 0 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: entropy signal (A100 GPU 0)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal entropy --dtype bfloat16 --resume'
# Job 4822 queued

laptop$ weft run cool100 --gpu 0 --after 4822 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: entropy_delta signal (A100 GPU 0)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal entropy_delta --dtype bfloat16 --resume'
# Job 4825 queued (depends on 4822)

# GPU 1: different signals in parallel
laptop$ weft run cool100 --gpu 1 \
  --tag adaptive-escalation --tag EXP-005 \
  -m "EXP-005: random signal baseline (A100 GPU 1)" \
  'uv run python scripts/run_backtracking_search.py \
    --signal random --dtype bfloat16 --resume'
# Job 4834 queued (runs simultaneously with 4822)

laptop$ weft run cool100 --gpu 1 --after 4834 \
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
      weft run cool30 \
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
      weft run cool30 \
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

### Iterate on cool30 (RTX 3090)

```
laptop$ weft run cool30 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation: single config (12h, 100 GFLOPs) to test pipeline" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 100 --device cuda'
# Job 4768 completed (exit 1 — found a bug)

laptop$ weft run cool30 --after 4768 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation v2: 12h, 500 GFLOPs, attentions fix" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 500 --device cuda'
# Job 4773 completed (exit 1 — another issue)

laptop$ weft run cool30 --after 4773 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation v3: fingerprint fix" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 500 --device cuda'
# Job 4777 completed (exit 1 — still not right)
```

### Once the pipeline works, move to cool100 for the full sweep

```
laptop$ weft run cool100 --gpu 1 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 validation v4: head_count_sweep (cool100 GPU 1)" \
  'uv run python scripts/head_count_sweep.py --design A --n-heads 12 \
    --target-gflops 500 --device cuda'
# Job 4784 completed (exit 0 — success!)

laptop$ weft run cool100 \
  --tag head-type-ontology --tag EXP-013 \
  -m "EXP-013 full sweep Design A: 4,8,12,24,48 heads" \
  'uv run python scripts/head_count_sweep.py --design A --device cuda'
# Job 4798 queued

laptop$ weft run cool100 \
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
Use 'weft campaign launch' or press 'c' in the TUI to launch on a cloud GPU.
```

The job appears in the TUI with status `$ needs rental`. From there, press `c`
to open the cloud menu for a single job, or use `weft campaign launch` to batch-
launch all `needs_rental` jobs at once.

### Prerequisites

```
pip install vastai
vastai set api-key YOUR_API_KEY
```

### Using `weft campaign launch`

The campaign launcher groups `needs_rental` jobs by GPU requirements, searches
for Vast.ai offers in parallel, and launches instances concurrently:

```
laptop$ weft campaign launch
```

The interactive TUI shows jobs grouped by GPU class with checkboxes. Deselect
jobs you don't want to launch, review cost estimates, and press Enter. Weft
creates a campaign (batch record), provisions one instance per GPU group **in
parallel**, then segues into watch mode.

```
laptop$ weft campaign launch --dry-run    # Preview without launching
laptop$ weft campaign launch --no-watch   # Launch and exit immediately
```

After launch, monitor and manage:

```
laptop$ weft campaign watch <id>          # Live status updates
laptop$ weft campaign list                # List campaigns
laptop$ weft campaign show <id>           # Campaign details
laptop$ weft campaign terminate <id>      # Destroy all instances
laptop$ weft instance ssh <id>            # SSH into an instance
```

See [docs/campaigns.md](campaigns.md) for the full campaign guide.

### Using the TUI cloud menu (single job)

For launching a single job from the TUI:

1. Open the TUI: `weft tui`
2. Navigate to a **queued** or **needs rental** job
3. Press `c` to open the cloud GPU menu

```
┌─ Send to Cloud GPU ──────────────────────────────────────────┐
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
laptop$ weft run cool100 \
  --tag exclusive \
  -m "CUDA multistream ANS (actual size)" \
  'uv run compression-lab gpu-bench --codec ans --multistream'
```

The queue runner waits until all other jobs finish, runs the exclusive job alone,
then resumes normal scheduling.

For reproducible benchmarking, the `benchmark` tag goes further — it waits for
the whole system (CPU, RAM, GPU, VRAM) to be idle before starting:

```
laptop$ weft run cool100 \
  --tag benchmark \
  -m "Measure throughput at batch_size=64" \
  'uv run python benchmark.py --batch 64'
```

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
# Auto-placed on cool100 (gpu_class: a100, data: local, queue: 2 jobs)
```

See what weft knows about your hosts:

```
laptop$ weft host list
laptop$ weft host data
```

## Draft jobs: plan now, run later

When iterating on a command and not ready to submit:

```
laptop$ weft run --draft cool30 \
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
laptop$ weft run cool30 'uv run python train.py'
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
