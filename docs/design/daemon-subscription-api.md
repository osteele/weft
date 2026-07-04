# Daemon Subscription API

The daemon subscription API is Weft's long-term local live-state boundary.
It is a Unix-domain-socket protocol used by trusted local clients such as
the CLI, TUIs, a web bridge, and macOS companion apps.

The API is intentionally local-only. Remote and mobile clients should talk to
a small authenticated bridge on the Mac, not directly to the socket.

## Layering

Weft keeps separate boundaries for separate responsibilities:

1. **Core services** own validation, database mutations, and reconciliation.
2. **Daemon subscriptions** own live-state fanout and repeated reads.
3. **CLI JSON/JSONL** is the stable scripting surface, backed by daemon
   subscriptions where available.
4. **Web, menu-bar, and mobile bridges** adapt daemon subscriptions to
   HTTP/WebSocket or platform-native notifications.
5. **TUIs** should become daemon subscribers for live state while keeping UI
   state local to Bubble Tea models.

SQLite remains an internal source of truth, not an integration contract.
External projects should use CLI JSON/JSONL today or a daemon-backed bridge
once one exists.

## Transport

The daemon listens on a Unix domain socket under `~/.cache/weft/daemon.sock`.
Each connection carries one JSON request followed by a newline. Subscription
requests keep the connection open and stream newline-delimited JSON events
until completion, timeout, daemon shutdown, or client disconnect.

The daemon closes the subscription when the client closes the socket. Clients
should reconnect rather than polling SQLite directly.

## Request

```json
{
  "type": "subscribe",
  "client_pid": 12345,
  "subscribe": {
    "resource": "job_status",
    "job_ids": [1531],
    "timeout_seconds": 3600,
    "poll_seconds": 1
  }
}
```

Common fields:

| Field | Meaning |
| --- | --- |
| `type` | Must be `subscribe`. |
| `client_pid` | Local client PID for diagnostics. |
| `subscribe.resource` | Subscription resource name. |
| `subscribe.timeout_seconds` | Optional subscription deadline. |
| `subscribe.poll_seconds` | Optional minimum polling interval; default is one second. |
| `subscribe.follow` | Resource-specific flag to keep streaming after idle state. |
| `subscribe.include_delta` | Resource-specific flag to include transition deltas. |
| `subscribe.include_status_line` | Resource-specific flag to include deterministic summary counts. |

## Events

Every subscription first emits a ready event:

```json
{
  "type": "subscription_ready",
  "api_version": 1,
  "resource": "job_status",
  "subscription_id": "12345:..."
}
```

Snapshot events use the same v1 job snapshot shape as CLI watch JSONL:

```json
{
  "type": "subscription_snapshot",
  "api_version": 1,
  "resource": "job_status",
  "subscription_id": "12345:...",
  "snapshot": {
    "type": "snapshot",
    "timestamp": "2026-04-27T10:30:00Z",
    "jobs": [
      {
        "job_id": "wj1531",
        "id": 1531,
        "status": "running",
        "host": "cool30",
        "project": "structural-probes",
        "instance_id": 1542,
        "exit_code": null
      }
    ]
  }
}
```

Completion uses `done` and may include the final snapshot:

```json
{
  "type": "done",
  "api_version": 1,
  "resource": "job_status",
  "subscription_id": "12345:...",
  "snapshot": {"type": "snapshot", "timestamp": "...", "jobs": []}
}
```

Errors use `error`:

```json
{
  "type": "error",
  "resource": "job_status",
  "subscription_id": "12345:...",
  "error": "context deadline exceeded"
}
```

Clients must ignore fields they do not understand. New fields may be added to
API version 1, but existing v1 fields must not be renamed or removed without a
major-version bump.

## Resources

### `job_status`

Watches specific jobs by numeric ID.

Request fields:

| Field | Meaning |
| --- | --- |
| `job_ids` | Required numeric job IDs. |
| `timeout_seconds` | Optional wait deadline. |

The subscription emits a snapshot whenever the effective status payload changes.
It emits `done` once all watched jobs are terminal.

### `project_watch`

Watches active jobs plus recent terminal jobs, optionally filtered by project.
This is the daemon-side data boundary for `weft project watch` and future
project-oriented TUIs.

Request fields:

| Field | Meaning |
| --- | --- |
| `project` | Optional exact project label. |
| `recent_seconds` | Recent terminal-job window; default is 24 hours. |
| `follow` | When true, continue streaming while idle. |

The subscription emits a snapshot whenever the job payload changes. If
`follow` is false, it emits `done` when there are no active jobs.

### `activity`

Streams the richer activity model used by `weft narrate`. This is the daemon
boundary for status surfaces that need semantic state rather than raw job rows.

Request fields:

| Field | Meaning |
| --- | --- |
| `project` | Optional exact project label. |
| `include_delta` | Include a `narrate.Delta` against the previous emitted snapshot. |
| `include_status_line` | Include deterministic status-line counts and unprocessed inbox counts. |
| `follow` | When true, continue streaming while idle. |

Activity events use the same outer `subscription_snapshot` envelope and carry
an `activity` payload:

```json
{
  "type": "subscription_snapshot",
  "api_version": 1,
  "resource": "activity",
  "subscription_id": "12345:...",
  "activity": {
    "snapshot": {
      "time": "2026-04-27T10:30:00Z",
      "jobs": {},
      "instances": {},
      "autopilot": {"state": "idle"}
    },
    "delta": {},
    "status_line": {
      "running_jobs": 1,
      "queued_jobs": 2,
      "active_instances": 1,
      "autopilot_state": "idle"
    },
    "unprocessed": {
      "completed": 1,
      "failed": 0
    },
    "unprocessed_jobs": [
      {
        "id": 1532,
        "status": "completed",
        "project": "structural-probes",
        "command": "python eval.py",
        "exit_code": 0
      }
    ],
    "formatted_snapshot": "{\"at\":\"...\",\"jobs\":[]}",
    "formatted_delta": "1 job added."
  }
}
```

The structured `snapshot`, `delta`, and `status_line` fields are for native
clients. `unprocessed_jobs` contains the recent unprocessed terminal-job rows
that correspond to the `unprocessed` counts, so clients can show completed or
failed job details on first connect without waiting for a transition delta. The
formatted fields match the compact prompt inputs used by `weft narrate`, so the
CLI can sit on the same daemon resource without recreating DB queries in the
command process.

## Compatibility

The older `watch_jobs` request remains supported for `weft status --wait`
compatibility. New clients should use `subscribe` with
`resource: "job_status"`.

CLI JSON/JSONL remains the recommended external process boundary. Over time,
those commands should prefer daemon subscriptions internally and retain direct
database fallback only for resilience.
