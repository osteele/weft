---
status: accepted
date: 2026-08-23
---

# 0017. Use host-addressed R2 mailboxes for isolated inventory hosts

## Context and Problem Statement

Inventory queue dispatch traditionally appends commands over SSH and legacy
source delivery used rsync. Submit-time source closures removed the source
transfer requirement for new queue jobs, but a selected inventory host still
could not receive work when it was on another LAN and had no inbound route from
the controller. Such a host may nevertheless have outbound access to the same
R2 store used for immutable sources and results.

The retired coordinator relay accepted R2 intents, maintained a second job
database, and forwarded jobs to hosts over its own LAN. Restoring it would
reintroduce placement and synchronization authority that the controller
already owns, plus a second deployment and failure domain.

## Decision Outcome

An explicitly configured inventory host may pull versioned queue commands from
a mailbox addressed directly to that host in R2. The controller writes the
same queue command used by direct SSH dispatch. The host's existing queue
daemon appends it to its local command log, acknowledges it, and publishes a
timestamped runner-state snapshot through R2.

Mailbox delivery is at least once. The daemon checks acknowledgements before
re-appending, while queue commands and attempt IDs remain idempotent across the
smaller crash window between local append and acknowledgement. Controller-side
status accepts only fresh, correctly versioned, correctly addressed snapshots;
unavailable or stale state remains unknown.

The controller remains the sole placement and database authority. There is no
relay, coordinator database, or host-to-host forwarding step.

### Consequences

- A pinned job can be submitted, queued, observed, and completed on an
  outbound-only inventory host without SSH or rsync from the controller.
- Directly reachable inventory hosts keep the established SSH transport by
  default; R2 remains an explicit per-host choice.
- The host daemon and controller both require access to the configured R2
  bucket, whose command envelopes may contain resolved job environment values.
- Legacy jobs without source closures still need the direct rsync path.
- `--needs` staging and force-killing an already-running process still require
  direct host access; queue add, update, priority, and cancel use the mailbox.
- A fresh R2 state snapshot is positive evidence. A stale or absent snapshot
  cannot be used to infer that a queued job disappeared.

## Considered Options

### Restore the coordinator relay

Rejected: it adds a second database and forwarding authority to solve a
transport problem, reversing the reasoning in decision 0002.

### Model the inventory host as a rental instance

Rejected: provider lifecycle, self-destruction, instance reuse, and billing
semantics do not apply to an owned workstation.

### Implement the full federated blackboard protocol first

Rejected: distributed claiming is useful when workers compete for unassigned
jobs. This case already has an explicit host selection and needs only addressed
delivery and observation.

## More Information

- **Builds on**: [0002](0002-retire-the-coordinator-daemon.md),
  [0004](0004-keep-r2-optional-for-on-prem-hosts.md), and
  [0016](0016-run-inventory-queue-jobs-from-the-submitted-source-closure.md)
- **References**: `docs/architecture/sync.md`; `specs/status-sync.allium`;
  `internal/inventoryqueue`; `cmd/agent/inventory_queue_r2.go`
