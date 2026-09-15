# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **Structured admission rejections**: Online-only JSON receipts include stable
  refusal codes and UTF-8 diagnostics bounded to 1,024 bytes. The v1 receipt
  contract preserves its existing no-job fallback guarantee.

- **Session-scoped read-only TUI**: `weft tui --session <id> --project
  <absolute-path> --read-only` shows only jobs attributed to that exact agent
  session and verified project root while preserving logs and disabling
  reconciliation and interactive mutations.

### Changed

- **`host agent-status --json` evidence**: Schema version 2 reports the desired
  agent fingerprint per host target and removes the document-level
  `desired_version`; rows include `desired_error` when target resolution fails.

- **CLI timestamp display**: Human-readable timestamps default to explicitly
  marked UTC for agent readers. `cli_timezone` selects UTC, local time, or an
  IANA timezone; interactive TUI displays stay local. Structured timestamp
  strings in JSON use UTC. TSV timestamp columns use human display formatting;
  `killed_at` changes from RFC3339 to a zone-marked display string.

### Fixed

- **Bounded cloud completion sync**: Routine sync retries incomplete terminal
  completion metadata for 24 hours after a launch ends. Older rows remain
  eligible for the explicit historical R2 repair sweep without consuming every
  daemon pass.
- **CLI completion with pending hooks**: Commands exit without draining the
  lifecycle-hook backlog. The daemon retains responsibility for durable retries,
  so slow hooks do not delay completed JSON queries.
- **Online-only admission cleanup**: Refused submissions remove attempt-bound
  telemetry before deleting provisional attempts, avoiding foreign-key failures
  while preserving execution evidence and transactional rollback.
- **Job-wait transport failures**: `status --wait` falls back to remote status
  polling when daemon recovery fails, preserving the original wait deadline.
- **Inventory queue admission failures**: R2-pull hosts publish attempt-fenced
  preflight rejections and diagnostics instead of repeatedly redispatching
  rejected source downloads or archives.
- **Terminal publication across runner updates**: Re-exec waits for completion
  handoff and marker callbacks. Startup and duplicate terminal deliveries recover
  missing publication through the durable post-job queue without executing the
  command again. Completion records preserve custom output directories; startup
  R2 recovery shares a 15-second budget and reports unprocessed retained records.
- **Automatic daemon recovery during job waits**: `status --wait` starts a
  confirmed-absent daemon and resumes observation within a bounded recovery
  window. Live processes and ambiguous socket failures remain untouched.
- **Job waits across daemon restarts**: `status --wait` reconnects after a
  subscription EOF while preserving pending jobs and the original wait timeout.
- **Rental preparation outside the Weft checkout**: Installed CLI builds use
  their recorded, prewarmed agent identity when invoked from another project,
  so cloud jobs no longer remain blocked while resolving the agent binary.
- **Queue-runner recovery**: A job with a confirmed-absent status file and
  worker process becomes `unresolved` after 15 minutes. This state releases
  placement capacity without inventing a terminal outcome, and accepts either
  late completion evidence or an explicit restart.
- **R2-pull queue reconciliation**: Runner state includes a complete,
  attempt-fenced payload inventory, allowing missing or stranded queued jobs
  to be safely re-dispatched instead of repeating an unresolvable
  publication-status warning.
- **`queue add` command**: Fixed database error when adding jobs to queue
  (`NOT NULL constraint failed: jobs.start_time`). Queued jobs now correctly
  have NULL start_time until they begin running.
- **Queued jobs in TUI and list**: Queued jobs now appear at the top of the
  job list and display "—" for start time instead of epoch date.

## [0.1.0] - 2024-12-24

### Added

- **Tabbed detail panel**: The job detail panel now has "Details" and "Logs" tabs
  - Press `Tab` to switch between Details and Logs views
  - Press `l` to jump directly to Logs tab
  - Active tab is shown in bold in the header
- **Environment variables display**: Job details now show environment variables
  extracted from `export VAR=value && ` command prefixes
- **Mouse support**: Click on jobs in the list to select them
- **`weft status` command**: Re-enabled as a top-level command (synonym for
  `job status`)

### Changed

- **Cleaner command display**: Commands are now displayed without `export VAR=... && `
  prefixes for cleaner output (environment variables shown separately in details)
- **Consistent truncation**: Job list now uses consistent `…` character for truncation
  instead of mixed `...` styles
- **.gitignore**: Added `.gocache/` to ignore Go build cache

### Fixed

- Job list truncation now only adds ellipsis at the end, not both ends
