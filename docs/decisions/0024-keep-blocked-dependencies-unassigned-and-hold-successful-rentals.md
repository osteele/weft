---
status: accepted
date: 2026-08-30
---

# 0024. Keep blocked dependencies unassigned and hold successful rentals

## Context and Problem Statement

A research sweep may be submitted behind a strict canary dependency. Submitting
the whole DAG exposes future demand early, while the canary prevents expensive
work from running before validation succeeds.

The canary's rental may already contain the right environment, model, and data
for part of the sweep. It previously self-destructed immediately after success,
often before the dependency cleared and autopilot could reuse it. Assigning the
blocked descendants early with `cloud_after` looks smaller, but creates real
attempts and launch claims before validation, complicates moves and retries,
and turns a placement preference into authoritative execution state.

## Decision Outcome

Strictly blocked descendants remain unassigned and create no execution attempt.
They are visible to planning as deferred demand. When a live producer rental is
compatible with that demand, Weft records a durable, bounded success-handoff
lease through the existing R2 control plane.

After a successful producer completes, the agent enters an explicit `handoff`
phase instead of immediately self-destructing. The normal planner then chooses
which now-runnable jobs reuse that rental and which use fresh capacity. The
agent terminates when the lease expires, is released, or the rental time budget
ends. Failure grace and success handoff remain distinct states.

A terminal unsuccessful strict dependency marks unplaced descendants
`skipped`, transitively and without creating another attempt. Retryable
infrastructure outcomes leave descendants deferred. `--after-any` continues to
clear on every terminal outcome.

The living specifications own the scoring function and lease duration. This
record fixes only the state boundary: planning demand is not placement, and
successful retention is not failure grace or `cloud_after`.

### Consequences

- Canary failure cannot accidentally run or create retry history for the sweep.
- Successful rentals remain available long enough for locality-aware reuse.
- Jobs not selected for reuse retain ordinary move, fan-out, and operator
  control because they were never claimed.
- The controller and agent require R2 for the durable handoff protocol; without
  it, successful teardown retains the prior immediate behavior.
- Very short canaries still require the descendants to be submitted before the
  controller can observe their deferred demand.

## Considered Options

### Pre-place blocked descendants with `cloud_after`

Rejected because a soft reuse preference would become an authoritative attempt
and launch membership before the strict dependency succeeds. It also inherits
exact-attempt pinning and unknown-marker hazards from the data-dependency wire
protocol.

### Reuse failure grace after a successful canary

Rejected because failure recovery and successful scheduling handoff have
different meanings, controls, and status expectations.

### Always terminate and launch fresh capacity

Rejected because it discards already-paid setup and shared-input locality even
when serial reuse is the best plan.

## More Information

- `DeferredDependencyDemandAndSuccessHandoff` and
  `TerminalStrictDependencySkipsDescendants` in
  `specs/campaign-lifecycle.allium`.
- `DeferredDemandInfluencesPlacementWithoutClaiming` in
  `specs/inventory-placement.allium`.
