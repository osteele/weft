---
status: accepted
date: 2026-08-23
---

# 0019. Admit concurrent jobs with host-local RAM reservations

## Context and Problem Statement

The placement RAM floor proves that one job can fit a host, but does not prevent
several individually eligible jobs from exceeding physical RAM together. Live
usage alone also has a race: a newly started model has committed future memory
before those pages become resident.

## Decision Outcome

The host-resident queue runner admits starts against a derived RAM commitment.
Every queue payload carries the larger of the effective declared CPU-memory
floor and the predicted peak-RSS upper bound. Admission combines live host
usage, the unmaterialized remainder of running reservations, and the next
reservation, with a target of 90% of physical RAM.

The calculation is derived on every decision. There is no separately mutable
reserve/release ledger. RAM blocking preserves FIFO order, and a missing host
memory probe fails open.

### Consequences

- Concurrent model-load spikes are covered before RSS catches up.
- Resident pages and unrelated host processes contribute without double-counting.
- Runner restart and old-state recovery do not leak reservations.
- Conservative declarations or prediction bounds can leave capacity idle.
- Jobs without either signal reserve no future growth, although current host
  usage and the 90% target still protect a loaded host.

## Considered Options

### Decide from live free RAM only

This misses the interval between admitting a job and its memory becoming
resident.

### Keep a persistent reserve/release ledger

This introduces reconciliation and leak failure modes without improving the
single-owner runner's decision.

### Let smaller jobs bypass a RAM-blocked head

This is more work-conserving but can starve a large job. Existing FIFO semantics
are deterministic and need no aging policy.
