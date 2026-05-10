# Weft — Coordinator Architecture

Weft is forked from weft to evolve from a laptop-centric job runner into
a coordinator-based workload scheduler with resource inventory and data locality
awareness.

The name "weft" comes from weaving — the weft is the thread actively carried
through the warp (the fixed infrastructure). Workloads are weft threads woven
across a fabric of compute resources.

## Motivation

The current weft architecture splits scheduling policy across two
execution contexts that can't always talk to each other:

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

The sibling project `llm-performance-models` already has:

- Hardware YAML configs for titan and atlas (GPU specs, storage tiers,
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
│         │ Makes placement & priority decisions                    │
│         │ Watches intent dir, polls host state                   │
│         │                                                        │
│         ├── SSH ──────────────────────┐                           │
│         │                             │                           │
└─────────┼─────────────────────────────┼─────────────────────────┘
          │                             │
          ▼                             ▼
┌───────────────────────┐  ┌───────────────────────┐
│      titan           │  │      atlas          │
│  ┌────────────────┐   │  │  ┌────────────────┐   │
│  │  Edge Agent    │   │  │  │  Edge Agent    │   │
│  │  (local sched, │   │  │  │  (local sched, │   │
│  │   execution)   │   │  │  │   execution)   │   │
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

### Federated scheduling: coordinator + edge agents

The system uses **federated scheduling** inspired by Borg/Kubernetes and Mesos,
where authority is divided between central and local agents by information
locality:

- **Coordinator** (studio): Cluster-wide view. Owns **placement** (which host)
  and **priority ordering** (which job next). Knows cross-host data locations,
  cluster queue state, and host capabilities. Does not need moment-to-moment GPU
  utilization.

- **Edge agents** (titan, atlas, studio queue runners): Host-local authority.
  Own **execution timing** (when to start a placed job), exclusive/benchmark
  coordination on local GPUs, resource enforcement, and measurement collection.
  Can autonomously defer jobs (e.g., system is hot, benchmark running) without
  asking the coordinator.

The contract between coordinator and edge agent:
- Coordinator sends: "run this job, priority X, inputs Y"
- Edge agent sends back: "accepted / deferred / completed / failed" + measurements
- Edge agent has full autonomy over *when* and *how* within its host

This separation means each agent decides based on what it can best observe.
The coordinator might place a job on atlas for data locality, but the edge
agent defers it because a benchmark is running. Neither needs the other's full
context to make a good decision.

The Go agent (`weft-agent`) already implements the edge agent role — it handles
tmux sessions, log files, process monitoring, GPU device allocation, exclusive
job coordination, and benchmark idle detection.

### Coordinator–edge agent contract

The coordinator and edge agents operate at the **job level**: the coordinator
decides *what* runs *where*, and the edge agent decides *when* and *how*. This
differs from Mesos's resource-level contract, where the master offers raw
resources and frameworks decide what to place on them.

A key property: the edge agent maintains a **local buffer of upcoming work**, so
disconnections don't stall execution. This comes from the HPC/batch world rather
than from Mesos (where accepted tasks start immediately with no local queue).

Whether the buffer is filled by push (coordinator appends to remote queue) or
pull (edge agent requests work) is an implementation detail — the information
flow is the same either way.

The contract includes:

- **Placement decisions**: Which jobs are assigned to this host, with priority
  ordering
- **Queue depth guidance**: How much work buffer the edge should maintain (the
  coordinator knows cluster-wide balance; the edge knows local throughput)
- **Job metadata**: Input data requirements, resource constraints, tags
  (exclusive, benchmark) that inform the edge agent's local scheduling
- **Status feedback**: The edge agent reports job lifecycle events (accepted,
  deferred, started, completed, failed) and measurements back to the coordinator

The edge agent has **deferral authority**: it can delay a placed job based on
local conditions (benchmark running, GPU thermal throttling, exclusive lock held)
without asking the coordinator. The coordinator sees deferral as a status update
and factors it into future placement decisions.

The current implementation uses push (JSONL append over SSH), inherited from the
original laptop-driven model. The contract is designed to be transport-agnostic.

### Coordinator learns host state via SSH polling

The `internal/hostinfo` and `internal/remote` packages already implement host
probing via SSH. The coordinator reuses these directly. No new agents needed on
compute hosts beyond the existing queue runners.

### Studio as both coordinator and compute host

Studio runs the coordinator daemon in a tmux session. When the coordinator
places a job on studio itself, it dispatches to studio's local queue runner the
same way it dispatches to titan/atlas. The `remote.Host` interface already
supports local execution.

### Graceful degradation

If the coordinator is unreachable, the laptop falls back to local placement
(the current behavior). The system never blocks on coordinator availability.

## Intent Model

The laptop becomes an intent-writing client. A job submission looks like:

```
weft run --gpu-class A100 --input hf:meta-llama/Llama-3-8B 'python train.py'
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
# config/hosts/atlas.yaml
name: atlas
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
weft run \
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
(conservative estimates given the bursty network between titan/atlas):

```go
func EstimateTransferTime(asset DataAsset, from, to string) time.Duration
```

This factors into placement scoring: a 10GB model already cached on titan
avoids a multi-minute transfer that would be needed to run on atlas.

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
   - GPU class match (job requires A100, host has A100) — fuzzy matching
   - GPU memory fit (job needs 40GB, GPU has 80GB)
   - Compute backend (CUDA vs MPS)

2. **Soft factors** (weighted scoring):
   - Data locality: +2 per input already cached on host
   - Transfer cost: -0 to -5 based on missing data size and host bandwidth
   - Performance factor: up to -5 for GPU jobs, up to -3 for CPU jobs (based on host cpu_factor/gpu_factor relative to baseline)
   - GPU utilization: up to -3 for high GPU load
   - CPU utilization: up to -1 for high CPU load
   - Queue depth: -0.5 per pending job (capped at -3)
   - Explicit host preference: +10 when user specifies a host

3. **Future factors** (research integration):
   - Predicted runtime from llm-performance-models roofline
   - Power/energy cost
   - Opportunity cost (small job on A100 when 2080 Ti suffices)
   - MPS vs CUDA performance differential for studio placement

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

- Started via `weft coordinator start` (runs in tmux on studio)
- Watches `~/.cache/weft/intents/` for new intent files
- Polls host state every 15-30 seconds
- Dispatches jobs to queue runners via the existing JSONL append mechanism
- Logs decisions to `~/.cache/weft/coordinator.log`

### Detection by CLI

The laptop CLI checks if the coordinator is running:
```bash
ssh studio 'test -f ~/.cache/weft/coordinator.pid && \
  kill -0 $(cat ~/.cache/weft/coordinator.pid) 2>/dev/null'
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

### Phase 1: Resource Inventory and Project Setup

Rename the project, set up host capability descriptions. No behavioral changes
to job execution.

- Rename module, binary, CLI references from weft to weft
- Create `internal/inventory/` package (HostSpec, GPUSpec types)
- Create host YAML files for titan, atlas, studio
- Extend `internal/config/` to load inventory
- Add `weft host list` showing GPU specs, VRAM, storage
- Wire inventory into `internal/hostinfo/`

### Phase 2: Data Locality Tracking

Track what data exists on which hosts.

- Create `internal/dataloc/` package (DataAsset, HostDataEntry types)
- Add SQLite tables (data_assets, host_data)
- HuggingFace cache scanner
- Extend artifact system to register outputs in host_data
- Add `weft host data <host>` command
- Add `--input` and `--output` flags to `weft run`

### Phase 3: Intent-Based Job Submission

Allow jobs without explicit host. Local placement scoring.

- Make host optional in `weft run`
- Create `internal/placement/` package (scoring, constraints)
- Define the intent writer package boundary
- Add `pending_placement` job status
- Local placement works from laptop when online

### Phase 4: Coordinator Daemon ✅

The always-on scheduler on studio.

- ✅ `weft coordinator start/stop/status` subcommands
- ✅ `internal/coordinator/` package (main loop, dispatch, watcher)
- ✅ CLI detects coordinator and switches to intent-only mode
- ✅ Coordinator dispatches to queue runners via existing JSONL append
- ✅ Coordinator absorbs cross-host scheduling

### Phase 5: Smart Scheduling and Data Transfer ✅

Transfer-cost-aware decisions, data pre-staging, web dashboard.

- ✅ Transfer cost estimation using inventory bandwidth specs (`internal/placement/`)
- ✅ Pre-staging: coordinator rsyncs missing inputs before dispatch (`internal/prestage/`)
- ✅ Utilization-aware scoring: GPU/CPU load and queue depth penalties (`internal/placement/`)
- ✅ Post-job artifact recording: declared outputs tracked on completion (`internal/ops/artifacts.go`)
- ✅ HF cache scanner: detailed output with sizes (`internal/dataloc/hfscan.go`)
- ✅ Web dashboard: cluster overview with live GPU bars, coordinator status, oplog (`internal/web/`)
- ✅ Crash-safe idempotency: processed intents persisted to SQLite (`internal/db`)
- ✅ Cloud GPU bursting: Vast.ai CLI wrapper, TUI cloud menu, cost estimation (`internal/vastai/`, `internal/placement/cloud_offers.go`)
- 🔮 Integration hooks for llm-performance-models (future)

### Phase Dependencies

```
Phase 1 (Inventory + Setup) ──┐
                               ├── Phase 3 (Intent submission) ──┐
Phase 2 (Data Locality) ──────┘                                  │
                                                                  ├── Phase 5
                                                      Phase 4 ───┘
                                                   (Coordinator)
```

Phases 1 and 2 can proceed in parallel. Phase 3 requires Phase 1. Phase 4
requires Phases 1-3. Phase 5 requires Phase 4.

## Open Questions

- **Coordinator HA**: If studio goes down, should another host take over? Or is
  graceful degradation (laptop does local placement) sufficient?
- **Agent simplification**: As the coordinator absorbs scheduling logic,
  how much of the Go agent can be simplified? (The bash queue-runner.sh has
  already been fully replaced by the Go agent.)
- **Multi-user**: Could multiple laptops write intents to the same coordinator?
- **Performance model integration depth**: Should the coordinator call
  llm-performance-models as a library, or consume pre-computed estimates?
- **Artifact garbage collection**: When should cached data be evicted from hosts?
- **Relationship to weft**: Should weft be able to interoperate with
  existing weft installations (same queue runner, same DB format), or
  is a clean break preferred?
