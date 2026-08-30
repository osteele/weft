---
status: accepted
date: 2026-08-31
---

# 0025. Progress parsing never terminates an instance

## Context and Problem Statement

Weft scrapes running jobs' output for progress: explicit `Progress: N%` and
`Progress: N/M` lines, tqdm bars, and `Epoch N/M` counters. The parsed value
feeds the TUI's progress bars and ETA estimates, and it also fed the rental
task-stall watchdog, which terminated an instance when the structured
`(job, phase, percent)` tuple had not changed for an hour.

Those two uses have opposite tolerance for error. A wrong progress bar is
cosmetic. A wrong termination destroys running work and the money already spent
producing it.

The signal cannot support the second use, because it is a scrape of a
convention rather than an observation of the job. A job reports progress only
if its output happens to match one of the recognized patterns. Anything else —
a bare counter, a decimal percent, a word before the number, a domain-specific
format, or any library that prints progress its own way — parses to nothing.
Weft cannot distinguish a job that reported no progress from a job whose
progress it failed to recognize, and it never sees the difference, because an
unrecognized line is discarded silently and looks exactly like a job that
printed nothing.

A job emitting `Progress: 5` through `Progress: 50` over 37 minutes of real
computation was terminated as stalled, having cleared its behavior gates and
written a 5.1 MB result plus twelve checkpoints. Roughly 200 attempts over two
days produced zero completions and a $53 floor. Every attempt was killed by the
progress rule while the job was working.

Weft already observes running jobs directly, and both direct signals were
satisfied throughout: the agent's stdout-silence watchdog saw output every few
minutes, and its GPU-idle watchdog saw a busy card. Only the inferred signal
concluded the job was stalled, and it was the only one that could be wrong
because of a string format.

## Decision Outcome

Parsed progress is a presentation signal. It may drive progress bars, ETA
estimates, and status display. It must never be the grounds for terminating an
instance, failing a job, or any other destructive or terminal action.

Stall termination rests on directly observed evidence: stdout silence, GPU
idleness, agent heartbeat, provider status, and the phase markers the agent
writes itself. These observe the job or its host rather than inferring from a
formatting convention the job never agreed to follow, and their absence means
the thing itself was absent rather than unrecognized.

This is the general rule in CLAUDE.md applied to one signal: for a destructive
action, unknown is never sufficient. An unparsed line is unknown.

### Consequences

- A job that hangs while printing recognized progress values is no longer
  caught by the progress rule. It is caught by stdout silence and GPU idleness
  if it is genuinely inactive, and by the rental time and spend budgets
  otherwise. A job that hangs while actively printing and using the GPU is not
  detectable by weft at all, and is the user's to bound with an in-script
  ceiling.
- Weft loses its only task-level stall signal. The remaining signals are
  process- and host-level, so a job stuck in a loop that still burns GPU and
  prints looks identical to one making progress. This is the cost, and it is
  accepted: the alternative was terminating jobs that were working.
- Progress parsing may be extended freely — new formats, looser matching —
  without a safety review, because no destructive action depends on it. That is
  the point of the boundary.
- A future contributor will find a parsed, monotonic, timestamped progress
  value and a watchdog needing exactly such a value, and the wiring will look
  like an oversight rather than a decision. It is not.

## Considered Options

### Accept more progress formats, including bare counters

Rejected. It narrows the gap without closing it: the rule still terminates on
absence of a recognized string, and every format not yet added still reads as a
stall. It also makes weft more fragile against code not written with weft in
mind, which is most code — a library printing its own progress dialect would
newly acquire the power to keep an instance alive or let it die.

### Warn when a `Progress:` line fails to parse

Rejected as a fix, though it would have surfaced this incident sooner. Any
output containing `Progress:` in a foreign format would warn, on jobs whose
authors never intended to talk to weft, so the warning is noise wherever weft
did not author the producer. It also leaves the termination path intact.

### Require jobs to opt into progress-based termination

Rejected. It preserves a correct-but-sharp mechanism at the cost of a flag
nobody sets, and the jobs most likely to set it are long-running expensive ones
where a false termination is worst.

## More Information

- **Builds on**: the absence-of-evidence rule in CLAUDE.md, and
  [0021](0021-keep-host-capability-observations-advisory.md), which keeps a
  different inferred signal out of a decision it cannot support.
- **References**: `internal/progress/progress.go` for the recognized formats;
  `specs/job-lifecycle.allium` rules `GPUIdleKillsJob` and
  `StdoutSilenceKillsJob` for the signals that do terminate.
