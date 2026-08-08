# Weft

[![Go Reference](https://pkg.go.dev/badge/github.com/osteele/weft.svg)](https://pkg.go.dev/github.com/osteele/weft)
[![Go Report Card](https://goreportcard.com/badge/github.com/osteele/weft)](https://goreportcard.com/report/github.com/osteele/weft)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Weft runs GPU research jobs across lab workstations, shared servers, borrowed
machines, and temporary Vast.ai or RunPod rentals. It records each job in a
local ledger, chooses a compatible host, transfers source and declared inputs,
and dispatches the work through a remote Go agent.

Operators can use the TUI, watch views, browser dashboard, or CLI. The CLI also
supports scripts and coding agents with bounded waits, JSON and JSONL output,
stable job IDs, and exit codes tied to job results.

Hosts can have independent filesystems and belong to different administrative
domains. Weft syncs the working tree, pre-stages declared inputs, uses persistent
host caches, and collects outputs into per-job artifacts. A shared filesystem is
optional.

Weft has a single-operator control plane. Its job ledger, credentials, and
placement policy belong to one user. On shared hosts, Weft observes work it did
not launch, avoids overloaded machines, and can wait for whole-host quiescence
before benchmarks. These controls help it coexist with other users, but they are
cooperative rather than enforced.

Multi-user accounts, fairshare, quotas, resource enforcement, and multi-node
MPI scheduling remain outside Weft's scope. See
[Comparison to SLURM](docs/design/comparison-to-slurm.md) and
[Comparison to SkyPilot](docs/design/comparison-to-skypilot.md) for the design
boundaries.

## Job Lifecycle

| Stage | What Weft handles |
| --- | --- |
| Submit | Records a durable job ID before network activity. Accepts CLI flags, PEP 723 script metadata, dependencies, and YAML plans. |
| Place | Scores inventory hosts, active rentals, and new offers. Placement includes compatibility, queue delay, data locality, transfer time, runtime, cost, and rental survival history. |
| Stage | Moves the working tree and declared data to the selected filesystem. Inputs can include local files, Hugging Face repositories, named assets, and prior job outputs. |
| Run | Uses persistent queues and Go agents. Jobs, dependencies, telemetry, and uploads continue after the laptop sleeps or SSH disconnects. |
| Observe | Reports progress, logs, telemetry, placement reasons, launch phases, costs, and failures through terminal, machine-readable, notification, and web surfaces. |
| Recover | Defers operations while hosts are offline and replaces failed rentals. Handles Vast.ai pause/resume bidding, restores streamed outputs on relaunch, and limits runaway retries. |
| Reuse | Tracks conventional and declared outputs by job and attempt. Preserves source snapshots, attempt history, and producer-to-consumer edges. |

Placement includes compatibility and measurement constraints. Weft derives
driver, CUDA, GPU architecture, and cloud-image requirements from the project's
`torch` pin. Benchmark isolation gates and per-run telemetry support controlled
GPU, model, and configuration sweeps.

## Interfaces

| TUI and visual views | CLI for humans and agents |
| --- | --- |
| TUI, grouped jobs, watch views, dashboards, logs, and keyboard actions | CLI commands, bounded waits, JSON/JSONL, plans, and lifecycle operations |
| Inspect placement, progress, telemetry, cost, and artifacts in one terminal | Run commands or wait for recorded job state without polling SSH processes or provider CLIs |
| Start, move, retry, cancel, cordon, or inspect work interactively | Use stable job IDs, result-sensitive exit codes, bounded waits, and diagnostic output |

The TUI and CLI call the same core operations, so an interactive action and its
command-line equivalent have the same validation and reconciliation semantics.
Scripts and agents can submit work, record the job IDs, wait for terminal state
or receive a channel notification, inspect failures, and retrieve artifacts.
They do not have to infer job health from local process lists or open-ended
polling loops.

```bash
weft status wj42 --wait --wait-timeout 30m
weft job list --format json
weft watch --plain --transitions-only --jsonl
weft autopilot status --quiet
```

## Quick Start

```bash
go install github.com/osteele/weft@latest
```

Or build from source:

```bash
git clone https://github.com/osteele/weft.git
cd weft
go install .
```

Remote hosts need SSH access and `tmux`. Cloud workflows also need the provider
CLI and credentials for the provider you use. See
[Cloud GPU Instances](docs/guides/instances.md) for setup.

Submit a job and let Weft choose a host:

```bash
weft run --gpu "nvidia>=24GB" -m "Train" 'uv run python train.py'
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
  --gpu "nvidia>=24GB" \
  'python train.py'
```

Check or fetch Hugging Face assets:

```bash
weft data where hf:meta-llama/Llama-3-8B
weft data fetch hf:meta-llama/Llama-3-8B --host cool100
```

Launch cloud capacity for unplaced jobs:

```bash
weft start instance
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

## An Example Session

The terminal captures below show an example workflow.

Submit a job without choosing a host. The GPU and input declarations give the
placer enough information to select a compatible machine with a useful cache:

```console
$ weft run \
    --gpu "nvidia>=24GB" \
    --input hf:EleutherAI/pythia-160m \
    -m "Pythia learning-rate sweep" \
    'uv run python train.py --lr 3e-4'
Job #1842 accepted and queued on atlas
  Normal range: inventory dispatch usually starts within one daemon sync after the target is free
  Action: wait; no manual retry or kill indicated, monitor with weft status wj1842 --wait

$ weft status wj1842
Job ID:   wj1842
Host:     atlas
Status:   running
Running:  12m18s
Details:  weft info wj1842  # Show directory, command, env vars
```

For interactive supervision, `weft tui` groups the fleet by lifecycle state and
keeps actions, target details, progress, and fleet health in one view:

```console
$ weft tui
Jobs | group=status | project=vision-lab

Running (2)
> wj1842  atlas  running 63%  Pythia learning-rate sweep       elapsed 12m
  wj1843  wi92   running 41%  Train 13B control               elapsed 7m

Unplaced (1)
  wj1844  waiting            Evaluate H100 baseline           hopper+ >=80GB

Completed (1)
  wj1841  atlas  completed   Prepare tokenized corpus         29m ago

--------------------------------------------------------------------------------
Job:  wj1842 | running 63% | atlas GPU 0 | elapsed 12m
Host: atlas | A100 80GB | GPU 91% | VRAM 18.6/80.0GB
Fleet: 2 running | 1 unplaced | rental rate $1.42/hr
A:auto (ON)  r:refresh  a:attempts  x:kill  /:project  i:instances  ?:help  q:quit
```

`weft job list --plain` exposes the same ledger as a script-friendly snapshot.
Running `weft job list` in an interactive terminal opens the live list instead.

```console
$ weft job list --plain
       ID HOST          STATUS        TIME        PROJECT     DESCRIPTION
    wj1844 (unplaced)   waiting       -           vision-lab  Evaluate H100 baseline
    wj1843 wi92         queued        -           vision-lab  Train 13B control
    wj1842 atlas        running       08/08 14:21 vision-lab  Pythia learning-rate sweep
    wj1841 atlas        completed ok  08/08 14:07 vision-lab  Prepare tokenized corpus
```

The system watch combines rental instances, inventory hosts, and unplaced jobs.
This is one frame from the live display:

```console
$ weft watch
=== System Watch 2026-08-08 14:36:10 ===

RENTAL INSTANCES (1)
  Summary: cost: $0.18  current rate: $1.42/hr

Instance wi92 - A100 80GB - running
  vastai: 28740123 (running)
  GPU: A100, 80GB per GPU, CUDA 12.4
  Cost: $0.18 (uptime: 7m36s, rate: $1.42/hr)
  Jobs: 0/1 resolved
    1843  running 41%   vision-lab  Train 13B control

INVENTORY HOSTS (1 active)
  atlas  1 running

UNPLACED JOBS (1)
  #1844  vision-lab  Evaluate H100 baseline  hopper+ >=80GB
```

Progress written by the job appears in logs and in the terminal views. Files
under `output/` or `outputs/` are collected automatically:

```console
$ weft log wj1842 -f
loading EleutherAI/pythia-160m
Progress: 1/3
epoch=1 validation_loss=3.84
Progress: 2/3
epoch=2 validation_loss=3.57
Progress: 3/3
wrote output/metrics.json

$ weft artifact list wj1842
-  output/checkpoints/best.pt  483183820  7e45e8a51a1fcb2b78774a4d9ac1e09e4d972668ad751d7284f392f73f21564d
-  output/metrics.json         1842       5f121fb821a89c9b5a92b5615fd924fd3c45b3d14f3a8e3c2f9a887f12e6a390

$ weft artifact get wj1842 output/metrics.json -o ./metrics.json
Wrote ./metrics.json
```

## Architecture

```
Human TUI / dashboards             CLI / JSON / coding agents
Control: local job ledger + shared core
Actions: placement | sync | provider control
Targets: inventory hosts | active rentals | new cloud offers
Remote: durable Go host and instance agents
Results: logs | telemetry | costs | artifacts
```

The control machine stores intent and history in a local SQLite ledger. It
syncs source and data into each target's own filesystem, dispatches work over
SSH, and reconciles remote state when connectivity is available. Remote agents
own the active queues and job processes, so dispatched work does not depend on
an always-connected laptop or a central cluster controller.

See [Architecture](docs/design/architecture.md) and
[Comparison to SLURM](docs/design/comparison-to-slurm.md) for design details.
The [SkyPilot comparison](docs/design/comparison-to-skypilot.md) explains the
tradeoffs between Weft's on-prem and marketplace focus and SkyPilot's broader
managed-cloud substrate.

## Monitoring

Weft provides several views depending on what you need to watch:

| Command | Use |
| --- | --- |
| `weft tui` | Full-screen jobs and hosts dashboard |
| `weft dashboard` | Tabbed summary dashboard (Pulse, Timeline, Fleet, Focus, Tree, Alerts, History, Flow, Cost, Usage); see [Dashboard](docs/guides/dashboard.md) |
| `weft watch` | System-wide view of cloud instances, on-prem jobs, and unplaced jobs |
| `weft instance watch [id]` | Live cost, phase, and job progress for a cloud launch batch |
| `weft project watch [name]` | Project-scoped instances and unplaced jobs |
| `weft web --open` | Local read-only browser dashboard |

The terminal dashboards can run autopilot in the background. Autopilot places
unplaced jobs, launches cloud instances, and relaunches orphaned jobs within
configured guardrails. See [Autopilot](docs/guides/autopilot.md) and
[Campaigns](docs/guides/campaigns.md).

For a macOS menu bar and floating HUD view of current activity, see
[Weft Status](https://github.com/osteele/weft-status).

## Data and Artifacts

Weft treats data movement as part of the job contract. The selected machine
does not need to see the laptop's paths through a shared mount:

- The working tree is synced to inventory hosts or captured as an immutable,
  content-addressed source snapshot for rentals. Uncommitted edits are included.
- Typed inputs such as `hf:`, `hf-dataset:`, `asset:`, and upstream job outputs
  affect placement, disk estimates, transfer estimates, and pre-staging.
- Files written under `output/` or `outputs/` are discovered automatically.
  `--produces` and other explicit declarations give outputs a stable per-job,
  per-attempt identity.
- Rental agents stream outputs to R2 during execution and drain remaining
  uploads before teardown. On-prem hosts can keep per-run snapshots and sync
  them into the control machine's artifact cache.
- `--needs PATH:JOB_ID` connects a downstream job to an exact producer. Weft
  can keep related jobs on one live rental or stage the recorded artifact before
  the consumer starts.

Artifacts can be listed, streamed, downloaded, or fed into another job without
embedding files in logs or manually copying them between machines:

```bash
weft run --produces output/model.pt -m "Train" 'uv run python train.py'
weft run --needs output/model.pt:1842 -m "Evaluate" 'uv run python eval.py'

weft artifact list wj1842
weft artifact cat wj1842 output/metrics.json | jq .validation_loss
weft artifact get wj1842 output/model.pt -o ./model.pt
```

See [Artifact Store](docs/design/artifacts.md) for the durability and retrieval
model.

## Cloud Bursting

When local hosts cannot run a job, Weft can search cloud GPU providers, upload a
source snapshot to R2, start an instance, run the queued jobs, collect telemetry,
and shut the instance down. Failed cloud jobs enter a grace period so they can
be inspected or resubmitted before cleanup.

Weft keeps placement, source sync, execution state, logs, artifact discovery,
and teardown records in one local job ledger. There is no separate managed-jobs
controller or controller log stream between the job record and worker setup.
Use `weft info <job-id>` and `weft instance audit <job-id>` for lifecycle and
cleanup evidence.

Jobs tagged `interruptible` may run on Vast.ai interruptible offers. Weft treats
provider outbids as pause/resume events when Vast retains the instance disk. It
can raise bids up to the recorded on-demand reference. If the job must relaunch
on a new instance, Weft restages previously uploaded `output/` files from R2.

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

## Documentation

Start with [docs/README.md](docs/README.md) for the documentation map.

- [Workflow Guide](docs/guides/workflow-guide.md) for common usage patterns.
- [Cloud GPU Instances](docs/guides/instances.md) for Vast.ai and RunPod setup.
- [Autopilot](docs/guides/autopilot.md) and
  [Campaigns](docs/guides/campaigns.md) for cloud launch automation.
- [Command Reference](docs/reference/commands.md) for CLI syntax.
- [Agent-Oriented Workflows](docs/guides/agent-workflows.md) for bounded waits,
  machine-readable output, notifications, and agent-safe lifecycle operations.
- [Artifact Store](docs/design/artifacts.md) for output capture, retrieval, and
  producer-to-consumer staging.
- [Comparison to SLURM](docs/design/comparison-to-slurm.md) and
  [Comparison to SkyPilot](docs/design/comparison-to-skypilot.md) for the design
  boundaries and tradeoffs.
- [Network Resilience](docs/guides/network-resilience.md) for offline and
  flaky-network behavior.
