---
status: accepted
date: 2026-08-18
---

# 0006. Settle blocked-reason verdicts through one typed builder

## Context and Problem Statement

An autopilot pass records why each still-unplaced job could not be placed.
These reasons come from different origins with different authority: planner
verdicts, run-rate budget gates, retry cooldowns, dependency preconditions,
and opportunistic reuse diagnostics that must never masquerade as the reason a
job is unplaced. The spec rule `AutoPilotBlockedReasonIsAuthoritative`
(specs/campaign-lifecycle.allium) states the precedence, but for as long as
the pass threaded five parallel per-job maps, the rule was enforced only by
per-site discipline — "set only if absent" guards at every write site and
sequential reattach loops at persist time. Each new write site had to
rediscover the discipline, and a missed guard silently masked the
authoritative reason.

## Decision Outcome

Blocked-reason state accumulates in `blockreason.VerdictBuilder`, the
unsettled form of the persisted `blockreason.Structured` breakdown, and one
`Settle` function produces the flat reason and structured form under the
spec's precedence. Write sites choose a typed entry point — `SetLaunchBlocker`
(first authoritative reason wins), `AddBlocker` (later gates join as secondary
fragments), `ReplaceLaunchBlocker` (a fresher re-evaluation supersedes),
`AddReuseDiagnostic` (trailing detail only) — instead of writing map entries.
The pass epilogue settles every candidate's verdict and asserts each planned
candidate ended classified.

The builder was deliberately built *around* `Structured` rather than as a
parallel verdict type wrapping it: `Structured` is already the settled
primary+secondary form persisted to `placement_blocked`, and a second verdict
shape alongside it would split the precedence across two types again.

### Consequences

- A new blocked-reason source must pick a typed entry point and a
  `BlockerKind`; there is no map to write directly. That is the point: adding
  a direct map write next to the builder reintroduces the masking bug class
  this structure removed.
- The precedence rules are unit-testable in one place
  (internal/blockreason/verdict_test.go) instead of being asserted only
  through full-pass integration tests.
- `finalizeUnplacedBlockedReasons` needs a probe callback injected because
  `blockreason` stays free of DB and orchestration imports.

## Considered Options

### Keep the parallel maps and document the discipline harder

Rejected: the ~90-line guidance block in the spec rule *was* that
documentation, and the reattach loops still had to be kept in order by hand.

### A verdict struct wrapping `*Structured` as a field

Rejected: duplicates the primary+secondary shape `Structured` already has,
leaving two types to keep consistent.

## More Information

- **References**: specs/campaign-lifecycle.allium §
  `AutoPilotBlockedReasonIsAuthoritative`; internal/blockreason/verdict.go
