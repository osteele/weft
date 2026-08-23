---
status: accepted
date: 2026-08-23
---

# 0018. Delete the retired coordinator implementation

## Context and Problem Statement

The remote placement daemon was retired, but its packages, relay protocol,
configuration, command namespace, dashboard status, deployment recipe, and
historical design document remained. Current features still imported a few
helpers from its package tree, which made the retired design appear partially
supported and made accidental restoration easier than a clean design.

Host-addressed R2 mailboxes now solve outbound-only inventory delivery without
a relay, second database, or host-to-host forwarding authority. Local sync and
autopilot own controller work, while remote agents own durable execution.

## Decision Outcome

Delete the retired daemon and relay implementation, including compatibility
configuration and user-facing surfaces. Move independently useful remediation
and cloud-result helpers to packages that describe their current ownership.

Do not retain a dormant implementation as a template. If Weft later needs an
always-on remote control service, design it from current requirements and give
it an explicit authority and consistency model.

### Consequences

- There is one supported dispatch architecture and no configuration switch
  that can silently activate an unmaintained path.
- Old relay mailbox objects, if any exist, are inert and may be removed as
  ordinary R2 cleanup.
- Reintroducing an always-on service requires a new design and migration rather
  than reenabling old code.
- Accepted decision records remain as immutable history; living documentation
  describes only the current architecture.

## Considered Options

### Keep disabled compatibility code

Rejected: disabled paths continue to compile, attract incidental dependencies,
and imply a recovery option that is neither tested nor architecturally valid.

### Preserve only the relay

Rejected: direct host-addressed mailboxes solve transport without adding a
second database or forwarding authority.

## More Information

- **Supersedes**: [0002](0002-retire-the-coordinator-daemon.md)
- **Builds on**: [0017](0017-use-host-addressed-r2-mailboxes-for-isolated-inventory-hosts.md)
