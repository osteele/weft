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

Multi-user accounts, fair-share scheduling, quotas, resource enforcement, and
multi-node MPI scheduling remain outside Weft's scope. See
[Comparison to SLURM](docs/design/comparison-to-slurm.md) and
[Comparison to SkyPilot](docs/design/comparison-to-skypilot.md) for the design
boundaries.

## Operating Model

The **control machine** is the computer where you run the Weft CLI; it owns the
job ledger and placement policy. An **inventory host** is a workstation or
server you already control, normally named by an SSH target. A **rental** is a
temporary Vast.ai or RunPod instance. Each execution host runs a persistent
Weft **agent** that owns its queue and job processes.

At submission, Weft records an immutable **source closure**: the working tree
and any local path dependencies needed by the job. **R2** is the object store
used for rental data exchange and for inventory hosts that can reach R2 but
cannot accept inbound SSH; it is optional when every inventory host uses direct
SSH. **Autopilot** is the local daemon component that places unassigned jobs and
manages rental launches. See
[Architecture](docs/design/architecture.md) and
[Network Resilience](docs/guides/network-resilience.md) for the detailed paths.

## Job Lifecycle

| Stage | What Weft handles |
| --- | --- |
| Submit | Records a durable job ID before network activity. Accepts CLI flags, PEP 723 script metadata, dependencies, and YAML plans. |
| Place | Scores inventory hosts, active rentals, and new offers. Placement includes compatibility, queue delay, data locality, transfer time, runtime, cost, and rental survival history. |
| Stage | Moves the working tree and declared data to the selected filesystem. Inputs can include local files, Hugging Face repositories, named assets, and prior job outputs. |
| Run | Uses persistent queues and Go agents. Jobs, dependency checks, and telemetry continue after the laptop sleeps or SSH disconnects. Cloud agents also continue R2 uploads; on-prem outputs sync after the control machine reconnects. |
| Observe | Reports progress, logs, telemetry, placement reasons, launch phases, costs, and failures through terminal, machine-readable, notification, and web surfaces. |
| Recover | Defers operations while hosts are offline and replaces failed rentals. Handles Vast.ai pause/resume bidding, restores streamed outputs on relaunch, and limits runaway retries. |
| Reuse | Tracks conventional and declared outputs by job and attempt. Preserves source snapshots, attempt history, and producer-to-consumer edges. |

Placement includes compatibility and measurement constraints. Weft derives
driver, CUDA, GPU architecture, and cloud-image requirements from the project's
`torch` pin. Benchmark isolation gates and per-run telemetry support controlled
GPU, model, and configuration sweeps.

An inventory host can run several jobs concurrently when its CPU, RAM, and GPU
admission gates allow it. The queue runner adapts CPU allotments from observed
process-tree use and combines live RAM use with declared or predicted per-job
reservations, preventing concurrent model-load commitments from exceeding its
host target. See
[Host-local RAM admission](docs/architecture/ram-admission.md).

## Interfaces

Interactive and automated interfaces call the same core operations and use the
same job ledger:

