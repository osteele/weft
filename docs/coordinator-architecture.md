# Coordinator Architecture

This document describes the planned evolution of remote-jobs from a
laptop-centric architecture to a coordinator-based architecture with resource
inventory and data locality awareness.

## Motivation

The current architecture splits scheduling policy across two execution contexts
that can't always talk to each other:

- **Laptop CLI** makes host placement decisions and communicates job intent to
  remote hosts via SSH
- **Per-host queue runners** make execution-time scheduling decisions (exclusive
  jobs, GPU allocation, CPU allotment, benchmark idle detection)

Every new scheduling feature faces the same question: implement locally and hope
enough context is communicated, or push more logic to the queue runner? The queue
runner has grown to 1800+ lines of bash because it needs to make decisions the
laptop can't make at submission time.

Meanwhile, the system is designed for occasionally-connected use — the laptop may
be offline for hours. Moving the control plane to an always-connected node
resolves both problems.

### Research Synergy

The sister project
[llm-performance-models](../../research/llm-performance-models/) already has:

- Hardware YAML configs for cool30 and cool100 (GPU specs, storage tiers,
  transfer bandwidth)
- Roofline performance models for predicting runtime from model architecture
- Power coefficients per GPU type, calibrated from measurements
- Machine configs with storage tiers and transfer characteristics

A resource-aware coordinator becomes a real testbed for scheduling heuristics:
predicting job runtime on different hardware, optimizing placement across
heterogeneous GPUs, and modeling data transfer costs — all with real workloads.

## Design Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                     Laptop (occasionally connected)              │
│  ┌──────────┐  ┌──────────┐                                     │
│  │   CLI    │  │  SQLite  │  Intent-only: writes job intents,   │
│  │          │──│  (local) │  reads status. No placement logic.  │
│  └────┬─────┘  └──────────┘                                     │
│       │ SSH (when connected)                                     │
└───────┼─────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Studio (coordinator)                         │
│  ┌──────────────┐  ┌──────────┐  ┌─────────────┐               │
│  │ Coordinator  │  │  SQLite  │  │  Resource    │               │
│  │   Daemon     │──│ (primary)│  │  Inventory   │               │
│  └──────┬───────┘  └──────────┘  └─────────────┘               │
│         │                                                        │
│         │ Makes all placement & scheduling decisions             │
│         │ Watches intent dir, polls host state                   │
│         │                                                        │
│         ├── SSH ──────────────────────┐                           │
│         │                             │                           │
└─────────┼─────────────────────────────┼─────────────────────────┘
          │                             │
          ▼                             ▼
┌───────────────────────┐  ┌───────────────────────┐
│      cool30           │  │      cool100          │
│  ┌────────────────┐   │  │  ┌────────────────┐   │
│  │  Queue Runner  │   │  │  │  Queue Runner  │   │
│  │  (execution    │   │  │  │  (execution    │   │
│  │   mechanics)   │   │  │  │   mechanics)   │   │
│  └────────────────┘   │  │  └────────────────┘   │
│  RTX 3090 (24GB)      │  │  2x A100 80GB         │
│                       │  │  8x RTX 2080 Ti       │
└───────────────────────┘  └───────────────────────┘
```

## Key Design Decisions

### Protocol: SSH + JSONL intent files

The existing system uses SSH as its transport and JSONL as its data format. The
coordinator watches a local intent directory (it runs on studio) and the laptop
writes intents via SSH. This avoids introducing a new network service, TLS
certificates, or service discovery.

SQLite replication was considered but rejected because the coordinator needs to
enrich intents with placement decisions before they become jobs — this is not a
simple replication problem.

### Coordinator sits above queue runners (initially)

The queue runner handles local execution concerns (tmux sessions, log files,
process monitoring, GPU device allocation at job start time) that would be
expensive to rewrite immediately. The coordinator owns **placement** (which host)
and **scheduling order** (which job next) while delegating **execution
mechanics** to existing queue runners via the same JSONL append interface.

Over time, the coordinator can absorb more queue runner responsibilities,
especially cross-host scheduling (exclusive jobs, benchmark coordination).

### Coordinator learns host state via SSH polling

The `internal/hostinfo` and `internal/remote` packages already implement host
probing via SSH. The coordinator reuses these directly. No new agents needed on
compute hosts beyond the existing queue runners.

### Studio as both coordinator and compute host

Studio runs the coordinator daemon in a tmux session. When the coordinator
places a job on studio itself, it dispatches to studio's local queue runner the
same way it dispatches to cool30/cool100. The `remote.Host` interface already
supports local execution.

### Graceful degradation

If the coordinator is unreachable, the laptop falls back to local placement
(the current behavior). The system never blocks on coordinator availability.

## Intent Model

The laptop becomes an intent-writing client. A job submission looks like:

```
remote-jobs run --gpu-class A100 --input hf:meta-llama/Llama-3-8B 'python train.py'
```

This writes an intent record locally and syncs it to the coordinator:

```json
{
  "ts": "2026-02-25T10:00:00Z",
  "op": "place",
  "job": {
    "id": 456,
    "cmd": "python train.py",
    "dir": "~/code/project",
    "desc": "Training run",
    "env": ["WANDB_PROJECT=myproject"],
    "inputs": [
      {"kind": "hf-model", "id": "meta-llama/Llama-3-8B"}
    ],
    "outputs": [
      {"kind": "checkpoint", "id": "llama-ft-v1", "path": "checkpoints/"}
    ],
    "constraints": {
      "gpu_class": "A100",
      "gpu_mem_gb": 40
    },
    "tags": ["exclusive"]
  }
}
```

The coordinator:
1. Evaluates host capabilities against constraints
2. Scores hosts considering data locality, utilization, queue depth
3. Dispatches to the best host via the existing queue append mechanism
4. Records the placement decision (host, reasons) in the job record

## Resource Inventory

### Static host capabilities

YAML files describe what each host has. Format inspired by
llm-performance-models but simplified for scheduling needs:

```yaml
# ~/.config/remote-jobs/hosts/cool100.yaml
name: cool100
compute_backend: cuda
gpus:
  - name: A100 80GB PCIe
    class: A100
    memory: "80 GiB"
    indices: [0, 1]
  - name: RTX 2080 Ti
    class: RTX2080Ti
    memory: "11 GiB"
    indices: [2, 3, 4, 5, 6, 7, 8, 9]
