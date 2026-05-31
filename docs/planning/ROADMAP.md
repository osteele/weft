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
| On-prem CUDA inventory | High | Record host CUDA/driver compatibility during inventory discovery so jobs with pinned CUDA wheels are not placed on incompatible local hosts. Cloud placement already has provider CUDA filters; on-prem needs comparable host metadata. | — |

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

## Structured termination reasons for credit exhaustion (partially landed)

The `TerminationReasonAccountCreditExhausted` constant now exists in
`internal/db/cloud_instances.go`, the bidding filter excludes it via the
`NOT IN` list (`internal/bidding/build.go`), and operators can apply it
retroactively via `weft instance mark-credit-exhausted` (see
`cmd/instance_mark_credit_exhausted.go`). This was triggered by a Vast.ai
credit-exhaustion incident that destroyed running instances — the operator
needed a way to label those rows as credit-related so they didn't poison
the survival model.

Still pending (the original deferral): wire CreateInstance-time
credit-exhaustion failures through the new constant. Today every
`UpdateLaunchStatus` write site that observes
`errors.Is(err, cloud.ErrAccountCreditExhausted)` still writes
`TerminationReasonInfraFailure` (or `TerminationReasonProviderFailure`)
plus a substring-matching detail. The bidding filter's LIKE patterns on
`termination_detail` remain as the safety net for those rows until the
call-site survey is done.

Right trigger to finish the work:

- A new credit-error variant lands and the substring lists drift.
- The two phrase lists in `internal/vastai/client.go isAccountCreditError`
  and `internal/bidding/build.go` LoadInstanceOutcomes diverge.
- Bidding gains richer outcome typing for other reasons (OOM, user
  cancellation, etc.) — at which point auditing the existing
  `cloud.ErrAccountCreditExhausted` call sites becomes cheap.

Until then: the structured constant + the LIKE-pattern net cover both the
retroactive-reclassification path and the legacy `provider_failure` +
detail path. Keep the two phrase lists in sync by hand.

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

Current state: `execution_targets` stores inventory hosts and rental
instances, `launches.target_id` links rental rows, and
`job_attempts.target_id` is now the authoritative placement pointer. The
legacy `host` / `launch_id` columns remain as compatibility shadows for
estimator exports, lab-notebook analysis, and ad hoc SQL.

Remaining work:

- Move host-sync, rental-sync, placement, and TUI queries to target-kind
  filters instead of ad hoc `host` / `launch_id` predicates.
- Keep `job_attempts.host` and `job_attempts.launch_id` as compatibility
  shadows while the stable estimator/research contracts (`training_examples`,
  `job_run_training_examples`, and lab-notebook SQL) still consume them.
  Physical removal is deferred until those consumers have a replacement
  contract.

The goal is to make "inventory host plus rental launch on the same attempt"
impossible by a single authoritative target plus validation, while preserving
the current analytics surfaces.

Deferred compatibility-contract work:

- Define the replacement SQL contract for estimator and research consumers
  before any physical removal of `job_attempts.host` or
  `job_attempts.launch_id`. The likely shape is a stable analysis view that
  projects `host`, `launch_id`, `target_id`, `target_kind`, provider, and
  hardware fields from `execution_targets`, `launches`, and host inventory.
- Update `docs/reference/estimation.md`, lab-notebook scripts, and export
  tests to consume that contract instead of raw `job_attempts` placement
  columns.
- Only after that migration is complete, consider rebuilding `job_attempts`
  without compatibility columns. Until then, keep the columns synchronized and
  validated.

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

## Mutable Current State Split

Move mutable lifecycle fields out of historical attempts once placement
identity is stable enough that the storage move can be evaluated on its own.

Candidate fields:

- `pending_status` / `pending_at` → an open intent or current-state table.
- `last_synced_status` → current remote-observation state for the open
  attempt.
- `jobs.requested_status` → a durable user-intent entity, as above.

The goal is to make completed `job_attempts` rows append-mostly historical
facts, while live intent and sync state live in constrained current-state
tables. Defer this until the current open-attempt and execution-target
normalization has settled; doing it together with placement identity changes
would make lifecycle regressions harder to isolate.

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

## Unified asset graph (post-named-assets)

`asset:NAME` (named assets, shipped 2026-05-25) unblocks the laptop → any-host
data flow that `checkpoint:` could not. It does so by carrying a parallel
`named_assets` table and a parallel resolution path next to the producer-job
`--needs path:JOB` form. Long-term these should converge into a single asset
graph that subsumes `hf:`, `hf-dataset:`, `checkpoint:`, `corpus:`,
`job-output:`/`--needs`, and `asset:` under one record type:

```
Asset {
  Name         string
  ContentHash  string
  Locations    []Location  // (host, path) | (r2, key) | (hf, repo@rev) | (peer-host, path)
}
```