| Interface | Use |
| --- | --- |
| `weft tui` | Operate jobs and inspect hosts in a full-screen terminal UI |
| `weft dashboard` | Monitor pulse, fleet, cost, usage, history, and alerts; see [Dashboard](docs/guides/dashboard.md) |
| `weft watch` | Watch rental instances, on-prem jobs, and unplaced jobs together |
| `weft instance watch [id]` | Follow cost, launch phase, and job progress for cloud instances |
| `weft web --open` | Open the local read-only browser dashboard |
| [Weft Status](https://github.com/osteele/weft-status) | Show current activity in a macOS menu bar and floating HUD |
| CLI with JSON or JSONL | Submit and inspect work from scripts or coding agents |

Scripts and agents can wait for terminal state, receive notifications, inspect
failures, and retrieve artifacts through stable job IDs and result-sensitive
exit codes.

```bash
weft status wj42 --wait --wait-timeout 30m
weft job list --format json
weft watch --plain --transitions-only --jsonl
weft autopilot status --quiet
```

## Quick Start

Install Git and Go 1.25.7 or newer. Ensure Go's binary directory is on your
`PATH`; for the current shell:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

```bash
go install github.com/osteele/weft@latest
```

Or build from source:

```bash
git clone https://github.com/osteele/weft.git
cd weft
go install .
```

Verify the installation. A binary installed with the commands above currently
identifies itself as a development build:

```console
$ weft version
weft dev
```

Set up each on-prem host before submitting work. In this guide, `atlas` is an
example SSH target; replace it with a hostname or alias from `~/.ssh/config`.
Confirm that key-based SSH works first:

```bash
ssh atlas
```

The remote account needs permission to install packages through `apt-get` and
`sudo` on Linux or Homebrew on macOS. If the required tools are already present,
use `--skip-prerequisites`. Setup records the host's hardware, deploys the agent,
and starts its queue runner:

```console
$ weft host setup atlas
...
Setup complete. Host atlas is ready.
```

Cloud workflows need the provider CLI and credentials for the provider you use.
See [Cloud GPU Instances](docs/guides/instances.md) for setup.

Submit a dependency-free smoke job to that host. Weft prints a durable job ID;
use that ID in later commands:

```console
$ weft run --host atlas -m "Smoke test" 'printf "hello from weft\n"'
Job #123 queued on atlas

$ weft status wj123 --wait
Job ID:   wj123
Host:     atlas
Status:   completed
...
Exit:     0

$ weft log wj123
hello from weft
```

Replace `wj123` with the ID printed by your submission. For longer jobs, use the
live log, job list, or TUI:

```bash
weft log wj123 -f
weft job list --running
weft tui
```

Once the smoke job works, omit `--host` to let Weft choose a compatible target.
For example, this requests an NVIDIA GPU with at least 24 GB of VRAM:

```bash
weft run --gpu "nvidia>=24GB" -m "Train" 'uv run python train.py'
```

Declare data dependencies so placement can prefer hosts that already have the
inputs:

```bash
weft run \
  --input hf:EleutherAI/pythia-160m \
  --gpu "nvidia>=24GB" \
  'python train.py'
```

Check or fetch Hugging Face assets:

```bash
weft data where hf:EleutherAI/pythia-160m
weft data fetch hf:EleutherAI/pythia-160m --host atlas
```

Autopilot may already be placing unplaced jobs and launching rentals. Check its
state before launching an instance manually:

```bash
weft autopilot status
```

If autopilot is running, let it launch the rental and monitor the result:

```bash
weft instance watch
```

If autopilot is idle, launch the rental manually:

```bash
weft start instance
weft instance watch
```

Pause autopilot first if you need manual offer or cost control while it is
running. See [Cloud GPU Instances](docs/guides/instances.md#coordinating-with-the-autopilot)
for the pause and resume sequence.

Inspect the submitted source closure and the worker's execution verification:

```bash
weft source inspect wj42
weft source ls wj42
weft source cat wj42 scripts/train.py
```

`source inspect` works for inventory and cloud attempts and reports whether the
worker verified the same identity that submission pinned. `source ls` reads a
pinned inventory directly from its stored R2 manifest; attempts that predate
immutable source pinning report that no authoritative listing is available.

Job IDs appear as `wj<N>` in output. Commands accept both `wj42` and `42`, and
many commands accept ranges such as `wj42:47`.

The full CLI reference is in [Command Reference](docs/reference/commands.md).
For task-oriented examples, start with the
[Workflow Guide](docs/guides/workflow-guide.md).

## Typical Workflow

A hostless submission lets placement choose a compatible machine with a useful
cache. The returned job ID identifies every attempt, log stream, and artifact:

```console
$ weft run \
  --gpu "nvidia>=24GB" \
  --input hf:EleutherAI/pythia-160m \
  --output output/metrics.json \
  -m "Pythia learning-rate sweep" \
  'uv run python train.py --lr 3e-4'
Job #1842 accepted and queued on atlas

$ weft status wj1842 --wait
$ weft info wj1842
$ weft artifact get wj1842 output/metrics.json -o ./metrics.json
```

Use `weft tui` for interactive supervision or `weft job list --plain` for a
script-friendly ledger snapshot. `weft watch` combines rental instances,
inventory hosts, and unplaced jobs in one live view.

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

## Data and Artifacts

Weft treats data movement as part of the job contract. Each selected machine can
use its own filesystem:

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

Artifacts can be listed, streamed, downloaded, or fed into another job. Direct
artifact commands replace log-embedded files and manual copies between
machines:

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
and teardown records in one local job ledger. The job record leads directly to
worker setup, without a separate managed-jobs controller or controller log
stream.
Use `weft info <job-id>` and `weft instance audit <job-id>` for lifecycle and
cleanup evidence.

Jobs tagged `interruptible` may run on Vast.ai interruptible offers. Weft treats
provider outbids as pause/resume events when Vast retains the instance disk. It
can raise bids up to the recorded on-demand reference. If the job must relaunch
on a new instance, Weft restages previously uploaded `output/` files from R2.

Provider setup and operation are covered in
[Cloud GPU Instances](docs/guides/instances.md). The command details live in
[Command Reference](docs/reference/commands.md).

## Data Locality and Estimation

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
