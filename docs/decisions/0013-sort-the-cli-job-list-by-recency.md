---
status: accepted
date: 2026-08-21
---

# 0013. Sort the CLI job list by recency

## Context and Problem Statement

`weft list` and `weft job list` order jobs with running, starting, and paused
first, then by job ID descending. `--limit` defaults to 50 and is applied as a
slice after that ordering, so a newly submitted job — necessarily queued, and so
in the second sort group — is among the first rows dropped whenever enough
active jobs precede it.

That ordering was chosen when the CLI was the primary surface for watching work
in flight, and putting active jobs first is the right answer for a human doing
that. It is no longer the primary surface: people watch the TUIs and the weft
status app, and the CLI is now driven mostly by agents.

An agent asks a different question. It is not surveying what is running; it is
checking whether the job it just submitted exists. For that question the newest
row is the only one that matters, and it is exactly the row the ordering
sacrifices. An agent that cannot see its own submission concludes the submission
failed and resubmits, which on this system means duplicate paid GPU jobs.

## Decision Outcome

The CLI's plain list path sorts by job ID descending before applying `--limit`,
so the most recently submitted jobs are the ones that survive the cap.

The scope is that path alone. The TUI, `weft project jobs`, and `weft job watch`
share the same collector and keep the SQL ordering, because active-first is
still correct for a human watching work in flight. The SQL is unchanged, so the
six sibling queries, the dashboard and monitor selection at `LIMIT 1000`, and
the query-plan guard that mirrors the clause are all untouched.

Ordering alone would only move the problem. Active-first is load-bearing as a
truncation-survival property: on an install with more than 50 matching jobs it
is the reason running jobs appear in the default listing at all. Sorting by
recency instead means running jobs become the truncatable ones. So the listing
now also reports how many rows `--limit` dropped, on stderr, alongside the
existing notice for the recency window. Neither ordering can drop rows silently,
and the choice of which rows to sacrifice stops being load-bearing.

### Consequences

- On a busy install the default `weft job list` can omit running jobs. This is a
  real behavior change; it is disclosed rather than silent, and the surfaces
  humans use to watch running work are unaffected.
- `weft list --group-by status` re-sorts for display but receives the same
  capped set, so active jobs can thin there too.
- `--search` on this path stops passing `--limit` down to the query and caps in
  Go instead. It has to: the search query orders by start time, and a job that
  has not started yet has none, so a SQL cap drops the newest match before the
  sort can see it.
- The two listings now answer different questions and may disagree about which
  jobs they show. That is intended: the CLI answers "what happened recently",
  the TUI answers "what is happening now".
- Notices stay on stderr. They are emitted above the format switch, so `--format
  json` and `--format tsv` reach them; stdout for those formats must remain
  byte-identical whether or not rows were dropped, and is pinned by test.

## Considered Options

### Report truncation without reordering

Leaves the newest job hidden. It converts a silent failure into a disclosed one,
which is worth doing on its own, but an agent checking its submission still
cannot see the row it came for.

### Change the SQL ordering

One clause instead of a gated sort, but it reorders every consumer of the shared
queries, including the dashboard and monitor selection at `LIMIT 1000` where
active-first governs which rows are fetched at all. It would also silently
decouple the deliberate query-plan mirror that guards index usage. Too much
blast radius for a CLI-shaped problem.

## More Information

**Builds on** [0002](0002-retire-the-coordinator-daemon.md), which treats
CLI/TUI as a single client. That framing still holds for placement; this record
splits the two only for list presentation.

`weft job mine` already fetches unlimited, sorts by ID descending, and truncates
— the same pattern, adopted earlier for the same agent-facing reason.