Benefits:

- Placement scoring becomes "find the min-cost reachable copy" — no per-kind
  branching. Resolves the current footgun where users pick the wrong kind
  (`checkpoint:` for a laptop file) and the job blocks instead of staging.
- Eviction (RQ-F1) and proactive pre-staging (RQ-F2) become uniform across
  HF caches, on-prem named assets, and R2-backed named assets.
- Peer-to-peer staging between inventory hosts (cool30 ↔ cool100) becomes a
  natural extension of the donor mechanism (§sec:exec:donor) rather than a
  separate code path.
- The asset-overlap batching mechanism (RQ-F3) extends naturally: `asset:`
  joins `hf:` as a co-batching key without bespoke handling.

This is a multi-PR data-model change touching placement, prewarm, donor,
cloud_provision_guard, eviction, and at least the cloud and on-prem staging
paths. The shipped `named_assets` table is shaped to extend additively: an
`asset_locations(asset_id, transport, path, host)` table sits on top of it
without migrating existing rows.

Bears on **RQ-F4** (where intermediate artifacts should be materialized) and
extends **§sec:planes:data** in the systems paper. Sequencing: defer until
named-assets usage in the lab notebook surfaces concrete cases where the
fixed `assets/<hash>` R2 location is the wrong choice (e.g. an on-prem
producer where R2 round-trips dominate).

## Reconciler hardening (consult on recurrence)

Triggered by an `upload_stall` termination_reason that the Go agent wrote but
the schema CHECK constraint did not allow, leaving wi3226 stuck in
status='running' with six queued jobs unable to re-place. Migration 00008 and
`TestTerminationReasonConstantsMatchSchema` close the specific bug; the items
below would have caught the failure mode itself, not just this instance of
the enum drift. Consult if a similar silent-stuck-instance pattern recurs.

- **Table-driven reconciler integration test over every termination reason.**
  `TestReconcileLaunches_TerminationIntent_DestroysAndMarksFailed` covers the
  full marker → `UpdateLaunchStatus` path but only with `disk_full`. Make it
  table-driven (or reflection-driven) over every exported `TerminationReason*`
  constant, so any reason that round-trips the reconciler is exercised
  end-to-end against a real SQLite. Catches drift the constants/schema test
  cannot — e.g. a new constraint, trigger, or status transition that rejects
  a particular reason.
- **Loud CHECK-constraint handling in `ExecuteAction`.**
  `internal/campaign/instance_check.go:946` emits a `slog.Warn` on
  `UpdateLaunchStatus` failure and returns `(false, false)`, then the
  autopilot retries forever with no oplog entry, metric, or narrative event.
  Detect SQLite constraint violations (error code 275 / message-prefix match)
  in `ExecuteAction` and: (a) emit a distinct oplog entry
  (e.g. `OpReconcileBlocked`) once per launch_id+reason pair so the failure
  shows up in `weft list events` and narratives, (b) optionally re-attempt
  with `TerminationReasonWeftBug` after N retries so the instance still
  drains and jobs are reset — the original reason gets recorded in
  `termination_detail` for postmortem.
## Dashboard: persistent system-state snapshots

`weft dashboard`'s Pulse sparklines (queue depth, running count, $/hr,
failures) currently accumulate in-process, which means each freshly launched
dashboard starts cold and shows a "warming up · N/5" placeholder until enough
samples land. The accumulation should move into the daemon so sparklines
reflect real history the moment the dashboard starts.

- **New table `system_snapshots`** (one row per ~30s tick) — columns: `ts`,
  `queue_depth`, `running_count`, `spend_usd_per_hour`, `failure_count_since`,
  plus a few more derived counters (unplaced, paused). Rough volume:
  ~60 bytes × 2880 samples/day = ~170 KB/day. Optional rollups (`hourly`,
  `daily`) for longer retention without growing the per-second table.
- **Recorder in the daemon**: a small goroutine alongside the existing
  autopilot tick that samples the same `LoadSnapshot` aggregates the
  dashboard already computes, then writes one row per tick.
- **Reader in dashtabs**: replace the in-process `RecentHistory` accumulator
  with `db.ListRecentSystemSnapshots(60)`. Removes the warm-up state and the
  ring-buffer code in `internal/ui/dashtabs/snapshot.go`.
- **Reusable beyond sparklines**: weekly/monthly spend trend reports;
  anomaly detection ("queue depth spiked 3σ above baseline"); post-mortems
  and lab notebooks ("what was queue depth when EXP-079 started failing");
  regression detection on placement decisions.
- **Schema-drift considerations**: standard `goose` migration with
  appropriate indexes (`ts DESC`); also a rollup migration that derives
  hourly/daily means from the per-second table so old rows can be pruned.
