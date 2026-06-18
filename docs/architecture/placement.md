# Placement Architecture

How weft decides where a job runs. Authoritative behavior lives in
`specs/inventory-placement.allium` (constraints, eligibility, scoring, data
locality) and `specs/campaign-lifecycle.allium` (rental launch gates); this
document is the developer's map of the moving parts.

## Constraint derivation

A job's placement constraints combine user-declared requirements with
inference from the project's Python environment.

**User-declared**: `--gpu`/`--gpu-class`, `--gpu-mem` (with `--gpu-mem-strict`
resolved into the effective memory value at submit, never reaching placement),
provider and behavior tags, `--cuda-driver-min`, and PEP 723 `[tool.weft]`
metadata (`gpu`, `gpu-mem`, `gpu-arch-max`, `cuda-driver-min`, `min-driver`,
`tags`).

**Inferred from the torch pin** (`dataloc.ScanTorchPin` reads `uv.lock`,
falling back to `pyproject.toml`):

- *Arch cap* (`max_compute_cap`): the highest CUDA compute capability the
  pinned wheel ships kernels for (`placement.TorchMaxComputeCap`). Persisted
  on the job as a three-state encoding: `"X.Y"` (bounded), `"any"`
  (explicitly unbounded via `gpu-arch-max = "any"`), `""` (unresolved; lazily
  backfilled by the cloud planner).
- *Arch floor* (`min_compute_cap`): the lowest capability the wheel still
  supports (`TorchMinComputeCap`) — modern wheels drop `sm_<7` kernels.
- *CUDA/driver floor* (`min_cuda_version` / `min_driver_version`): resolved
  by `placement.MinRuntimeFloorForJob` with provenance
  (`RuntimeFloor.CUDAOrigin`). The torch-pin contribution is the CUDA
  **family** floor (cu12x → `12.0` → driver ≥525): pip wheels bundle their
  CUDA runtime and run on any same-major driver under minor-version
  compatibility. Library dependency floors (e.g. vLLM) are cited toolkit
  requirements and stay exact, as do image-label requirements on the cloud
  path (a deliberate asymmetry — see the comment in
  `campaign.ResolveJobImageSettings`).

**Override hierarchy** for the CUDA/driver floor, lowest to highest
precedence: inferred (torch family ⊔ library floors, max-merged) →
`.weft.toml [cloud]` → script `[tool.weft]` → CLI `--cuda-driver-min`. Each
explicit level **replaces** the floor (it may lower it); `"any"` clears it.
An explicit `min-driver` is preserved exactly, never raised by the
CUDA-implied backfill (`RuntimeFloor.ApplyExplicit` / `FinalizeDriver`).
`gpu-arch-max` similarly overrides the inferred arch cap, with `"any"`
disabling it.

All torch-derived GPU-runtime constraints apply **only to jobs that request a
GPU** (`db.Job.RequestsGPU`: gpu-class or gpu-mem set). CPU-only jobs carry
none of them regardless of the project's torch pin.

### Shared resolution path

Submit-time placement, persisted-job placement, launch grouping, and
existing-instance reuse all resolve through `placement.ResolveConstraints`.
Callers adapt either CLI inputs or a `db.Job` into `placement.ConstraintSource`;
the resolver applies GPU-runtime inference and CLI/metadata override precedence
once, returns the `placement.Constraints` used by scoring, and returns the
three-state `max_compute_cap` value that should be stored on the job. If a
persisted concrete cap disagrees with the current source tree, the fresh
derived cap wins and launch grouping persists it back.

## Eligibility and scoring

`placement.scoreHost` evaluates one host against one constraint set,
producing a `Score` with `Eligible` and human-readable `Reasons` (each
prefixed with the constraint kind: `driver floor:`, `CUDA floor:`,
`arch cap:`, `arch floor:`). Hard gates, in order: cordon, opt-in-only,
host overload (`AssessHostLoad`; "busy" is a soft penalty), then
`CheckHostGPUConstraints` (class, memory, arch cap/floor, CUDA/driver
floors). Soft scoring adds data locality (HF cache hits via
`internal/dataloc`), load penalties, performance factors, and
predictor-based runtime estimates.

