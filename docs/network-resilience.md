# Network Resilience

Remote Jobs is built for workflows where laptops roam between Wi-Fi networks,
SSH servers hiccup, and VPNs flap. This document explains the main techniques
the CLI uses to keep jobs alive and observable despite unreliable links.

## Persistent execution

Every job runs inside a tmux session on the remote host. Once tmux is launched,
the job survives:

- Local SSH disconnects
- Laptop sleep / network changes
- Dropped VPN tunnels

Metadata, PID files, log files, and status files are written to
`~/.cache/remote-jobs/logs/` on the host so that the CLI can reconstruct the job
state even if it was restarted elsewhere.

## Connection-aware job submission

When you start a job (`remote-jobs run` or `remote-jobs job start`), the CLI
records the job locally before touching the remote host. If SSH cannot be
established, the job is deferred to the remote queue automatically. A
`deferred_operations` entry is created and `remote-jobs sync` (or any command
that syncs) will add the job back to the host's queue when it becomes
reachable. No work is lost, and you no longer need a separate retry command.

Queue submissions behave the same way. `plan submit` and `queue add` now
spool queue entries locally whenever the host is unreachable. The job remains
visible in the local database immediately, and the next sync run replays the
append operation. If the user didn't disable auto-start and the job has no
dependencies, the queue runner is automatically started as soon as the host
comes back so the job begins right away.

Queued jobs that are manually started via `remote-jobs job start` use the same
mechanism. If the host goes down while removing the entry from the queue file,
the CLI records exactly what happened and replays it later.

## Deferred operations + sync

Anything that must touch the remote host (moving queue entries, starting queued
jobs, or killing sessions) goes through the deferred operations table when the
host is unreachable. A later `remote-jobs sync`:

1. Detects that the host is back.
2. Replays the recorded operations (re-append queue entries, start tmux,
   remove queue lines, etc.).
3. Cleans up the deferred rows so the queue stays consistent.

This approach lets you issue commands from the subway or airplane without
needing a live SSH connection at that moment.

## Monitoring while offline

Blocking commands (`remote-jobs status --wait`, `remote-jobs plan submit --watch`)
now treat intermittent SSH failures as informational instead of fatal:

- When a host drops, the CLI prints `Connection to HOST is unavailable. Polling
  will continue...` and keeps waiting.
- When the connection returns, it prints `Connection to HOST restored. Resuming
  monitoring.` before reporting fresh statuses (unless the job already finished,
  in which case you only see the final result).

This makes it safe to start a wait on one network and finish it on another;
the CLI always falls back to the local database and periodically re-syncs when
possible.

## Offline logs

Completed logs are mirrored into `~/.cache/remote-jobs/logs/` so `remote-jobs log`
and the TUI can show output even while offline. Entries honor the max-age and
size limits from `config.yaml`. `remote-jobs sync` keeps the cache fresh and
prunes expired files, and cache hits are served instantly without touching SSH.

## Recovering job status

`remote-jobs sync` continuously reconciles local records with the remote host:

- Detects tmux sessions that vanished and marks jobs as `dead`.
- Reads status files to mark jobs as `completed` with their exit code.
- Updates `start_time` from metadata if the queue runner launched the job.

Because the job record lives locally, you can always inspect, search, or clean
up jobs even while offline. The next sync run will fold in remote changes once
the host is reachable again.

## Summary

Remote Jobs expects unreliable networks. Jobs keep running on the host, all
operations are queued locally when a host is down, and sync/watch commands are
explicitly connection-aware. Together these primitives let you start work from
any network, wander freely, and come back later knowing the CLI has been
tracking everything for you.
