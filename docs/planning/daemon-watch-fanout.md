# Daemon Watch Fanout

## Problem

Long-running CLI monitors currently open and poll the local SQLite database on
their own. This works for a small number of attached processes, but it breaks
down when agents create shell loops or when old monitors outlive the session
that started them. The daemon cannot see or optimize those loops, and DB lock
churn becomes harder to diagnose.

## Goals

- Make first-class Weft watch/wait commands the only recommended monitoring
  surface for jobs, logs, daemon state, and instance state.
- Move repeated status polling into the daemon so there is one DB reader loop
  per watched resource class, not one per client process.
- Let clients subscribe over a local transport and stop receiving updates when
  the connection closes.
- Expose active subscribers for diagnosis, for example through
  `weft daemon clients` or `weft debug watchers`.
- Preserve one-shot commands that read the DB directly for fast non-watching
  status checks.

## Non-Goals

- Do not make every CLI command an RPC request. Submit, repair, and admin
  commands can continue to perform direct DB writes where that is the simpler
  source of truth.
- Do not replace the TUI in the first step. The TUI can become a subscriber
  after the daemon watcher API exists.
- Do not require a network service. The transport should be local-only.

## Proposed Shape

Add a local daemon listener, preferably a Unix domain socket under
`~/.cache/weft/`, with request types for:

- job status watch: job IDs, optional project/tag filters, timeout
- job log watch: job ID, tail/from/grep options, follow flag, timeout
- host/daemon health watch
- instance watch

Each subscription has:

- client PID, executable, command line summary, and start time
- requested resources
- last-delivered sequence/time
- optional deadline
- connection-backed liveness

The daemon owns coalescing and backoff. Multiple clients watching the same job
share one DB refresh path, and updates are pushed only when status, reason,
phase, log offset, or relevant metadata changes.

## First Increment

1. Keep current command behavior, but require first-class Weft wait/watch
   commands in agent-facing guidance instead of external shell loops.
2. Add a watcher registry table or pidfile directory for long-running
   first-class Weft commands. Record PID, command, resources, started-at,
   heartbeat, and optional deadline.
3. Add `weft daemon clients` or `weft debug watchers` to list active/stale
   watchers.
4. Add cleanup for watcher records whose PID is gone or heartbeat is stale.

This increment gives visibility before changing the transport.

## Second Increment

1. Add the daemon socket and a small subscription protocol. Implemented for job
   status watches in `internal/daemonapi`; the daemon listens on
   `~/.cache/weft/daemon.sock`.
2. Move `weft status --wait` to subscribe through the daemon when it is
   available, falling back to direct internal polling only when the daemon cannot
   be started or the socket cannot be reached. The fallback remains inside Weft
   so agents are not encouraged to add their own outer loop.
3. Move `weft log --follow` to the same mechanism for DB-backed log state; keep
   direct remote log streaming where that is the real source.
4. Teach the TUI to subscribe instead of owning its own polling loop.

## Remaining Work

- Coalesce same-resource subscriptions inside the daemon so multiple clients
  share one refresh path instead of one server-side loop per connection.
- Add subscriber diagnostics (`weft daemon clients` or `weft debug watchers`).
- Move plain job/system watch, log follow, and TUI refreshes onto the daemon
  subscription API.

## Operational Requirements

- Wait/watch commands may be unbounded. A deadline is optional and should come
  from an explicit user request or command flag such as `--wait-timeout`.
- Client disconnect must remove the watcher without waiting for a DB heartbeat.
- A stale daemon restart must close client sockets so clients can reconnect to
  the new daemon or exit with a clear message.
- Diagnostics must distinguish daemon work from client watchers and unrelated
  CLI one-shot commands.