cpu_cores: 64
memory: "256 GiB"
storage:
  - path: /home
    capacity: "2 TiB"
    read_bw: "7 GB/s"
```

```yaml
# ~/.config/remote-jobs/hosts/cool30.yaml
name: cool30
compute_backend: cuda
gpus:
  - name: RTX 3090
    class: RTX3090
    memory: "24 GiB"
    indices: [0]
cpu_cores: 48
memory: "128 GiB"
```

```yaml
# ~/.config/remote-jobs/hosts/studio.yaml
name: studio
compute_backend: mps
gpus:
  - name: M2 Max
    class: M2Max
    memory: "96 GiB"   # unified memory
    indices: [0]
cpu_cores: 12
memory: "96 GiB"
```

### Dynamic state

The coordinator periodically probes each host (reusing the existing
`internal/hostinfo` package) to get:

- Current GPU utilization and VRAM usage per device
- Running jobs and their resource reservations
- Queue depth (pending jobs)
- Disk usage on key paths

This combines with the static inventory to form a complete cluster view.

## Data Locality

### Asset tracking

The system tracks what data exists on which hosts:

| Asset Kind    | Example Identifier              | How Tracked                 |
|---------------|--------------------------------|-----------------------------|
| `hf-model`    | `meta-llama/Llama-3-8B`       | Scan `~/.cache/huggingface` |
| `hf-dataset`  | `wikitext`                     | Scan `~/.cache/huggingface` |
| `checkpoint`  | `llama-ft-v1` (job-produced)   | Job artifact manifest       |
| `artifact`    | `job-456:output.tar`           | Job artifact manifest       |

Assets are tracked in SQLite tables:

```sql
CREATE TABLE data_assets (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,       -- "hf-model", "hf-dataset", "checkpoint"
    identifier TEXT NOT NULL, -- "meta-llama/Llama-3-8B"
    size_bytes INTEGER,
    UNIQUE(kind, identifier)
);

CREATE TABLE host_data (
    id INTEGER PRIMARY KEY,
    host TEXT NOT NULL,
    asset_id INTEGER REFERENCES data_assets(id),
    path TEXT NOT NULL,
    verified_at INTEGER,      -- last confirmed present
    UNIQUE(host, asset_id)
);
```

### Job input/output declarations

Jobs declare what data they need and produce:

```
remote-jobs run \
  --input hf:meta-llama/Llama-3-8B \
  --output checkpoint:llama-ft-v1:checkpoints/ \
  'python train.py'
```

When a downstream job depends on an upstream job's output, the coordinator:
1. Knows which host has the artifact (from the upstream job's placement)
2. Prefers placing the downstream job on the same host
3. If placed elsewhere, can pre-stage the artifact via rsync

### Transfer cost estimation

The coordinator estimates transfer time using host-to-host bandwidth
(conservative estimates given the bursty network between cool30/cool100):

```go
func EstimateTransferTime(asset DataAsset, from, to string) time.Duration
```

This factors into placement scoring: a 10GB model already cached on cool30
avoids a multi-minute transfer that would be needed to run on cool100.

## Placement Scoring

When a job has no explicit host, the coordinator scores all eligible hosts:

```go
type PlacementScore struct {
    Host    string
    Score   float64
    Reasons []string
}
```

Scoring factors (in priority order):

1. **Hard constraints** (pass/fail):
   - GPU class match (job requires A100, host has A100)
   - GPU memory fit (job needs 40GB, GPU has 80GB)
   - Compute backend (CUDA vs MPS)

2. **Soft factors** (weighted scoring):
   - Data locality: prefer host with required inputs already cached
   - Current utilization: prefer less-loaded host
   - Queue depth: prefer host with fewer pending jobs
   - Transfer cost: penalize hosts that need data staged

3. **Future factors** (research integration):
   - Predicted runtime from llm-performance-models roofline
   - Power/energy cost
   - Opportunity cost (small job on A100 when 2080 Ti suffices)

## Coordinator Daemon

### Architecture

```go
type Coordinator struct {
    db          *sql.DB
    inventory   []inventory.HostSpec
    hostMonitor *monitor.Monitor
    scorer      *placement.Scorer
    dataTracker *dataloc.Tracker
    intentDir   string
}

