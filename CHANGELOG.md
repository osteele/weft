# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **Execution platform requirements**: `--platform OS/arch` and script
  `platform` metadata constrain CPU-only and GPU jobs across inventory hosts,
  new rentals, and rental reuse. Explicit host pins must satisfy the same
  requirement; retry and edit preserve it.

- **Published directory dependencies**: Rental and R2-pull consumers stage
  ready producer directories as exact child-file transfers, preserving nested
  paths and producer-attempt boundaries.

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

- **Terminal attempts are never reset in place**: A job that reaches a terminal
  status while a dispatch pass is in flight keeps its recorded outcome; the
  pass abandons its append and records `queue.dispatch.superseded` instead of
  writing `queued` over the finished attempt. Re-asserting an attempt's
  existing terminal status is now idempotent, so a job left in that state by
  an earlier release settles on the next sync instead of failing every
  completion write against the terminal-once index.
- **Attempt-fenced completion sync**: Runner completion snapshots identify the
  exact attempt. Legacy job-only completions cannot terminate a newer retry,
  and a failed row update does not block synchronization of sibling jobs (wb178).
- **Retry target preservation**: `edit --retry` and `edit --status queued`
  retain stored inventory pins or compatible live rentals. Normal restart
  restores a stored pin when the latest attempt is unplaced (wb177).
- **Dispatch diagnosis provenance**: Daemon sync warnings include timestamps;
  log replay is marked as historical. Queued-job diagnosis reports current
  dispatch history and treats missing or unreadable evidence as unknown (wb176).
- **Edge diagnosis command classification**: `diagnose shadow` is classified as
  a mirrored read command; an unavailable edge view gets the standard view
  diagnostic instead of an unclassified-command error (wb175).
- **Agent identity comparison**: An agent version that could not be hashed from
  source is marked as a revision fallback and no longer judges a source-hash
  deployment. A process that cannot compute the hash reports the host as
  `unknown` and leaves its binary in place, instead of redeploying and
  re-execing the runner on every dispatch pass.
- **Runner capability evidence**: A host whose runner state could not be read
  is reported as unknown rather than as a runner lacking the capability. The
  `weft queue update` remedy is reserved for a state that was read and does
  lack it, so an unreadable publication no longer prescribes a redeploy that
  cannot clear the block (wb182).
- **Queue position under an unobserved host**: While an inventory host's runner
  publication is stale, `weft status` reports the queue position and progress
  as unknown instead of naming a job ahead, whose row is equally stale (wb181).
- **Job cost completeness**: `weft cost jobs` computes cost from rental history
  on the same basis as `weft info`, states the selection it applied, names the
  jobs it did not examine, and reports an unattributable shared rental as
  unknown rather than as zero. `--all` and `--limit` select the window (wb179).
- **Pinned-source edits**: A command-only `weft edit` of a job with an
  immutable source pin no longer attempts a mutable source push, which failed
  against scoped task snapshots and blocked the edit. The queue update still
  applies and the recorded pin is untouched (wb180).
- **Re-dispatch held until the runner catches up**: A payload absence observed
  before the runner consumed weft's previous re-dispatch is treated as unknown
  rather than as a lost job, so a dispatch in flight is no longer reset and
  re-appended on every sync pass.
- **Unattributable completions**: A job-only completion is settled when the job
  has exactly one live attempt and the reported finish falls inside it.
  Genuinely ambiguous completions record a visible event that `weft status` and
  `weft diagnose` render, instead of a debug line and an indefinitely running
  row. Each attempt also records when weft first observed a host-reported
  terminal exit, so settlement latency is attributable.

- **Artifact dependency failure isolation**: Confirmed missing or failed
  producer artifacts fail the rental consumer with a dependency diagnosis.
  Pending publication and storage lookup errors defer admission. These
  rejections preserve healthy jobs and do not trip the infrastructure breaker.
- **Daemon identity probes**: Recovery uses the caller's probe timeout and reports
  uncertain daemon identity without suggesting that a start or restart failed.
  Uncertain probes leave the daemon process and its state files untouched.
- **Machine-readable job lists**: Explicit sync writes status summaries to stderr,
  keeping JSON and TSV stdout free of human-readable sync diagnostics.
- **CUDA failure attribution and retry history**: Hardware-fault detection
  requires diagnostic Xid, NVLink, peer-memory, or ECC evidence, so ordinary
  output such as "oxidized" cannot terminate a GPU job. Job info reports the
  cumulative cost of exclusive rental attempts, and `log --attempt` does not
  append progress or diagnosis from the latest attempt.
- **Live phase versus terminal attempt**: Reconciliation no longer destroys a
  healthy rental when a fresh running phase conflicts with a stale terminal
  attempt row. It signals the agent's per-job stop path and lets completion
  sync close the attempt and launch.
- **Writable extracted source trees**: Pinned-source extraction restores
  archived directory and file modes with the owner access bits forced on, and
  applies directory modes only after the subtree exists. A read-only mode in
  the archive no longer blocks nested extraction or the worker's own paths
  inside the project root, such as agent-execution's `.agent-execution/results`.
- **Torch 2.6 placement compatibility**: CUDA wheel variants now cap compatible
  devices at Hopper (`sm_90`), preventing unsupported Blackwell rentals while
  preserving the supported boundary.
- **Queue start ownership**: Start-now reconciliation recognizes an active,
  attempt-fenced Studio queue writer and preserves the open attempt when runner
  evidence is unavailable, preventing false terminal states and duplicate work.
- **Studio producer dependencies**: R2-pull dispatch stages `--needs` payloads
  only from the completed producer's authoritative attempt publication. Missing,
  pending, failed, or mismatched publication evidence blocks dispatch visibly.
- **Recovered review results**: Inventory recovery republishes declared
  `.agent-execution` result files through the attempt-scoped output boundary
  after waiter loss without re-executing the worker command.
- **Declared artifacts after runner restart**: Persist `--produces` declarations
  before execution and recover their manifests and completion-file listings.
  Artifact retrieval includes declared paths outside `output/` and `outputs/`
  when a manifest is missing, while retaining attempt-window checks.
  Fast-path direct cloud retrieval and local SQLite/filesystem artifact caching
  prevent automated retrieval timeouts (e.g. from detached review waiters).
  Scripts can append artifact paths to pre-registered JSON manifests.
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
