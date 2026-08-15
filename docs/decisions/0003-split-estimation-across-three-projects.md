---
status: accepted
date: 2026-08-15
decision-date: 2026-04-02
---

# 3. Split estimation across three projects

## Context and Problem Statement

Placement needs answers to several different questions: does this workload fit
on this GPU, how long will it run there, how confident is that estimate, and
which offer is the best buy. Three codebases can plausibly own each answer —
`llm-performance-models`, `job-estimator`, and `weft` — and without an explicit
boundary the same logic gets implemented in more than one of them, or `weft`
invents its own heuristics.

The concrete failure mode this guards against: `weft` inferring that a
larger-memory GPU is faster because its `DLPerf` or VRAM tier is higher. That is
a speed judgement made by a scheduler using a fit signal, and it is wrong
whenever a workload is not memory-bound.

## Decision Outcome

Divide ownership by *kind of question*, not by convenience:

| Project | Owns | Answers |
|---|---|---|
| `llm-performance-models` | Deterministic analytical modeling: hardware resolution, first-principles runtime and peak-memory estimates, bottleneck features | "Does this fit?" "What is the analytical runtime?" "What hardware property explains it?" |
| `job-estimator` | Workload interpretation and learned prediction: command parsing, workload fingerprints, learned duration/RSS/GPU-memory models, schema versioning, calibration, uncertainty | "How long will this likely run?" "How much memory?" "How confident?" "Are the artifacts fresh?" |
| `weft` | Placement, launch, and economics: execution plans, offer and reuse search, hard feasibility filters, setup/transfer/wait/survival modeling, final ranking on runtime, cost, and reliability | "Where should this run, and is it worth it?" |

Three consequences of the split are load-bearing:

- **`weft` treats estimator runtimes as authoritative when available**, and does
  not substitute its own speed heuristics for them.
- **`weft` treats `job-estimator status` as the source of truth** for model
  freshness and schema readiness, rather than recomputing freshness from local
  row counts.
- **When no runtime prediction is available**, feasible GPUs default to *equal*
  runtime and price, setup overhead, and reliability break the tie. Memory
  capacity stays a fit constraint and never becomes a scheduler-owned speed
  proxy.

Neither estimation project owns scheduler policy, cloud pricing, offer search,
retry budgeting, or launch strategy.

### Consequences

- Runtime intelligence is testable in isolation, against its own data, in the
  project that owns it.
- `weft` gets a single place to apply semantics-aware runtime adjustment, used
  by both new-offer ranking and reusable-instance scoring, rather than two
  divergent scoring paths.
- The cost is a cross-repository interface: a `weft` change that needs a new
  workload feature or a new prediction field requires a coordinated change in
  `job-estimator`, and the fallback policy has to stay good enough that `weft`
  keeps working when the estimator is stale, unavailable, or schema-mismatched.
- The boundary is a convention, not an enforced one. Nothing prevents `weft`
  from reintroducing a `DLPerf`-based speed guess; only review does.

## Implementation status

The division of labor is accepted. The implementation target described in
[`docs/design/estimation-boundaries.md`](../design/estimation-boundaries.md)
§ "Current Direction" — estimator-predicted per-candidate runtimes driving both
new-offer and reuse ranking through one adjustment path, with neutral fallback
instead of `DLPerf` guesses — is partially realized; that document is the
current record of where it stands.

## More Information

- [`docs/design/estimation-boundaries.md`](../design/estimation-boundaries.md)
  — full role definitions, fallback policy, and metadata expectations.
- [`docs/reference/estimation.md`](../reference/estimation.md) — user-facing
  behavior of runtime, resource, transfer, and cost estimation.
- [`docs/planning/estimation-services.md`](../planning/estimation-services.md)
  — prospective work against this boundary.
