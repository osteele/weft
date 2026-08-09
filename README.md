# Weft

[![Go Reference](https://pkg.go.dev/badge/github.com/osteele/weft.svg)](https://pkg.go.dev/github.com/osteele/weft)
[![Go Report Card](https://goreportcard.com/badge/github.com/osteele/weft)](https://goreportcard.com/report/github.com/osteele/weft)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Weft is a workload runner for GPU research and engineering clusters. It lets you
submit jobs from a laptop, place them on local or cloud GPU hosts, monitor them
from a terminal UI, and keep going when SSH drops or the laptop disconnects.

It is designed for small teams and personal clusters that need more automation
than ad hoc SSH scripts, without adopting a heavyweight HPC scheduler.

## Highlights

- Places jobs automatically by GPU class, memory, queue depth, data locality,
  and estimated resource use.
- Matches jobs to compatible hosts and cloud offers by NVIDIA driver, CUDA, and
  GPU compute-capability floors inferred from your `torch` pin and curated
  library requirements — and picks (and merges) the Docker image for cloud jobs
  from the same package analysis, so you rarely specify an image by hand.
- Supports reproducible benchmarking: isolation tags keep a measured job off
  shared or busy hardware, and per-job CPU/GPU telemetry is recorded for
  performance analysis across GPU, model, and configuration sweeps.
- Detects overloaded on-prem hosts, drains movable jobs to healthier local or
  rental capacity, and marks overloaded hosts/jobs in terminal dashboards.
- Runs jobs in durable remote sessions managed by a Go agent.
- Tracks logs, exit status, progress, artifacts, and job metadata locally.
- Submits jobs locally first; daemon/autopilot placement and SSH dispatch keep
  submission responsive even when hosts are slow to probe.
- Bursts to Vast.ai or RunPod when local machines are full.
- Keeps cloud job control in Weft's own job ledger instead of adding a separate
  managed-jobs controller layer.
- Supports Vast.ai interruptible instances: outbid pauses are recoverable
  same-instance stops, bids can be raised within an on-demand cap, and
  R2-streamed outputs are restored if a job relaunches elsewhere.
- Treats project outputs as first-class artifacts: write under `output/` or
  `outputs/`, or declare extra outputs, then list and retrieve them with
  `weft artifact`.
- Shows active jobs, queues, cloud instances, and hosts from terminal and web
  views.
- Keeps source snapshots for cloud jobs so completed remote runs can be
  inspected later.

## Quick Start

```bash
go install github.com/osteele/weft@latest
```

Or build from source:

```bash
jj git clone https://github.com/osteele/weft
cd weft
go install .
```

Remote hosts need SSH access and `tmux`. Cloud workflows also need the provider
CLI and credentials for the provider you use. See
[Cloud GPU Instances](docs/guides/instances.md) for setup.

Submit a job and let Weft choose a host:

```bash
weft run --gpu-class a100 -m "Train" 'uv run python train.py'
```

Watch it from the terminal:

```bash
weft log wj42 -f
weft job list --running
weft tui
```

Declare data dependencies so placement can prefer hosts that already have the
inputs:

```bash
weft run \
  --input hf:meta-llama/Llama-3-8B \
  --gpu-class a100 \
  'python train.py'
```

Check or fetch Hugging Face assets:

```bash
weft data where hf:meta-llama/Llama-3-8B
weft data fetch hf:meta-llama/Llama-3-8B --host cool100
```

Launch cloud capacity for unplaced jobs:

```bash
weft instance launch
weft instance watch
```

Inspect the exact source snapshot used by a cloud job:

```bash
weft source ls wj42
weft source cat wj42 scripts/train.py
```

Job IDs appear as `wj<N>` in output. Commands accept both `wj42` and `42`, and
many commands accept ranges such as `wj42:47`.

The full CLI reference is in [Command Reference](docs/reference/commands.md).
For task-oriented examples, start with the
[Workflow Guide](docs/guides/workflow-guide.md).

## How It Fits Together

```
Laptop CLI / TUI
   |
   | placement, sync, queue dispatch, status, logs
   v
Remote host agents
   | tmux jobs, GPU allocation, logs, telemetry
   v
On-prem or cloud GPU hosts
```

The legacy coordinator daemon is deprecated. The CLI/TUI owns placement and
sync locally, queues work directly over SSH, and relies on remote agents for
durable execution after the submitting laptop disconnects.

See [Architecture](docs/design/architecture.md) and
[Comparison to SLURM](docs/design/comparison-to-slurm.md) for design details.

## Monitoring

Weft provides several views depending on what you need to watch:

| Command | Use |
| --- | --- |
| `weft tui` | Full-screen jobs and hosts dashboard |
| `weft dashboard` | Tabbed at-a-glance dashboard (Pulse, Timeline, Fleet, Focus, Tree, Alerts, History, Flow, Cost, Usage) — see [Dashboard](docs/guides/dashboard.md) |
| `weft watch` | System-wide view of cloud instances, on-prem jobs, and unplaced jobs |
| `weft instance watch [id]` | Live cost, phase, and job progress for a cloud launch batch |
| `weft project watch [name]` | Project-scoped instances and unplaced jobs |
| `weft web --open` | Local read-only browser dashboard |

The terminal dashboards can run autopilot in the background. Autopilot places
unplaced jobs, launches cloud instances, and relaunches orphaned jobs within
configured guardrails. See [Autopilot](docs/guides/autopilot.md) and
[Campaigns](docs/guides/campaigns.md).

For a native macOS menu bar and floating HUD view of current activity, see
[Weft Status](https://github.com/osteele/weft-status).

## Cloud Bursting

When local hosts cannot run a job, Weft can search cloud GPU providers, upload a
source snapshot to R2, start an instance, run the queued jobs, collect telemetry,
and shut the instance down. Failed cloud jobs enter a grace period so they can
be inspected or resubmitted before cleanup.

This native path is intentionally lighter than delegating normal experiments to
an external managed-job controller. Weft owns placement, source sync, execution
state, logs, artifact discovery, and teardown records in one local job ledger,
so there is no separate AWS/controller credential path to set up and no
controller log stream to chase before you reach worker setup or user-code logs.
Use `weft info <job-id>` and `weft instance audit <job-id>` for lifecycle and
cleanup evidence.

Jobs tagged `interruptible` may run on Vast.ai interruptible offers. Weft treats
provider outbids as pause/resume events when Vast retains the same instance disk,
raises bids up to the recorded on-demand reference when possible, and restages
previously uploaded `output/` files from R2 if the job has to relaunch on a new
instance.

For result files, the intended path is to write into `output/` or `outputs/`
and use `weft artifact list|get|sync`. That avoids log-embedded tarballs or
manual file-copy conventions for ordinary smoke tests and experiment outputs.

Provider setup and operation are covered in
[Cloud GPU Instances](docs/guides/instances.md). The command details live in
[Command Reference](docs/reference/commands.md).

## Data Locality And Estimation

Placement can use declared inputs, known host caches, transfer history, and
resource prediction. Weft can pre-stage missing inputs, download declared
Hugging Face assets, and learn transfer bandwidth from observed copies. Runtime
and memory estimates are trained from historical job data and feed placement and
cloud cost planning.

See [Job Plans](docs/reference/job-plans.md),
[Estimation and Modeling](docs/reference/estimation.md), and
[Logging and Progress](docs/reference/logging-and-progress.md).

## Configuration

Configuration lives in `~/.config/weft/config.toml`. Project-specific sync and
cloud overrides can be placed in `$PROJECT/.weft.toml`.

Minimal examples:

```toml
default_command = "tui"

[cloud.ssh]
identity_file = "~/.ssh/weft_cloud_ed25519"
public_key_file = "~/.ssh/weft_cloud_ed25519.pub"

[sync]
exclude_dirs = ["data", "runs"]

[runpod]
# cloud_type = "secure" # default is "community"
```

Use `.weft.toml` for project-level source excludes, output directories, cloud
image overrides, and runtime disk requirements. Use the provider guides for
Vast.ai, RunPod, R2, registry credentials, retry limits, and cloud SSH setup.

## Learn More

Start with [docs/README.md](docs/README.md) for the full documentation map.

- [Workflow Guide](docs/guides/workflow-guide.md) for common usage patterns.
- [Cloud GPU Instances](docs/guides/instances.md) for Vast.ai and RunPod setup.
- [Autopilot](docs/guides/autopilot.md) and
  [Campaigns](docs/guides/campaigns.md) for cloud launch automation.
- [Command Reference](docs/reference/commands.md) for complete CLI syntax.
- [Network Resilience](docs/guides/network-resilience.md) for offline and
  flaky-network behavior.
