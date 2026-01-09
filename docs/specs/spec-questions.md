# Spec Questions, Alternatives, and Decisions

This document enumerates previously underspecified areas, lists plausible
alternatives, and records the chosen decisions.

## 1) Starting vs Running vs Failed transitions (core)

Question
- When is `starting` vs `running` set, and when is `failed` used vs `dead`?

Alternatives
- A: `starting` is only local setup; set `running` once tmux session exists; set
  `failed` only on setup/SSH errors.
- B: `starting` exists only in DB for a short interval; all successful starts
  jump directly to `running`.
- C: `failed` is reserved for explicit setup failures; `dead` is only for
  unexpected termination after `running`.

Decision
- Use A + C.

## 2) Update operations: local-only vs remote change

Question
- Do `describe/edit` enqueue a remote update (queue file edit), or are they
  local metadata changes only?

Alternatives
- A: Local-only metadata update (no pending op unless job is queued).
- B: Always enqueue a remote update for queued jobs.
- C: Local-only for running; enqueue for queued; reject for terminal.

Decision
- Enqueue a remote update when an operational field changes: directory,
  environment, command, or status transitions among queued/draft/killed.
- Applies to CLI `describe` (with arguments) and `edit`, plus TUI edits.

## 3) Log cache policy

Question
- What are the exact cache retention and eviction rules?

Alternatives
- A: Max age only (days), delete anything older on sync.
- B: Max size only, LRU eviction.
- C: Both max age and max size; delete oldest first, respect age as hard cutoff.

Decision
- Use C.

## 4) Log follow source selection

Question
- When both remote and cached logs exist, which source is authoritative under
  flaky connectivity?

Alternatives
- A: Prefer remote if reachable; fallback to cache on error.
- B: Always use cache for completed jobs.
- C: Use remote if job not terminal; cache for terminal jobs.

Decision
- If the job is done and a final log is cached, always use the local cache.
- Otherwise, prefer remote and fall back to cached with a warning.

## 5) Progress parsing edge cases

Question
- How should multiple progress formats, decreases, or missing denominators be
  handled?

Alternatives
- A: Last matching line wins; allow decreases.
- B: Track monotonic max; ignore decreases.
- C: Prefer percentage over ratio when both present.

Decision
- Use B + C.

## 6) Queue runner lifecycle triggers

Question
- When is the queue runner started/stopped/updated, and what versioning means?

Alternatives
- A: Always auto-start on any queued job; never auto-stop; update only on
  explicit command.
- B: Auto-start; auto-stop when queue empty; auto-update when runner version
  mismatch detected.
- C: Only start/stop via explicit `queue start/stop`.

Decision
- Use B.

## 7) Plan dependency encoding

Question
- How do `wait: success` and `wait: any` map to queued job dependencies?

Alternatives
- A: `success` -> `--after`, `any` -> `--after-any`.
- B: `success` -> `--after` (status file exit=0), `any` -> no dependency.
- C: Use explicit dependency objects in queue files instead of flags.

Decision
- Use A.

## 8) Plan `kill` edge cases

Question
- What happens when `kill` targets missing or terminal jobs?

Alternatives
- A: Ignore silently.
- B: Warn but continue plan execution.
- C: Fail plan execution.

Decision
- Use B.

## 9) Conflict resolution policy selection

Question
- When are non-default reconciliation policies applied?

Alternatives
- A: Only configurable in config file.
- B: CLI flags override config per invocation.
- C: Hardcode default policy only.

Decision
- Remove policy configuration and CLI flags; use the default policy only.

## 10) Host scoping rules for sync/queue runner

Question
- Which hosts are eligible for sync and queue runner start in mixed states?

Alternatives
- A: All known hosts in DB.
- B: Only hosts referenced by requested job IDs.
- C: Hosts with non-terminal jobs or queued work.

Decision
- Use B for targeted commands (status/info), C for global sync.

## CLIStateUpdatesPlusCal.tla: resolved questions

1) Starting state modeled
- `run` now creates `starting` and sets pending to `running`.

2) Cancel is distinct
- `cancel` uses the `canceled` target/status, separate from kill.

3) UpdateQueued semantics
- `update` represents remote queue edits for operational changes (dir/env/cmd/
  status). Metadata-only edits do not enqueue a remote update.

4) Base tracking
- Sync actions update `base` to the current status after successful reconciliation.

5) Offline intent
- Ops are always enqueued regardless of host reachability to support offline prep.

6) Transition guards
- Guards should reflect CLI rules (e.g., cancel only queued jobs). This remains
  to be enforced in the models and code paths as needed.

## 11) Wait/watching multiple jobs

Question
- When a wait/watch command targets multiple jobs, how does it complete and
  what does it report along the way?

Alternatives
- A: Complete only when all jobs are terminal; report incremental changes.
- B: Complete when any job is terminal; report only final summary.
- C: Complete when all reachable jobs are terminal; ignore offline hosts.

Decision
- Use A. Terminal states for waiting are `completed`, `dead`, and `failed`.
  Report incremental status changes as they happen.

## 12) Log tailing behavior

Question
- How should log tailing behave when logs are missing, in-flight, or complete?

Alternatives
- A: Block until logs appear; follow until terminal and log closed.
- B: Poll for missing logs; follow updates when present; finish when terminal
  and log closed or timeout.
- C: Only read once (no follow).

Decision
- Use B for both CLI and TUI. Missing logs are handled by polling; follow new
  output while the job runs; stop when the job is terminal and logs are closed,
  or when the timeout expires.