func (c *Coordinator) Run(ctx context.Context) error {
    // 1. Watch intent directory for new files (fsnotify)
    // 2. Periodically poll host state (reuse monitor package)
    // 3. For each unplaced job: score hosts, pick best, dispatch
    // 4. Dispatch = append to remote queue via SSH (reuse ops.AppendQueueEntry)
    // 5. Update DB with placement decision
    // 6. Periodically scan hosts for data assets
}
```

### Lifecycle

- Started via `remote-jobs coordinator start` (runs in tmux on studio)
- Watches `~/.cache/remote-jobs/intents/` for new intent files
- Polls host state every 15-30 seconds
- Dispatches jobs to queue runners via the existing JSONL append mechanism
- Logs decisions to `~/.cache/remote-jobs/coordinator.log`

### Detection by CLI

The laptop CLI checks if the coordinator is running:
```bash
ssh studio 'test -f ~/.cache/remote-jobs/coordinator.pid && \
  kill -0 $(cat ~/.cache/remote-jobs/coordinator.pid) 2>/dev/null'
```

If running: write intent file, let coordinator handle placement.
If not running: fall back to local placement (current behavior).

## DB Topology

```
Laptop SQLite ──(intent sync)──▶ Studio SQLite (coordinator, authoritative)
                                       │
                                       │ (placement decisions flow back
                                       │  via standard sync/reconcile)
                                       ▼
                               Queue Runners (file-based state)
```

- Laptop DB is a local replica for offline use
- Studio DB is the authoritative source for placement decisions
- The existing three-way merge reconciliation handles conflicts
- Intent files are append-only and idempotent (safe to replay)

## Migration Phases

### Phase 1: Resource Inventory

Add static host capability descriptions. No behavioral changes.

- New: `internal/inventory/` package (HostSpec, GPUSpec types)
- New: Host YAML files in `~/.config/remote-jobs/hosts/`
- Extend: `internal/config/` to load inventory
- Extend: `cmd/host.go` to display inventory data
- Command: `remote-jobs host list` shows GPU specs, VRAM, disk

### Phase 2: Data Locality Tracking

Track what data exists on which hosts.

- New: `internal/dataloc/` package (DataAsset, HostDataEntry types)
- New: SQLite tables (data_assets, host_data)
- New: HuggingFace cache scanner
- Extend: artifact system to register outputs in host_data
- Command: `remote-jobs host data cool30` shows cached models/datasets

### Phase 3: Intent-Based Job Submission

Allow jobs without explicit host. Local placement scoring.

- Make host optional in `remote-jobs run`
- New: `internal/placement/` package (scoring, constraints)
- New: `internal/intent/` package (intent file writer)
- New: `pending_placement` job status
- New: `--input` and `--output` flags on `remote-jobs run`
- Local placement works from laptop when online

### Phase 4: Coordinator Daemon

The always-on scheduler on studio.

- New: `cmd/coordinator.go` (start/stop/status subcommands)
- New: `internal/coordinator/` package (main loop, dispatch, watcher)
- CLI detects coordinator and switches to intent-only mode
- Coordinator dispatches to queue runners via existing JSONL append
- Coordinator absorbs cross-host scheduling (exclusive, benchmark)

### Phase 5: Smart Scheduling and Data Transfer

Transfer-cost-aware decisions, data pre-staging, research integration.

- Transfer cost estimation using inventory bandwidth specs
- Pre-staging: coordinator issues download/rsync before dispatch
- Artifact-aware job chaining (place downstream near upstream output)
- Integration hooks for llm-performance-models (runtime prediction)
- Web dashboard: cluster overview, data locality map, decision log

### Phase Dependencies

```
Phase 1 (Inventory) ──┐
                       ├── Phase 3 (Intent submission) ──┐
Phase 2 (Data Loc) ───┘                                  │
                                                          ├── Phase 5 (Smart scheduling)
                                              Phase 4 ────┘
                                           (Coordinator)
```

Phases 1 and 2 can proceed in parallel. Phase 3 requires Phase 1. Phase 4
requires Phases 1-3. Phase 5 requires Phase 4.

## Open Questions

- **Coordinator HA**: If studio goes down, should another host take over? Or is
  graceful degradation (laptop does local placement) sufficient?
- **Queue runner simplification**: As the coordinator absorbs scheduling logic,
  how much of queue-runner.sh can be simplified? Should the coordinator
  eventually talk directly to tmux instead of through the queue runner?
- **Multi-user**: Could multiple laptops write intents to the same coordinator?
  (Not a current requirement, but the intent model supports it naturally.)
- **Performance model integration depth**: Should the coordinator call
  llm-performance-models as a library, or consume pre-computed estimates?
- **Artifact garbage collection**: When should cached data be evicted from hosts
  to free disk space? Who decides?