- **Degraded UX when daemon not running**: the dashboard's existing
  in-process accumulator stays as fallback for users who don't run autopilot.

## Dashboard: per-job CPU/GPU sparklines in Focus

The Focus tab's running-job cards currently show progress, ETA, host, GPU,
elapsed, and cost. We already have rich per-job telemetry in
`job_telemetry_samples` (CPU user/sys, RSS, host CPU util, disk IO) and
`job_timeseries` (cpu_pct, gpu_util_pct, gpu_mem_used_mib). Adding small
sparklines to each Focus card from this existing data would make the view
genuinely useful for "is wj2257 making progress or stuck."

- **Read path**: `internal/jobtelemetry/` package with
  `LoadSamples(database, jobID, window)`; called once per Focus render for
  each running job. Indexes on `(job_id, ts DESC)` exist.
- **Render**: stack 2-3 mini-sparklines (GPU util, host CPU, progress %) at
  the bottom of each focusCard.
- **Cost**: cheap — telemetry table is per-job-indexed; reading the last
  hour of samples for ≤5 running jobs is sub-millisecond.

- **Audit the agent ↔ schema seam.** The agent writes termination intents to
  R2 with reason strings that the coordinator later inserts into SQLite.
  `cmd/agent/upload_drain_test.go::TestRecordDrainOutcomeStallTriggersSelfDestruct`
  stops at the `uploadStallSelfDestructHook` boundary because the real path
  shells out to rclone. Either (a) drive the post-hook path with a fake
  `r2Put` and a fake reconciler, or (b) add a contract test that asserts
  every reason string passed to `terminateInstanceWithReason` is in
  `IsRetryableTermination`'s switch (and, transitively, in the schema). The
  current test suite never crosses the agent → DB boundary for any reason
  the agent emits.

## Spec coverage for agent subsystems

`specs/upload-drain.allium` is the first Allium spec that covers an
agent-internal subsystem rather than coordinator-visible lifecycle. It was
written after the wi3333 / wi3334 upload-stall regression, which would have
been prevented by the spec's `HeartbeatResetsOnAnyStderrLine` invariant.
The agent has accumulated several other subsystems whose behavior currently
lives only in code comments and which would benefit from the same
treatment. Each is small enough (~300-500 spec lines) to be written and
kept current.

- **`specs/agent-jobloop.allium`** — per-job phase transitions inside the
  agent (setup → running → finalizing → uploading), how the agent decides
  to pull the next job vs. enter grace, prior-job-upload barrier behavior
  before workdir reuse (`eec0475c9`), exit-code → DB-status mapping.
  Implementation anchors: `cmd/agent/jobloop.go`, `cmd/agent/runinstance.go`.
- **`specs/agent-grace-wait.allium`** — the R2-polling grace period after
  job failures, the `submit` / `extend` / `release` control-message
  protocol, the deadline / extend-default semantics, what counts as a
  terminal state for the grace loop. Implementation anchors:
  `cmd/agent/gracewait.go`, `cmd/agent/r2ops.go`,
  `internal/r2keys/keys.go` (grace/ keyspace).
- **`specs/agent-heartbeat.allium`** — the three trigger conditions for
  `AgentHeartbeatTick` (startup, phase transition, 30s tick), the
  separate `last-seen` liveness key that survives nvidia-smi hangs,
  what fields are sampled vs. skipped on disk-full or GPU-probe
  failures. The current `AgentReportsHeartbeat` rule in
  `status-sync.allium` documents the *contract* but not the *policy*.
- **`specs/agent-prewarm.allium`** — cloud-job prewarm pipeline
  (`518bd4d9c`), setup-timeout classification as `infra_failure`
  vs. `job_failure` (`4d14ac08e`), HF input bootstrap on cloud
  (`d24dab851`). Particularly worth a spec because failure
  classification here gates whether the autopilot retries on a
  different instance or marks the job as user-broken.
- **`specs/agent-onstart-probe.allium`** — the OnStart probe path
  (`cf49c7766`), what disambiguates "OnStart never ran" from "OnStart
  ran but bootstrap failed", the bootstrap-stage progression
  surfaced in `weft instance diagnose`.

Priority order is roughly the order of past production surprises:
jobloop and grace-wait first (most cross-coupling with coordinator
state), then heartbeat (rarely changes, well-understood), then
prewarm (newer, still evolving), then onstart-probe.

Each new spec should follow the upload-drain template: header noting
what the spec is *for* and what it explicitly excludes, value types
for the subsystem's public surface, invariants captured as regression
guards (referencing the incident that exposed them when applicable),
rules with `when:` / `ensures:` blocks, `@guidance` blocks pointing
to implementation anchors. Cross-reference via `use "./other.allium"
as alias`.
