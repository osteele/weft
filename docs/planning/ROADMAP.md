# Campaign System Roadmap

Gaps between weft's campaign system and llm-performance-models' Vast.ai
management. Filling these would let llm-performance-models run on top of weft
instead of maintaining its own provisioning infrastructure.

## Tier 1: Must Have

| Gap | Priority | User Benefit | llm-perf-models ref |
|-----|----------|--------------|---------------------|
| Reuse Docker image packages | High | Skip re-downloading ~3 GB of torch/CUDA wheels during `uv sync` by reusing packages pre-installed in the PyTorch Docker image. Pre-create the venv with `--system-site-packages` and pin `UV_PYTHON` to the image's conda interpreter. Requires either projects pin torch to the image version (currently 2.6.0) or regenerate their lockfile at a compatible version. | — |
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

## Predictor Daemon

`weft run` currently shells out to `uv run --project <predictor-path> job-estimator
status` to check predictor model readiness on every submission. The `uv` cold-start
plus Python import graph (sklearn, pandas, etc.) takes ~30s, dominating submission
latency. A persistent file cache (`status-cache.json`, 5-minute TTL) and async
stale-check mask the cost for most submissions, but the first submission after the
cache expires still pays the full cost.

A long-lived predictor daemon would eliminate the cold-start entirely:

- Start once (on demand or via launchd alongside the coordinator).
- Expose status + prediction RPCs over a Unix socket.
- Keep the job-estimator Python process warm with loaded models.
- `weft run` queries the daemon instead of shelling out.

This would take submission latency from seconds to milliseconds on the predictor
path, at the cost of managing another long-lived process.

## On-Prem Derived Status Model

Extend the derived-status approach used by cloud jobs to on-prem jobs so status
is computed from execution facts rather than multi-field mutable merges.

Remaining intent:

1. Derive on-prem display status from attempt facts.
2. Remove three-way status merging for on-prem queue updates.
3. Retire transitional status fields once all paths use derived status.

## Naming Cleanup: Launch → CloudInstance

Internal naming has drifted three ways: the user-facing CLI says `instance`
(`weft instance list`, `wi<id>` IDs, docs), the Go code says `db.Launch`, and
the SQL tables say `launches` / `launch_live_state` / `launch_job_membership`.
The file `internal/db/cloud_instances.go` already uses the user-facing concept.
Priority: Low — housekeeping, no functional change.

Renames (all together to avoid a fourth mismatch):

- Struct: `db.Launch` → `db.CloudInstance`.
- Tables: `launches` → `cloud_instances`, `launch_live_state` →
  `cloud_instance_live_state`, `launch_job_membership` →
  `cloud_instance_job_membership`.
- Columns/fields: `Job.LaunchID` → `Job.CloudInstanceID`; helpers and
  variables named `Launch*` (e.g. `LaunchByID`, `GetLaunch`,
  `ListLaunches`, `LaunchStatus*`) → `CloudInstance*`.

Migration path: SQLite `ALTER TABLE ... RENAME TO` and `RENAME COLUMN` in a
single migration; Go-side rename is mechanical (touches ~10 files). The
user-facing CLI (`weft instance ...`, `wi<id>` IDs, docs) stays as-is —
this aligns the internals to what users already see.

`cloud_instances` over bare `instances`: keeps the namespace unambiguous if
on-prem hosts ever become first-class table-backed entities, and avoids
collisions with Go's pervasive use of "instance" for type/struct instances
when grepping.

## Banner monitors (deferred)

The `internal/banner/` system already surfaces schema drift, autopilot
pause, R2 unreachable, and Vast.ai unreachable across the watch TUI
(`RunWatchLoop`). Adding more producers and wiring more consumers is
mechanical from there. Candidates, ordered by frequency I'd expect to hit:

| Banner | Source | Notes |
|--------|--------|-------|
| Database opened read-only | When `db.OpenForReading` falls back | User is reading possibly-stale data; surface so display drift isn't a surprise. |
| Grace-period instance expiring soon | Poll `cloud_instances.grace_deadline` | Banner at < 5 min remaining; users miss these. |
| Background sync error streak | Track consecutive `cloud sync skipped` events | N≥3 in a row → banner. Catches network/VPN flaps that aren't a single-poll failure. |
| Disk space low (backups dir, log cache) | `statfs` on `~/.config/weft/backups`, `~/.cache/weft/logs` | Threshold ~5 GB free or <5%. |
| Long DB write lock held | Track time since last successful Open() with migrations | Banner if another process has held the writer > 30 s. |
| Update available (binary) | Compare `os.Executable()` mtime to running PID's start | Distinct from schema-drift: binary updated but DB schema unchanged; offer relaunch. |

Wiring sites still needed (one-time, ~40 lines each):

- `RunWatchPlainLoop` — plain-mode watch entrypoints (job watch, project watch). Subscriber prints stderr line on first detection of each banner ID.
- `RunProjectWatchTUI` — project-watch bubbletea program; same pattern as `watchRouterModel`.
- `RunLaunchProgram` — launch-instance TUI.
- `runAutopilotRunLoop` — daemon. On schema drift with newer binary, exit cleanly so launchd respawns; on stuck case, log a critical-level warning.

The "rare TUI gets banner integration first time it's used" pattern is fine
— banners are a net additive feature; entrypoints without integration are
no worse than before.

## requested_status / pending_status / attempt-status three-way merge

The `job_status` view derives `Job.status` from a three-way merge of:

- `jobs.requested_status` — user-issued state transitions (cancel, kill,
  requeue, draft) that have not yet propagated to the agent.
- `job_attempts.pending_status` — the legacy "this attempt is being
  worked on" sub-state. Most uses (placement-window) have moved to
  `PlacementIntent`; the remaining cases are reconcile-driven mid-attempt
  transitions.
- `job_attempts.status` — the actual state of the latest open attempt.

The merge logic in the view took a comment block to explain. With
`PlacementIntent` extracted (see specs/job-move.allium), the surface area
shrunk but did not collapse. Worth revisiting when the view's complexity
becomes a concrete pain point. Candidate moves:

- Promote `requested_status` to a `UserIntent` entity (kind = cancel /
  kill / requeue / draft) so the merge becomes "open intent overrides
  attempt status" instead of a column-comparison dance.
- After enough soak time, drop `pending_placement` from the `Status`
  enum entirely — `PlacementIntent` now carries that signal — and remove
  the autopilot's pending_placement rescue branch + the
  `is_unplaced_awaiting_placement` predicate's special-case for it.

## InstanceAcceptsJobs unified predicate

`instanceAcceptsReuse` (`internal/campaign/reuse.go`) ANDs three signals:

- `HasActiveTerminationIntent` (the JSON marker — see specs/job-move
  TerminationIntent)
- `Cordoned` (a separate field on Launch)
- `LaunchStatusGrace` with `GraceDeadline` not yet expired

Today it returns `(bool, string)` with an ad-hoc reason string formatted
per case. UI surfaces (`weft instance list`, the move picker, the watch
TUI) reformat the string. Promoting to a typed result like:

```go
type InstanceBlocker int
const (
    InstanceBlockerNone InstanceBlocker = iota
    InstanceBlockerTerminating
    InstanceBlockerCordoned
    InstanceBlockerGraceExpired
    InstanceBlockerWrongStatus
)
```

would let UI surfaces format consistently and let any future fourth
shutdown signal slot in cleanly. Mechanical refactor; small surface.
