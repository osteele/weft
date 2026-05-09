# Estimation Boundaries

This document defines the intended division of labor between the three
estimation-related projects that `weft` depends on.

## Project Roles

### `llm-performance-models`

`llm-performance-models` owns deterministic analytical modeling:

- hardware and machine resolution
- first-principles training and inference estimates
- analytical peak-memory estimates
- bottleneck-oriented model features derived from hardware specs

It should answer questions such as:

- "Does this workload fit on this GPU?"
- "What is the analytical runtime on this GPU class?"
- "What hardware property explains the estimate?"

It should not own scheduler policy, cloud pricing, or ranking across offers.

### `job-estimator`

`job-estimator` owns workload interpretation and learned prediction:

- parsing commands into workload features
- producing stable workload fingerprints and script-family labels
- calling `llm-performance-models` to derive analytical features per candidate GPU
- fitting and serving learned predictions for duration, peak RSS, and GPU memory
- model schema versioning, retraining, cache compatibility, and freshness reporting
- model-side evaluation, calibration, uncertainty, and explanation metadata

It should answer questions such as:

- "How long is this command likely to run on this candidate GPU?"
- "How much GPU memory is it likely to use?"
- "How confident is that prediction?"
- "Are these trained artifacts ready, stale, or blocked by a schema mismatch?"

It should not own cloud-offer search, retry budgeting, or launch strategy.

### `weft`

`weft` owns placement, launch, and economic decisions:

- grouping jobs into execution plans
- finding cloud offers and reusable instances
- hard feasibility filters such as minimum VRAM and CUDA compatibility
- setup, transfer, wait, and survival modeling
- phase timing, failure/censoring, and telemetry export for estimator training
- recent local residual correction for operational drift
- final ranking on predicted runtime, cost, and reliability

`weft` should treat estimator-produced runtimes as authoritative when they are
available. It should not infer that a larger-memory GPU is faster just because
its `DLPerf` or VRAM tier is higher.

`weft` should also treat `job-estimator status` as the source of truth for
model freshness and schema readiness, instead of recomputing freshness from
local row counts alone.

When estimator runtime metadata is available, `weft` is also responsible for
using it consistently across execution paths:

- new-offer ranking and reusable-instance scoring should use the same
  semantics-aware runtime adjustment path
- low-confidence predictions with `bottleneck=unknown` should be shrunk toward
  a neutral baseline
- explicit `memory_capacity` signals may justify paying for a larger GPU
- `benefits_from_additional_vram=false` should suppress oversized-GPU wins when
  the predicted speedup is otherwise weak
- residual-corrected durations should carry metadata that identifies the
  correction source and factor

## Fallback Policy

When `job-estimator` cannot provide a runtime:

- `weft` may still use estimator-produced or declared memory information for feasibility
- feasible GPUs should default to equal runtime unless there is explicit measured evidence otherwise
- price, setup overhead, and reliability should break ties

This keeps memory capacity as a fit constraint instead of a scheduler-owned
speed heuristic.

## Current Direction

The implementation target is:

1. `job-estimator` predicts per-candidate runtimes and memory use.
2. `weft` ranks both new offers and reusable instances from those predictions.
3. `weft` applies the same semantics-aware runtime adjustment before scoring
   either path.
4. `weft` falls back to neutral runtime assumptions instead of `DLPerf`-based
   speed guesses.

That keeps analytical and learned runtime modeling in the estimator stack while
leaving orchestration and economic tradeoffs in `weft`.
