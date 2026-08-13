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
External local projects should use CLI JSON/JSONL or the versioned daemon
subscription described below. Remote clients should use an authenticated
bridge rather than access SQLite or the Unix socket directly.

## Canonical read-only job feed

Local GUI consumers should use the versioned `activity` subscription rather
than query SQLite or repeatedly spawn `weft job list`. The canonical request
is one newline-terminated JSON object sent to `~/.cache/weft/daemon.sock`:

```json
{
  "type": "subscribe",
  "client_pid": 12345,
  "subscribe": {
    "resource": "activity",
    "follow": true,
    "include_delta": true,
    "include_status_line": true
  }
}
```

This is a cached/no-sync read: handling the subscription reads local database
state and does not contact hosts or providers. The first
`subscription_snapshot` is a complete initial snapshot. Later snapshots carry
the complete current active-job map plus an optional delta. The
`unprocessed_jobs` array carries the bounded terminal-job inbox (at most the
last 14 days), including jobs that finished before the client connected.
Consumers that only need one project may add `"project": "<name>"`.

The feed is naturally bounded by active jobs, active instances, and the
14-day unprocessed inbox. Clients should reconnect after daemon restart and
replace their local model from the new initial snapshot.

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
    "runaway_breakers": [
      {
		"scope": "campaign 42, project=augur",
		"campaign_id": 42,
		"project": "augur",
		"reason": "repeated_infrastructure_failures_without_progress",
		"tripped_at": "2026-04-27T08:30:00Z",
		"chain": 1,
		"orphaned": 2,
		"infra_failures": 3,
		"spend_cents": 125,
		"window": "24h0m0s"
      }
    ],
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
clients. `runaway_breakers` is omitted when no breaker is active; a non-empty
collection means autopilot has stopped automatic relaunches in at least one
scope visible to the subscription. `tripped_at` is the stable timestamp clients
can use to distinguish a fresh trip from an old one without requiring
per-second payload updates. `reason` is either
`repeated_launch_failures_without_progress` or
`repeated_infrastructure_failures_without_progress`. Project-filtered
subscriptions include matching project scopes and global (`project=<all>`)
scopes. `unprocessed_jobs` contains
the recent unprocessed terminal-job rows that correspond to the `unprocessed`
counts, so clients can show completed or failed job details on first connect
without waiting for a transition delta. The formatted fields match the compact
prompt inputs used by `weft narrate`, so the CLI can sit on the same daemon
resource without recreating DB queries in the command process.

The daemon refreshes runaway-breaker state at most once every five seconds and
shares that result across activity subscribers. This bounds the lifecycle-event
query cost while keeping trip and reset notifications prompt.

### Activity job schema (API v1)

Both `activity.snapshot.jobs` and `activity.unprocessed_jobs` use the same job
object. Snapshot jobs are keyed by numeric ID; each object also contains `id`.

| Field | Meaning |
| --- | --- |
| `id` | Stable numeric logical-job ID. Display it as `wj<id>`. |
| `status` | Status stored in SQLite. This is retained for audit and compatibility. |
| `effective_status` | Status clients should render; applies the documented pending-intent/unplaced refinement from `Job.EffectiveStatus()`. |
| `project` | Stored project label. |
| `host` | Current host target, when assigned. |
| `instance_id` | Numeric rental-instance ID, when assigned. |
| `description` | Effective description: user text, generated text, then command fallback. |
| `command` | Compatibility command summary, capped at 200 bytes. |
| `command_full` | Complete effective command; new consumers should prefer this field. |
| `tags` | Stored job tags. |
| `gpu` | Assigned device indices when known, otherwise the requested GPU class. |
| `gpu_class`, `gpu_mem_gb` | Structured requested GPU constraints. |
| `exit_code` | Process exit code when known. |
| `start_time` | Unix seconds at authoritative execution start. It is never a placement or queue timestamp. |
| `end_time` | Unix seconds at terminal completion when known. |
| `placement_bucket` | Current semantic display bucket (`running`, `paused`, `placing`, `launching`, `queued`, `unplaced`, `completions`, `failures`, or `killed_canceled`). |
| `placement_at` | Unix seconds at the start of the current placement condition. Omitted outside a timestamped placement condition. |
| `state_since` | Best authoritative Unix timestamp for the current `placement_bucket`: execution start for `running`, placement/queue epoch for placement buckets, and end time for terminal buckets. Omitted when Weft did not record a matching transition timestamp (notably some paused/legacy rows). |
| `source` | Bounded source provenance: working directory, source identity and pinned-snapshot hashes, source roots, and VCS revision/change ID/dirty flag. Blob manifests and storage keys are intentionally excluded. |

Durations are intentionally absent. Clients calculate elapsed or state age from
the snapshot's RFC3339 `time` and the applicable Unix timestamp. In particular,
clients must not treat `placement_at` as execution start: use `start_time` for
execution elapsed time and `state_since` for the age of the current display
bucket.

All fields added to API v1 are additive. Clients must continue ignoring unknown
fields. A field with unknown evidence is omitted rather than populated from a
timestamp with different semantics.

## Compatibility

The older `watch_jobs` request remains supported for `weft status --wait`
compatibility. New clients should use `subscribe` with
`resource: "job_status"`.

CLI JSON/JSONL remains the recommended external process boundary. Over time,
those commands should prefer daemon subscriptions internally and retain direct
database fallback only for resilience.
