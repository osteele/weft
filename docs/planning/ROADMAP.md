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

1. `weft instance launch --yes --jobs <ids>` provisions instances.
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

## Execution target normalization

Phase one is in place: `execution_targets` stores inventory hosts and rental
instances, `launches.target_id` links rental rows, and
`job_attempts.target_id` is now backfilled as a compatibility shadow for the
existing `host` / `launch_id` placement columns.

Remaining work:

- Make `job_attempts.target_id` the only placement pointer used by new code.
- Replace `job_status.effective_target_kind` derivation from parallel
  `host` / `launch_id` columns with a join through `execution_targets`.
- Move host-sync, rental-sync, placement, and TUI queries to target-kind
  filters instead of ad hoc `host` / `launch_id` predicates.
- Add integrity validation that rejects mixed placement state
  (`target_id` disagrees with `host` or `launch_id`).
- Once callers no longer depend on the shadow fields, rebuild
  `job_attempts` without duplicated placement columns or keep them as
  generated/compatibility columns with one-way writes from `target_id`.

The goal is to make "inventory host plus rental launch on the same attempt"
impossible by schema shape rather than by scattered update guards.

## Placement correctness by construction

Recent move-to-new fixes added explicit transition helpers, a stale-row
reconciler, and a canonical placement read model consumed by the TUI and
narrator snapshots. The next step is to keep moving placement writes and
notifications behind the same model, so invalid intermediate rows are harder
to produce and harder for operators to observe.

- **Make placement transitions the only write path.**
  `AttachMoveIntentTargetLaunch` and `HandleMoveTargetFailedBeforeStart`
  now exist, but other placement and attempt paths still write `jobs`,
  `job_attempts`, `launches`, and `move_intents` directly. Keep moving state
  changes behind named transition functions until orchestration mostly says
  "apply transition X" instead of editing rows.
- **Add DB invariants.**
  Useful constraints include at most one open `move_intents` row per job,
  at most one open `job_attempts` row per job, live source launches for open
  move-to-new intents, `end_time` on terminal attempts, and a guard that
  started attempts are never restored to a source queue.
- **Unify retry-budget semantics.**
  Move-to-new retry counters, launch attempts, relaunch blockers, and
  campaign replacement loops still account for attempts separately. The goal
  is one durable placement-attempt budget model so "four retries" means the
  same thing across initial placement, relaunch, move-to-new, and replacement.
- **Emit placement notifications from transitions.**
  The TUI and narrator now share a canonical placement read model, but
  notifications can still be generated from snapshots taken between several
  writes. Emit placement change events from named transitions, or from a
  durable transition-event table, so narration does not report transient
  intermediate states.

## UserIntent entity (replaces requested_status three-way merge)

The `job_status` view derives `Job.status` from `jobs.requested_status`
(user-issued state transitions: cancel, kill, requeue, draft) overlaying
the latest attempt's status. Promoting `requested_status` to a
`UserIntent` entity (kind = cancel | kill | requeue | draft) would make
the merge "open intent overrides attempt status" instead of a column-
comparison dance, matching the Move/Placement/Termination intent shape.

Worth revisiting when the view's complexity becomes a concrete pain
point. Mostly clarity, not behavior change.

## Rebalance: variance-aware and regret-minimizing objectives

The initial rebalance redesign (see `specs/campaign-lifecycle.allium`
QueueRebalanceJob) accepts moves that improve a scalarized
`Cost·$ + Time·hr` score using **mean** runtime predictions from
`predictor.ResolvePredictBatch`. Two follow-ups are deferred behind an
empirical trigger.

**Promotion criterion.** V1 logs the lower- and upper-bound Δscore
alongside the mean Δscore for every accepted/rejected move (cheap —
three score evaluations, not 100). If the bounds straddle zero on a
material fraction of decisions, mean-only is making coin-flip calls and
the variance-aware version moves up. If they don't, these stay
deferred indefinitely.

- **Monte Carlo over predictor variance.** The predictor returns
  Mean/Lower/Upper per job. Sample N ≈ 100 runtime draws from the
  implied distribution (PERT or triangular) and accept moves whose
  *expected* score improvement clears the threshold. ~10× v1's per-pass
  compute (still single-digit ms). Same code path — replace `Mean`
  with sampled values. Needs deterministic seeding (job IDs or pass
  count) so consecutive `weft rebalance` runs agree, and a fallback for
  jobs without a predictor result (`DefaultJobDuration`'s 4× spread
  produces noise, not signal).

- **Expected-regret minimization.** Strict regret-minimizing objective:
  for each candidate move sample joint runtimes, compute makespan with
  and without the move, and take the mean of
  `max(0, makespan_before − makespan_after)` minus the symmetric loss.
  Accept only when expected improvement is positive. Conceptually
  cleaner; harder to debug ("why didn't this move?") and shares MC
  infrastructure with the variance-aware version. Worth doing only if
  MC-mean rebalance proves systematically wrong.

True online regret minimization (Thompson sampling, posterior updates
from realized outcomes) is a larger build-out and not on this list.

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
