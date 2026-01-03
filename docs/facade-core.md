# CLI, TUI, and Core Responsibilities

Remote Jobs now divides responsibilities across three layers so that user
interfaces stay thin and resilient while orchestration logic lives in one place.

## Core (`internal/core`)

* Owns database access for all mutating operations (start, kill, draft, queue).
* Validates input, sets pending/persistent state, and invokes `internal/ops`
  helpers to reconcile against remote hosts.
* Wraps every request in a `Service` object so callers receive a structured
  result (updated job snapshot, success/deferred flag, human-readable message).
* Kicks off reconciliation immediately after recording intent; deferred work
  remains queued for the next sync tick.

> Status-change helpers (kill, draft, queue, run) all share the same pattern:
> record target status → call sync/reconciliation with the caller’s timeout
> budget → return the reconciled job plus a message describing what happened.

## CLI (`cmd/`)

* Parses flags/arguments and renders messages for terminal consumption.
* Delegates all mutations to the core service. Commands never issue SQL
  queries except for read-heavy inspection commands such as `job list`.
* Converts validation errors or structured results into CLI-friendly text while
  preserving exit codes.

## TUI (`internal/tui`)

* Maintains ephemeral UI state: selection, filters, cached logs, host telemetry,
  periodic timers, and Bubble Tea commands.
* Uses the core service for any job mutation (kill, draft, queue, start) so the
  UI does not duplicate validation or reconciliation logic.
* Continues to execute read-only queries directly for performant list/host views.

## Sync and Reconciliation

* The TUI’s periodic background sync and the CLI’s `remote-jobs sync` command
  both call the same reconciliation helpers under the hood.
* Deferred work (e.g., a kill request issued while a host is offline) is kept as
  pending state in the database until the next sync pass clears it.

This split keeps presentation layers focused on interaction while the core
service centralizes every operation that mutates job state.
