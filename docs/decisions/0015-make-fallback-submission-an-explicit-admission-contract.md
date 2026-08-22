---
status: accepted
date: 2026-08-22
---

# 0015. Make fallback submission an explicit admission contract

## Context and Problem Statement

An external scheduler may prefer an authenticated Weft worker but retain its
own fallback. Ordinary durable queue submission is unsafe for that workflow:
if no worker accepts immediately, both systems may eventually execute the same
assignment. Host selection must also distinguish explicitly provisioned and
authenticated agent CLIs from ordinary executable discovery.

## Decision Outcome

Fallback callers use an explicit online-only submission mode. It performs live
inventory placement, requires remote queue acknowledgment, and removes the
local job if acknowledgment is not established. Ordinary submission retains
its durable queue semantics.

Host-local services are explicit capability labels. Agent capabilities use the
`agent:<name>` namespace and may have per-host concurrency limits. These
requirements are stored with the job so every later placement pass enforces
the same constraints.

External assignment IDs reuse the durable submit-token uniqueness mechanism.
Submission receipts are versioned independently of human CLI output so callers
can branch on acceptance, rejection, or deduplication without parsing prose.

### Consequences

- A fallback caller can safely proceed only after a `not_accepted` result.
- An authenticated CLI is never inferred merely because its executable exists.
- Capability slots count queued and running jobs and therefore reserve worker
  capacity before execution begins.
- Online-only admission may spend time pinning source data before discovering
  that no eligible worker is reachable.
- The ordinary durable queue remains the default and is unchanged.

## Considered Options

### Infer capabilities during host discovery

Rejected: executable presence does not prove that the SSH identity has usable
credentials, and authentication probes can be provider-specific or interactive.

### Let callers submit normally and cancel before fallback

Rejected: cancellation races dispatch and gives the caller no single outcome
on which it can safely choose another executor.

### Add a second idempotency table

Rejected: submit tokens already provide transactional uniqueness and daemon
retry recovery. A separate identity store would create two deduplication paths.

## More Information

- **References**: `docs/guides/agent-workflows.md`; `cmd/run.go`;
  `internal/placement/target.go`
