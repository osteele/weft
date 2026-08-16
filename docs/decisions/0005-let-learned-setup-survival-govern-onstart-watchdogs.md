---
status: accepted
date: 2026-08-16
---

# 5. Let learned setup survival govern the OnStart watchdogs

## Context and Problem Statement

Commit `c352be9d6` ("campaign: prefer learned survival thresholds over
hardcoded kill timeouts", jj change `uxltszlx`) made the reconciler's watchdog
rules prefer learned survival terminate thresholds over fixed constants. For
the two OnStart windows — the stall window (stage marker frozen on an
in-progress install step) and the total-active cap (probe seen, no bootstrap
handoff, regardless of marker freshness) — the effective threshold is now
`SetupSurvival.Terminate` when the setup survival curve has learned one;
the constants `onStartStallTimeout` (10 min) and `onStartTotalActiveTimeout`
(25 min) survive as fallbacks.

As first written the preference was gated on a non-zero threshold value, which
is never false: `ComputeSetupSurvival` and `ComputeBootstrapSurvival` return a
populated struct carrying *default* thresholds when history is insufficient,
recording that in a provenance flag rather than a zero value. Every watchdog
window on a database without history was therefore silently stretched to the
20-minute (bootstrap) or 25-minute (setup) default, and the constants were
unreachable. That was a defect, not a decision, and it is fixed. Thresholds are
now a `db.Threshold` value carrying the duration together with its provenance:
enforcement calls `Learned()`, which reports nothing for a default, while
display calls `Duration()` and `IsLearned()`. This record concerns only the
behavior that remains — what happens when a threshold genuinely was learned.

Even so, there is a consequence the original constants were tuned to prevent.
The total-active cap exists because of incident wi5105 (2026-07-12): a RunPod
OnStart looped `started↔deps-ready` for ~90 min ($1.02), refreshing its stage
marker on every loop so the stall window never fired. The 25-minute cap
bounded that bleed. Under the new scheme:

1. **Both windows collapse to one duration.** With learned data, the stall
   window and the total-active cap both resolve to the same
   learned `SetupSurvival.Terminate`; they differ only in anchor (last marker
   change vs first probe sighting). The deliberate "cap set generously above
   the stall window" margin holds only in the fallback case.
2. **The shared duration can be learned up to ~60 min.** Setup survival
   ignores observations beyond `MaxTimeSecs` (3600 s), so a project with
   genuinely long setups (large `uv sync`, multi-GB model downloads) can
   learn a terminate threshold well above 25 min. A wi5105-style looping
   OnStart on such a project can now bleed for up to roughly an hour before
   the cap reaps it, versus a hard 25 min before.
3. **The learning is blind to the failure mode it now governs.** Setup
   survival is computed from job setup phases (`setup_start`/`setup_end` in
   `job_phase_timings`). A looping OnStart never hands off to bootstrap.sh,
   so no job setup ever starts and the incident contributes zero
   observations. The threshold cannot adapt downward in response to the very
   incidents the watchdog exists to catch. The learned-only gate bounds this:
   a project with no setup history keeps the 25-minute constant, so the
   exposure is limited to projects whose own history shows setups genuinely
   running that long.

Should the total-active cap be exempted from the learned threshold (or
separately capped), or is the weakening acceptable?

## Decision Drivers

- **False kills are the costlier error.** Killing a legitimately slow but
  progressing OnStart wastes the whole rental and requeues the job onto a
  fresh offer, repaying the entire bootstrap cost — usually more than the
  bleed it prevents. The learned threshold exists because fixed constants
  were false-killing slow-but-healthy launches.
- **The exposure is bounded and small in dollars.** The worst case adds
  ~35 min over the old cap. At typical interruptible rental rates
  (~$0.25–1.00/hr) that is cents to well under a dollar per incident, and
  the looping-OnStart signature has been observed rarely (one recorded
  incident).
- **A threshold learned high is evidence, not noise.** A learned terminate
  threshold exceeds 25 min only when historical setups for that command/workdir
  actually run that long with P(success) still above 3%. For exactly those
  projects, the old 25-minute cap was the miscalibrated value.
- **The looping case is still caught.** The total-active anchor
  (`FirstOnStartProbeSeenUnix`, set once) is churn-immune; only the deadline
  moved, not the detection mechanism.

## Considered Options

- **Accept the weakening** — learned setup survival governs both OnStart
  windows uniformly.
- **Exempt the total-active cap** — keep `onStartTotalActiveTimeout` as a
  hard ceiling (`min(learned, 25 min)` or unconditionally fixed) while the
  stall window uses the learned value.
- **Learn a dedicated OnStart curve** — model probe-to-bootstrap-handoff
  times directly instead of borrowing the setup curve, so the governing data
  matches the governed phase.

## Decision Outcome

**Accept the weakening.** Both OnStart windows use the learned setup survival
terminate threshold when available, with the constants as fallback, exactly
as implemented. No special case is carved out for the total-active cap:
a `min(learned, constant)` clamp would silently reintroduce the false-kill
behavior for long-setup projects, which is the failure mode the learned
thresholds were adopted to fix, and it would do so precisely where the
learned data says the constant is wrong.

A dedicated OnStart survival curve remains the better long-term fix for the
blind spot in driver 3 — it is the same shape of gap already recorded for dud
detection (borrowed curve, see the `Open:` note in
`specs/campaign-lifecycle.allium` § DudVastDetection) and should be addressed
with it if the looping signature recurs. Recurrence is the trigger for
revisiting: a second wi5105-class incident on a long-setup project supersedes
this record.

### Consequences

- Worst-case bleed for a looping OnStart rises from 25 min to ~60 min
  (`MaxTimeSecs`), but only on projects with enough setup history to learn a
  threshold, and only where that history puts it that high. Projects without
  learned data are unaffected.
- Long-setup projects stop being false-killed at 25 min — the benefit side
  of the same change.
- The same learned-only gating was subsequently applied to the setup-stall
  watchdog, where the effect ran the other way: its unlearned defaults were
  *tighter* than its constants, so gating on provenance loosened termination
  from 25 to 45 minutes rather than tightening it.
- The governing threshold does not learn from looping-OnStart incidents
  (they produce no setup observations), so monitoring for recurrence is
  manual: `weft instance audit` / incident review, not the survival model.
- `specs/campaign-lifecycle.allium` § OnStartChainWatchdog documents the
  collapsed-windows behavior and links here.
