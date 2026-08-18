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

## Adding one

See [0001](0001-record-architecture-decisions.md) for the full scheme. In short:
`NNNN-lowercase-hyphenated-decision.md`, next free number, title states the
position rather than the question, never rename or renumber, supersede rather
than edit.
