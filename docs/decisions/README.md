# Architecture Decision Records

Numbered records of consequential architectural decisions. Each file states one
decision, the forces behind it, the options rejected, and what the decision
costs.

These are **history, not current-state documentation**. A record is never edited
to flip its outcome; it is superseded by a new record. For how the system works
today, read [`docs/design/`](../design/) and [`docs/architecture/`](../architecture/);
for planned work, [`docs/planning/`](../planning/).

| # | Decision | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | accepted |
| [0002](0002-retire-the-coordinator-daemon.md) | Retire the coordinator daemon | accepted |
| [0003](0003-split-estimation-across-three-projects.md) | Split estimation across three projects | accepted |
| [0004](0004-keep-r2-optional-for-on-prem-hosts.md) | Keep R2 optional for on-prem hosts | accepted |
| [0005](0005-let-learned-setup-survival-govern-onstart-watchdogs.md) | Let learned setup survival govern the OnStart watchdogs | accepted |
| [0006](0006-settle-blocked-reason-verdicts-through-one-typed-builder.md) | Settle blocked-reason verdicts through one typed builder | accepted |
| [0007](0007-typed-recheck-with-string-classifier-fallback.md) | Type the recheck cadence, keep the string classifier as fallback | accepted |
| [0008](0008-cap-the-borrowed-curve-in-dud-detection.md) | Cap the borrowed survival curve in dud detection | accepted |
| [0009](0009-read-the-container-before-declaring-it-dead.md) | Read the container before declaring it dead | accepted |
| [0010](0010-cap-the-borrowed-curve-in-the-pre-running-watchdogs.md) | Cap the borrowed curve in the pre-running watchdogs | accepted |
| [0011](0011-heartbeat-preserves-snapshot-time.md) | Heartbeats preserve the snapshot timestamp | accepted |
| [0012](0012-separate-state-and-data-from-configuration.md) | Separate state and data from configuration | accepted |
| [0013](0013-sort-the-cli-job-list-by-recency.md) | Sort the CLI job list by recency | accepted |
| [0014](0014-key-source-archives-by-canonical-tar-identity.md) | Key source archives by canonical tar identity | accepted |
| [0015](0015-make-fallback-submission-an-explicit-admission-contract.md) | Make fallback submission an explicit admission contract | accepted |
| [0016](0016-run-inventory-queue-jobs-from-the-submitted-source-closure.md) | Run inventory queue jobs from the submitted source closure | accepted |
| [0017](0017-use-host-addressed-r2-mailboxes-for-isolated-inventory-hosts.md) | Use host-addressed R2 mailboxes for isolated inventory hosts | accepted |
| 0018 | ~~Delete the retired coordinator implementation~~ — withdrawn, see below | withdrawn |
| [0019](0019-admit-concurrent-jobs-with-host-local-ram-reservations.md) | Admit concurrent jobs with host-local RAM reservations | accepted |
| [0020](0020-keep-unattributed-sessions-inclusive-and-unproven-project-roots-unassigned.md) | Keep unattributed sessions inclusive and unproven project roots unassigned | accepted |
| [0021](0021-keep-host-capability-observations-advisory.md) | Keep host capability observations advisory | accepted |
| [0022](0022-attach-capability-behaviour-to-the-namespace-prefix.md) | Attach capability behaviour to the namespace prefix | accepted |

## 0018, withdrawn

0018 recorded that the retired coordinator's code and surfaces were deleted. It
was an action, not a decision: once done, nothing was left for a future reader
to undo by mistake, and its one durable clause — design any future placement
service from current requirements rather than from the deleted one — restates
[0002](0002-retire-the-coordinator-daemon.md).

It also claimed to supersede 0002, which is wrong in substance as well as in
bookkeeping: 0002 remains `accepted` with no `superseded-by`, and 0018 confirmed
0002 rather than reversing it.

The file is removed rather than left in place, because a record that does not
carry a decision costs every other record some of the attention they are meant
to command. The number is not reused. The content is in the repository history,
which is where a withdrawn record belongs — a record does not need to double as
its own archive.

The deletion it described is complete: no `cmd/coordinator*.go`, no
`docs/design/coordinator-architecture.md`, and both `internal/coordinator*`
trees hold no files. 0002 and 0017 carry the constraints that remain live.

## What earns a record

A decision earns a record when **a future reader could reasonably undo it by
mistake**. That is the operative test, and it sharpens the three conditions in
[0001](0001-record-architecture-decisions.md) — architectural, contested,
durable — rather than replacing them. A decision can satisfy all three and
still need no record, because nobody would think to reverse it; the mistake has
to be *available*. Where the two readings differ, this one governs.

Consequences of applying it:

- Prefer a code comment plus an invariant note for a local choice. A directory
  that records everything signals nothing, so each record has to earn attention
  it takes from the others.
- **A rule that gains clauses belongs in a spec, not here.** Records are
  immutable and are superseded rather than amended, so filing a growing rule as
  a record makes it harder to amend than the code it governs. Records fix a
  position; specs evolve in place. A position that *reverses* is exactly what a
  superseding record is for.
- **An action taken is not a decision.** Once a deletion or migration is done,
  there is nothing left to undo by mistake. The durable constraint it served
  belongs in the record that decided it.
- Do not restate a record's content in a spec and the spec's content in a
  record. Where both exist, the spec says what the system does and the record
  says which alternative was rejected and why.

## Adding one

See [0001](0001-record-architecture-decisions.md) for the full scheme. In short:
`NNNN-lowercase-hyphenated-decision.md`, next free number, title states the
position rather than the question, never rename or renumber, supersede rather
than edit.
