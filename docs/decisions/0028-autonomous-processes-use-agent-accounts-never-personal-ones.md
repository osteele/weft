---
status: accepted
date: 2026-09-04
---

# 0028. Autonomous processes use agent accounts, never personal ones

## Context and Problem Statement

Research work is moving to studio, where agent sessions run unattended. Those
sessions submit weft jobs to a hub on the laptop, and the hub in turn needs to
reach studio to bootstrap a public key.

The constraint driving the design was first stated as a direction: no remote
agent connecting into the laptop. That framing is too narrow and too broad at
once. Too narrow, because an autonomous process on the laptop reaching the
personal account on studio has the same character and was not covered. Too
broad, because a blanket ban on inbound connections would forbid the key
bootstrap the design needs, and the offered alternative of a non-admin agent
account on the laptop is itself inbound access.

The distinction that actually matters is which account, not which direction.
The personal account on studio authenticates through a biometric prompt on
every login and is explicitly unsuitable for unattended work; the agent account
uses a dedicated keyfile with no interactive agent.

## Decision Outcome

Autonomous processes use dedicated agent accounts. They never authenticate to a
personal account, in either direction, on any machine.

An autonomous process on the laptop may reach `agent@studio`. It may not reach
`osteele@studio`. A process on studio may not reach the laptop's personal
account. Whether a connection is inbound or outbound is not the test.

Two things follow that a directional rule would have gotten wrong. The hub's
bootstrap key pull is permitted, because it targets the agent account — and it
*must* target that account, since a pull against the personal account would
block an unattended run on a biometric prompt. And an agent account on the
laptop, if one is ever created for another purpose, does not thereby become a
route for edge submission: the edge keeps exactly one channel to the hub, which
is writing objects.

That last point is a transport decision rather than a security one. A second
inbound path would double the validation surface and would be exercised rarely
enough that it is where a defect would survive.

### Consequences

- Every machine participating in autonomous work needs an agent account
  provisioned, with its own keys. A host reachable only through a personal
  account cannot take part at all.
- Automation must be audited for which identity it authenticates as, not merely
  for which direction it connects. An SSH command that works interactively can
  fail or hang under automation for this reason alone.
- Anything requiring the personal account requires a human, by construction.
  This is the intended cost.
- The edge submission path stays single-channel even where a second one becomes
  technically available.

## Considered Options

### A directional rule: no inbound connections to the laptop

Rejected: it permits an autonomous process on the laptop reaching the personal
account on studio, which carries the same risk, and it would forbid the
bootstrap key pull the design depends on. It is the framing the work started
from, and it is credible — it addresses the case that prompted the concern —
but it draws the line on the wrong axis.

### Allow a non-admin agent account on the laptop as a submission route

Rejected for the edge path specifically. It is consistent with this record's
own invariant, so it is not forbidden in general and may be built for other
purposes. It is declined here because a second transport into the hub doubles
the validation surface for no gain the object store does not already provide.

## More Information

- **Builds on**: [0026](0026-isolate-edge-submissions-in-their-own-bucket.md)
- **References**: `docs/design/edge-submission-protocol.md`;
  the `remote-machines` skill, which documents the studio account split
