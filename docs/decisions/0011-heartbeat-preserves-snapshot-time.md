---
status: accepted
date: 2026-08-19
---

# 0011. Heartbeats preserve the snapshot timestamp

## Context and Problem Statement

The daemon's activity feed emits a snapshot on change, plus a heartbeat every
45 seconds when idle. Clients derive data age from `snapshot.time`; Weft
Status shows "stale" when it stops advancing.

A heartbeat is a statement about the daemon: it is alive. `snapshot.time` is a
statement about the data: when it was last observed. These are different
facts. Recomputing `snapshot.time` on every emission conflates them — a daemon
whose payload builds are starving keeps asserting fresh data indefinitely, so
the feed's own starvation becomes undetectable from the client side. This is
not hypothetical: the feed has gone silent for minutes while every timestamp
it emitted claimed freshness.

The general principle: refreshing a timestamp the daemon did not re-observe
asserts a freshness it has no evidence for. This repo has a recurring bug
class of exactly this shape — treating a failed, stale, or absent observation
as a confirmed fact.

## Decision Outcome

The activity subscription heartbeat re-emits the last built payload unchanged.
`snapshot.time` is preserved, never refreshed to the emission time. The
heartbeat frame proves the daemon is alive; the preserved timestamp tells the
truth about how old the data is. Before the first build completes there is no
payload to re-emit, so the heartbeat emits nothing.

### Consequences

- A client watching only `snapshot.time` correctly reports stale while builds
  are starving. This looks like a regression against the old always-advancing
  timestamp; it is the intended behaviour, and it is why this decision needs
  recording.
- `snapshot.time` alone can no longer distinguish "daemon dead" from "daemon
  alive but slow". That distinction requires the client to notice that
  heartbeat frames still arrive — a capability weft-status does not have
  today.
- Elapsed-age display on a genuinely idle feed no longer advances from the
  payload alone.
- The previously documented behaviour — recomputing `snapshot.time` and
  `autopilot.pass_age_seconds` on every emission — is withdrawn.

## Considered Options

### Refresh `snapshot.time` on every heartbeat

Rejected: it asserts unobserved freshness, and it is precisely what lets a
starving feed look healthy.

### A separate lightweight heartbeat event carrying no payload

Rejected: it requires a client contract change, whereas re-emitting the last
payload keeps existing clients working unchanged. Worth revisiting if a client
needs to distinguish slow from dead without inspecting frame arrival.

## More Information

- **References**: `docs/design/daemon-subscription-api.md` (the heartbeat
  contract), `internal/daemonapi/watch.go` (`runActivityLoop`)
