---
status: accepted
date: 2026-08-18
---

# 10. Cap the borrowed curve in the pre-running watchdogs

## Context and Problem Statement

Two more watchdogs take `BootstrapSurvival.LearnedTerminate()` unclamped:
the empty-status timeout (legacy constant 1 minute) and the
stale-non-running-status timeout (5 minutes). On a mature provider curve both
became roughly 110 minutes.

[0008](0008-cap-the-borrowed-curve-in-dud-detection.md) established the test
for whether a borrowed curve may govern a rule: does the curve measure the
event the rule adjudicates? Here it does not, and the mismatch is sharper than
in the dud case. `ComputeBootstrapSurvival` measures from
`COALESCE(provider_running_at, launched_at)` to the first job's wrapper start,
so for the successful observations that set its tail the clock starts when the
provider reports `running`. Both of these rules are waiting for events that
happen *before* that: one for the provider to report any status at all, the
other for it to reach `running`. The intervals are disjoint. The curve does not
merely fit badly; it describes a period that begins only after the event these
rules are waiting for has already occurred.

The constants are also wrong, in the other direction. Measured over n=4270
launches, `launched_at`→`provider_running_at` runs p50 1.9 min / p90 5.7 min /
p99 23.0 min on vast.ai and p50 0.8 / p90 4.7 / p99 25.8 on RunPod. The
5-minute pre-running constant sits near p88: 12.5% of vast.ai launches take
longer than that and go on to run normally. Reverting to the constants would
false-kill an eighth of the fleet, which is why the learned substitution was
adopted.

So neither the constant nor the borrowed quantile is right, and the choice is
what ceiling to put between them.

## Decision Outcome

**Clamp both to ceilings derived from the pre-running distribution rather than
from the borrowed curve: 10 minutes for empty status, 30 minutes for stale
non-running status.**

The two ceilings differ because the events do. Reaching a non-empty status is
a strict prefix of reaching `running`, and it is an API-propagation event
rather than a provisioning one, so the pre-running distribution bounds it from
above and 10 minutes clears its p90 with margin. The stale-non-running window
covers provisioning and image pull, genuinely slower work, so its ceiling sits
above the observed p99. Against the unclamped learned value, 30 minutes newly
reaps about 0.4% of launches (24 versus 9 of 3864 vast.ai launches took longer
than 30 and 110 minutes respectively) and reclaims roughly 80 minutes of
rental on each wedged one.

Clamping the dud window in 0008 had a consequence that only surfaced here.
`instance.status` becomes `running` when the launch call returns — "instance is
now self-starting" — not when the provider reports running, so weft routinely
holds `status=running` over an instance the provider still lists as `created`.
Rule 4d never checked the provider's own status despite documenting that it
applies when "the provider reports the rental as running". At the old
110-minute window that latent mismatch was harmless, because the pre-running
tail fitted inside it. At 15 minutes it is not: a still-provisioning instance
past the dud window would be destroyed as a dud. **Rule 4d now stands down when
the provider positively reports a non-running, non-empty status**, leaving
those instances to the rules that own them by status: the stale-non-running
rule for the pre-running ones, provider-dead for any the provider reports as
terminal. The guard is
scoped to a positive reading; an unknown provider status is left to the
unknown-streak bound, which is a designed escape hatch for interruptible
instances whose polling is failing.

### Consequences

- Three hand-tuned ceilings now exist where there was one learned threshold,
  each pinned to a distribution measured on one date. They will drift as
  provider behaviour changes and nothing recomputes them. The percentiles are
  recorded here and in the constants so the next reader can re-derive rather
  than guess.
- Roughly 0.4% of launches that would eventually have reached `running` are
  now reaped in the pre-running window and relaunched.
- Rule 4d no longer fires for instances the provider reports as pre-running.
  If the stale-non-running rule is ever weakened, that population loses its
  adjudicator, and the loss will not be visible from 4d's own tests.
- The `launching_phase_timeout` calibration in the spec was found stale — it
  claimed no launch had ever exceeded 10 minutes to reach `running`, which
  5.0% of vast.ai launches now do. The note is restated, but the 12-minute
  constant itself is left alone; it sits near p95 and is a fallback that the
  learned threshold normally displaces.
- `effectiveLaunchingPhaseTimeout` is a fourth consumer of the same borrowed
  curve and is deliberately left unclamped, so in normal operation rule 4c
  runs at the learned value over a window whose p99 is ~23 minutes. Its
  failure direction is over-permissive rather than false-killing — a wedged
  pre-launch rental burns the window instead of a healthy one being reaped —
  and every launch it governs lacks a `BootstrapOrigin`, so no measured
  distribution anchors a ceiling for it. Clamping it needs its own
  measurement, not a borrowed one.
- A dedicated pre-running survival curve would retire all of this, and is now
  the third place that answer has been deferred (0005, 0008, here).

## Considered Options

### Remove the learned preference and restore the constants

Rejected on measurement. The 5-minute constant reaps 12.5% of vast.ai launches
that go on to run, and the 1-minute one is tighter still. The learned
substitution exists because these constants were false-killing; reverting
would reinstate exactly that.

### Clamp both to the same ceiling

Rejected: it would either take the empty-status window far past what an API
status propagation can justify, or squeeze provisioning and image pull into a
window the data says is too tight. One ceiling cannot serve two events an
order of magnitude apart.

### Widen the dud window again instead of guarding rule 4d

Rejected: it trades a correct fast detector for a latent-bug workaround. The
dud window is calibrated to probe arrival and should stay there; the actual
defect was that 4d never checked the provider status its own documentation
claimed as a premise.

## More Information

- **Builds on**: [0008](0008-cap-the-borrowed-curve-in-dud-detection.md)
- **References**: `specs/campaign-lifecycle.allium` § DudVastDetection,
  § StaleNonRunningStatus, and the `launching_phase_timeout` calibration note;
  `db.ComputeBootstrapSurvival`
