# Campaign System Roadmap

Gaps between weft's campaign system and llm-performance-models' Vast.ai
management. Filling these would let llm-performance-models run on top of weft
instead of maintaining its own provisioning infrastructure.

## Tier 1: Must Have

| Gap | Priority | User Benefit | llm-perf-models ref |
|-----|----------|--------------|---------------------|
| Persistent volumes | High | Reuse caches across instances to avoid repeated dependency/model downloads and shorten time-to-first-job. | `vastai_provision.sh`: volume create/find/attach |
| Phone-tree copy | High | Fan out warm caches from one seeded machine to many targets so campaigns scale faster with less redundant network work. | `run_campaign.py`: copy loop with seeded/unprovisioned sets |
| Copy failure fallback | High | Keep launch robust: if copy fails repeatedly, targets should still proceed via direct setup instead of stalling the campaign. | `run_campaign.py`: `--copy-retries` flag |

## Tier 2: High Value

| Gap | Priority | User Benefit | llm-perf-models ref |
|-----|----------|--------------|---------------------|
| Seed vs target distinction | Medium | Let users choose a dedicated seed machine type for setup efficiency while keeping target fleet optimized for workload execution. | `--seed-gpu`, `--seed-instance` flags |
| External donor support | Medium | Reuse an already-warm instance as donor to reduce launch cost and startup delay. | `--donor-instance ID` flag |
| Instance phase tracking | Medium | Improve operator visibility by showing setup/copy/run lifecycle stages, not just coarse running/completed states. | State file with per-GPU status tracking |
| Per-instance actual cost tracking | Medium | Show real campaign spend from observed runtime, not just estimates or limits. | Cost summary with per-GPU breakdown |
| Resume interrupted campaigns | Medium | Recover quickly after interruption by continuing from persisted state rather than restarting all work. | `--resume` flag, JSON state file |
| Seed-first validation | Medium | Reduce blast radius by validating the setup path on one seed before provisioning full scale. | `--seed-first` flag |

## Tier 3: Nice to Have

| Gap | Priority | User Benefit | llm-perf-models ref |
|-----|----------|--------------|---------------------|
| Volume pooling | Low | Reuse existing volumes across campaigns to reduce provisioning churn and startup time. | Volume find/reuse in provisioning |
| Multi-phase workflows | Low | Support calibration or setup flows that need multiple ordered phases per instance. | Two-phase calibration per GPU |
| vLLM-only mode | Low | Provide a shorter calibration path for faster iteration when full calibration is unnecessary. | `--only-vllm` flag |

## Control Plane Evolution

Keep artifacts/caches on R2, but separate artifact storage concerns from mutable
coordination concerns.

- Data plane (`internal/dataplane`): append-only artifacts such as bundles,
  scripts, logs, outputs, and manifests.
- Control plane (`internal/controlplane`): mutable coordination state such as
  commands, heartbeats, phase markers, and termination intent.

Preferred direction:

1. Keep R2 as data plane storage.
2. Use Cloudflare Queues for coordinator command ingestion.
3. Use Durable Objects (per instance) for ordered mutable control state.
4. Use R2 event notifications to drive result ingestion.

## Integration Strategy

llm-performance-models can already use weft for lifecycle management while
keeping its existing calibration logic:

1. `weft campaign launch --yes --jobs <ids>` provisions instances.
2. `weft instance ssh --print <id>` provides access details.
3. llm-performance-models runs calibration scripts over SSH.
4. `weft campaign terminate <id>` handles teardown.

The major blockers for deeper integration remain persistent volumes and
phone-tree copy.

## On-Prem Derived Status Model

Extend the derived-status approach used by cloud jobs to on-prem jobs so status
is computed from execution facts rather than multi-field mutable merges.

Remaining intent:

1. Derive on-prem display status from attempt facts.
2. Remove three-way status merging for on-prem queue updates.
3. Retire transitional status fields once all paths use derived status.
