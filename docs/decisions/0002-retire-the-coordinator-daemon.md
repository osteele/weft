---
status: accepted
date: 2026-08-15
decision-date: 2026-06-05
---

# 2. Retire the coordinator daemon

## Status note

The decision was taken on 2026-06-05 (commit `a450f4a20`, "Deprecate coordinator
daemon"). **The rationale was never written down.** This ADR is retroactive: the
Context and Decision Drivers below are reconstructed from that commit's diff and
from the documentation it replaced, not from a contemporaneous record. Sections
marked *(reconstructed)* should be corrected by anyone who remembers the actual
reasoning.

## Context and Problem Statement

Weft was originally designed as a three-component system: CLI/TUI on the laptop,
a **coordinator daemon** on studio, and Go agents on each remote host. The
coordinator owned placement scoring, data pre-staging (rsync), and queue
dispatch. The laptop submitted work as intent files over SSH; the coordinator
watched an intent directory with fsnotify, enriched each intent with a placement
decision, and dispatched. The design is described in
`docs/design/coordinator-architecture.md`.

By the time of the decision, the coordinator was already documented as
**optional**. The README stated: "The coordinator is optional. If it is
unavailable, the CLI uses the same placement logic locally and queues work
directly over SSH." `docs/design/architecture.md` said the same: "When the
coordinator is unreachable, the CLI falls back to local placement scoring and
direct SSH dispatch — the same job submission works either way."

So the system carried two dispatch paths that produced the same outcome, and the
fallback path was strictly more available than the primary one.

## Decision Drivers *(reconstructed)*

- **No capability gap.** The fallback path already did placement scoring and
  SSH dispatch. The coordinator's remaining exclusive function was to do that
  work on a different machine.
- **The coordinator was not more available than the client.** It ran on studio —
  a workstation, not a highly available server — under launchd with
  `KeepAlive=true`. Durability across laptop disconnection was already provided
  by the remote agents, which keep running queued jobs regardless of whether
  either the laptop or the coordinator is reachable. That is the property that
  actually mattered, and the coordinator was not what supplied it.
- **Deployment cost.** Shipping a coordinator change meant `just
  deploy-coordinator`: rsync sources to studio, build there, stop the launchd
  service, wait for it to auto-restart, check status. That is a second
  deployment channel to keep working, on top of agent binary deployment.
- **Two paths, one of them rarely exercised.** Maintaining a primary and a
  fallback for the same operation means the fallback is under-tested until the
  moment it is needed — or, as here, the primary is the one that atrophies.

## Considered Options

- **Keep the coordinator as the primary path**, with the CLI fallback retained.
- **Retire the coordinator**, promote local CLI/TUI placement to the only path.
- **Keep the coordinator but narrow its role** (e.g. to pre-staging only).

## Decision Outcome

**Retire the coordinator daemon.** The CLI/TUI owns placement scoring, sync, and
direct SSH dispatch. Durable execution after the submitting laptop disconnects
comes from the remote agents, not from a central service.

Migration was by deprecation rather than deletion:

- `weft coordinator start` is disabled; `stop` and `status` remain as cleanup
  commands for any daemon still running in the field (`cmd/coordinator.go`).
- `just deploy-coordinator` fails with a pointer to `just build`,
  `just deploy-agent <host>`, and `weft sync --full <host>`.
- `docs/design/coordinator-architecture.md` is retained, banner-marked, as the
  historical description of what was retired.
- The `internal/coordinator/` package still exists.

### Consequences

- Placement requires a reachable control machine. Work already queued keeps
  running without one, but *new* placement does not happen while the laptop is
  closed. This is the principal capability given up, and it is the gap that
  [`docs/design/federated-blackboard.md`](../design/federated-blackboard.md) is
  designed to close without reintroducing a central service — that document's
  constraint "must not introduce an always-on coordinator service" is downstream
  of this ADR.
- One dispatch path, always exercised.
- One deployment channel (agent binaries) instead of two.
- The autopilot inherits the role of "the thing that places work continuously",
  but it runs inside the CLI/TUI process rather than as a service.

### This is not a decision against daemons in general

Weft has since grown a **local** daemon exposing a Unix-socket subscription API
(see [`docs/design/daemon-subscription-api.md`](../design/daemon-subscription-api.md)).
That is deliberately a different thing: it is local-only, it serves live-state
fanout and repeated reads, and it is not an authority over placement. This ADR
rejects a *remote, always-on, placement-owning* service. It does not constrain
local process structure.

## More Information

- Commit `a450f4a20` (2026-06-05) — the deprecation, spanning README, `cmd/coordinator.go`,
  `docs/design/architecture.md`, `docs/reference/commands.md`, and the justfile.
- `docs/design/coordinator-architecture.md`
  — the retired design, including its own decisions on SSH + JSONL intents and
  the coordinator/edge-agent authority split.
- [`docs/design/architecture.md`](../design/architecture.md) § Overview — the
  resulting two-component architecture.
