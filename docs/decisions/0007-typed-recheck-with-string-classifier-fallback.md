---
status: accepted
date: 2026-08-18
---

# 0007. Type the recheck cadence, keep the string classifier as fallback

## Context and Problem Statement

The autopilot dispatcher wakes on observed database changes and otherwise
stays quiet, so a blocked pass must declare what could change its answer: a
fresh provider query (market), a local deadline elapsing (backoff), or a
database write the wake snapshot observes (nothing to poll). Deriving this by
classifying reason *strings* (`blockreason.Recheck`) meant rewording a message
silently changed retry cadence, and the classifier had to be one-sided —
anything unrecognized keeps the timer — because failing to classify a reason
is not evidence the market is irrelevant to it.

## Decision Outcome

The recheck class is a property of the typed `BlockerKind` recorded with each
verdict (market, budget, backoff, precondition), settled onto the pass result
and folded by `ClassifyPass` into refined blocked outcomes
(`blocked-market`, `blocked-deadline`, `blocked-none`) that `TimerRunsPass`
keys on. The string classifier is **retained**, in two roles that must not be
removed as dead code:

- **Boundary fallback**: `KindUnclassified` blockers (raw external strings —
  provider errors, relaunch event reasons) and results with no settled typed
  need still classify through `Recheck`/`RecheckFor`, preserving the
  one-sided safety net: unrecognized keeps the timer.
- **Nested-countdown rule**: a backoff countdown inside a reuse diagnostic
  names a clock gating the reuse avenue even when the primary blocker is
  database-observable. `VerdictBuilder.Need` raises such a verdict to
  deadline via the countdown sniff, mirroring the whole-reason string rule.
  Dropping this "redundant" check strands a job whose budget-blocked launch
  hides an elapsing reuse backoff until the quiet backstop.

### Consequences

- Rewording a reason produced by a typed write site no longer changes
  cadence; rewording one that flows through `KindUnclassified` still can.
- `weft autopilot run --json` emits the refined outcome strings (all
  prefixed `blocked`) for typed passes; consumers switching on the exact
  string `blocked` see only untyped-fallback passes.
- Deliberate cadence fixes over the string path: checkpoint-deferred,
  dependency, and source-too-large blockers are database-observable
  (`blocked-none`) and no longer poll at market cadence; the 10-minute quiet
  backstop bounds any residual gap.

## Considered Options

### Delete the string classifier once kinds exist

Rejected: external reasons nobody typed still arrive as strings, and the
one-sided default (unrecognized → market) is the safety net the spec calls
for.

## More Information

- **Builds on**: [0006](0006-settle-blocked-reason-verdicts-through-one-typed-builder.md)
- **References**: internal/orchestration/dispatch.go; internal/blockreason/market.go
