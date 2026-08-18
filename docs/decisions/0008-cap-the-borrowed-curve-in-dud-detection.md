---
status: accepted
date: 2026-08-18
---

# 8. Cap the borrowed survival curve in dud detection

## Context and Problem Statement

Dud detection (`specs/campaign-lifecycle.allium` § DudVastDetection, rule 4d in
`internal/campaign/instance_check.go`) fires when a provider reports a rental
as running but no sign of life ever appears: no OnStart probe, no bootstrap
activity, no heartbeat, no phase, no stage. It exists to reap structurally
dead rentals in minutes rather than letting them bleed to the adaptive
bootstrap deadline.

Its window came from `dudVastTimeout`, 8 minutes, calibrated to the OnStart
probe's arrival distribution. The probe is the OnStart shell's first line, so
it lands within a couple of minutes of the container starting or it never
lands at all — bimodal, with almost nothing in between.

Commit `c352be9d6` made every watchdog prefer a learned survival terminate
threshold over its constant, and `effectiveDudVastTimeout` took
`BootstrapSurvival.Terminate` unclamped. That curve models a different random
variable: time to bootstrap *completion*, which is long-tailed, and whose
P(success) < 0.03 cutoff sits beyond an hour on a provider with history. The
spec already recorded the mismatch as an open gap — "probe arrival is
~60s-or-never; bootstrap completion is a long tail" — but nothing bounded it.

The gap is not theoretical. Instance wi7309 (2026-08-18) matched rule 4d on
every clause: vast.ai accepted weft's `--onstart-cmd`, stored it in the
instance record, injected `extra_env` into the container, and never
materialised the script onto the filesystem. `/root/onstart.sh` was vast.ai's
82-byte default stub, so no probe could ever be written. The provider's
learned bootstrap threshold was 1h50m, so an 8-minute detector held its fire
for 86 minutes and $3.80 before a human intervened. Two further instances
that week (wi7287, wi7288) burned 108 and 142 minutes on the same signature.

Should the dud window be exempted from the learned threshold, and does doing
so contradict [0005](0005-let-learned-setup-survival-govern-onstart-watchdogs.md),
which considered a `min(learned, constant)` clamp and rejected it?

## Decision Outcome

**Clamp the learned value: the dud window is
`min(BootstrapSurvival.Terminate, dud_vast_timeout_ceiling)`, ceiling 15
minutes.** Learned data may extend the window for a provider whose probes
genuinely arrive late; it may not define it.

This does not contradict 0005. That record governs the *setup*-survival curve
over the OnStart stall window and total-active cap, and rejected a clamp there
for a specific reason: a project with genuinely long setups has history saying
so, the learned value is the calibrated one for that project, and clamping it
false-kills slow-but-healthy launches. Every step of that argument depends on
the learned curve measuring the phase it governs. Here it does not. No
property of a provider makes its OnStart shell's *first line* take an hour to
run, so a high learned bootstrap threshold is not evidence about probe
arrival, and clamping it discards no information about the governed event.

Rule 4d's six-way absence conjunction carries the false-kill risk that the
constant alone would otherwise pose. Firing requires every one of six signals
to be confirmed-absent, with each predicate returning its safe value when the
underlying read fails. A slow-but-healthy launch shows at least one signal
well inside 15 minutes.

The ceiling is an interim bound, not the right fix. A dud-specific survival
curve — modelling probe arrival directly instead of borrowing bootstrap
completion — remains the correct answer, and is the same remedy 0005 names
for its own blind spot.

### Consequences

- A provider whose probes genuinely arrive between 15 minutes and its learned
  bootstrap threshold will now be false-killed. No such provider has been
  observed, and the conjunction makes it unlikely, but the ceiling is a
  standing bet that probe arrival stays bimodal.
- The ceiling is a second hand-tuned constant, and it will drift out of
  calibration the same way 8 minutes did if probe behaviour changes. Nothing
  learns it or checks it.
- Two watchdog families now disagree on whether a borrowed curve may be
  clamped — setup survival may not (0005), bootstrap survival in dud
  detection may (here). The distinguishing test is whether the curve measures
  the event the rule adjudicates; a reader who applies either record without
  that test will get the other case wrong.
- The `Open:` note in § DudVastDetection is narrowed, not closed. Gap (1),
  stratification by data center, is untouched; gap (2), curve fit, is bounded
  rather than fixed.
- Recurrence of a dud-signature bleed past the ceiling supersedes this record
  in favour of a dedicated curve.

## Considered Options

### Remove the learned preference from dud detection entirely

Rejected: it discards a real signal. A provider whose probes are measurably
slower than the fleet should be allowed to move the window, and the learned
curve is the only per-provider input available. Clamping keeps that while
bounding the pathology; removing it re-freezes the window at a constant tuned
against one fleet snapshot.

### Learn a dud-specific survival curve now

Rejected for this change, not on the merits. It is the right long-term fix and
both this record and 0005 name it, but it needs a new observation series
(container-running to probe-arrival, including the never-arrives right
censoring) and enough history to fit. The ceiling is one line and stops the
bleed today.

### Rely on a positive check instead of any timeout

Rejected as a *replacement*, adopted as a complement. Every signal rule 4d
reads is an R2 read of a push mechanism, so when the provider drops that
mechanism, absence is indistinguishable from "could not look" — which is why
the conjunction is correctly slow to fire. SSHing in and reading the container
turns the ambiguity into positive evidence and is tracked separately; a
timeout is still needed for instances that are unreachable altogether.

## More Information

- **Builds on**: [0005](0005-let-learned-setup-survival-govern-onstart-watchdogs.md)
- **References**: `specs/campaign-lifecycle.allium` § DudVastDetection and
  § OnStartChainWatchdog; incident instances wi7287, wi7288, wi7309