**Fail-open vs fail-closed**: the arch *cap* passes GPUs with unknown
capability (cannot prove violation); the arch *floor* and the CUDA/driver
floors fail closed (cannot prove compatibility — see the EXP-179 regression
notes in the spec). The cloud-offer filter
(`campaign.filterOffersByTorchArch`) is fail-closed on both bounds.

**Tags drive placement**, not just labeling: `rental` / `inventory` route to
cloud or on-prem; `provider:<name>` pins the rental provider;
`cpu-intensive` (canonicalized from legacy `compute-intensive` via
`db.CanonicalizeTag`) changes scoring and adds a 30-minute spill margin
before renting; `benchmark` requires an idle host; `interruptible` permits
preemptible rentals. See `docs/guides/placement.md` for the user-facing
rules.

## Placement entry points

| Entry point | Code | Notes |
|---|---|---|
| `weft run` (submit) | `cmd/run.go` | Persists constraints; `--dry-run` previews per-host scores; auto-placement defers to the autopilot |
| Autopilot tick | `internal/orchestration/autopilot.go` | The placement engine: reuse pass, rebalance, launch planning, relaunch |
| Reuse pass | `campaign.PlanReuse` + `submitAutoPilotReuseAssignments` | Validates job sources **before claiming** (`campaign.ValidateJobSourceForCloud`), backs off after consecutive submit failures (`reuse.submit_failed` streak + `retrypolicy.BackoffDelayClamped`) |
| New rentals | `weft start instance` / autopilot launch planner | Offer search + filters in `internal/campaign/offers.go` |
| Manual moves | `weft job move` | Bypasses autopilot gating (no backoff) |

The autopilot is a singleton coordinated via `weft autopilot status/pause/
resume`; manual launches should check it first (see
`.claude/skills/autopilot-coordination.md`).

## Observability

What persists vs. what is in-memory only:

- **`jobs.placement_reasons`** (flat history, deduped against exact repeats)
  and **`jobs.placement_blocked`** (`blockreason.Structured` JSON, refreshed
  every autopilot pass): the authoritative "why is this job unplaced"
  record, rendered by `weft info`, the TUI expand view, and `weft explain`.
  The structured form carries the launch-avenue reason, per-instance reuse
  rejections (recorded submit/match failures take precedence over re-probed
  ones — `MergeRecordedReuse`), and the per-host on-prem rejection detail
  (`Structured.OnPrem`).
- **`lifecycle_events`**: append-only history (`queue.dispatch.*`,
  `relaunch.*`, `reuse.submit_failed/ok`, `reuse.skipped.backoff`,
  `reconcile.*`) consumed by `weft explain` and the backoff derivations.
- **oplog**: operational trace (`auto_pilot.*`), human-readable, not queried
  by display surfaces.
- **In-memory per pass**: the autopilot's working maps (blocked reasons,
  reuse diagnostics) — flushed into the persisted forms by
  `finalizeUnplacedBlockedReasons` at the end of each pass.

`weft info <job>` shows the GPU constraint, the arch cap (GPU jobs only),
and the derived driver/CUDA floor with provenance ("Driver floor: >=525
(CUDA >=12.0, from torch 2.9.1+cu128)") — the floor, not the cap, is what
usually rejects hosts.

## File map

| Package | Role |
|---|---|
| `internal/placement` | Constraints, eligibility, scoring, torch-compat inference (`torch_compat.go`), GPU class/generation parsing (`gpugen.go`), host load (`overload.go`) |
| `internal/inventory` | Host capability records (`~/.config/weft/hosts/*.yaml`), GPU specs, test fixtures |
| `internal/dataloc` | Torch pin scanning, CUDA variant/family floors, script metadata (PEP 723), HF data locality |
| `internal/imagereq` | CUDA-toolkit→driver floor table, image-label requirement resolution (raise-only merges) |
| `internal/gpucatalog` | GPU model → compute capability catalog |
| `internal/campaign` | Offer filtering, instance grouping, reuse matching/submission, source validation |
| `internal/orchestration` | Autopilot tick, blocked-reason finalization, reuse backoff |
| `internal/blockreason` | Structured blocked-reason model + rendering |
| `internal/explain` | `weft explain` synthesis from job state + lifecycle events |
