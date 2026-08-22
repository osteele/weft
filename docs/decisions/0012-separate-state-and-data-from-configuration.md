---
status: accepted
date: 2026-08-21
---

# 0012. Separate state and data from configuration

## Context and Problem Statement

Weft keeps configuration, a growing operational database, database snapshots,
and durable local artifacts under `~/.config/weft`. The database and artifacts
are not configuration: they are large, mutable, and have different backup and
retention requirements. Mixing them with declarative configuration makes
configuration backup, inspection, and relocation unnecessarily expensive.

Weft also runs a persistent launchd daemon. Every CLI and daemon process must
resolve the same state and data directories; otherwise two processes can open
different databases while appearing to operate on the same installation.

## Decision Outcome

Weft follows the XDG base-directory roles for local persistent storage:

- configuration and host inventory use `XDG_CONFIG_HOME/weft`, defaulting to
  `~/.config/weft`;
- operational databases, database snapshots, and lightweight UI state use
  `XDG_STATE_HOME/weft`, defaulting to `~/.local/state/weft`;
- durable local artifacts use `XDG_DATA_HOME/weft`, defaulting to
  `~/.local/share/weft`;
- rebuildable files, logs, sockets, and process metadata remain cache data.

Only absolute XDG overrides are honored. The launchd service records the
absolute XDG overrides present when it is installed so it resolves the same
directories as the installing CLI.

### Consequences

- Existing installations must stop every database writer before relocating
  SQLite files, then reinstall the daemon before resuming work.
- Changing an XDG override requires reinstalling the daemon.
- State backups no longer sweep durable artifact bytes, and configuration
  backups no longer sweep either category.
- A user who wants all persistent Weft files under one custom root must set
  both `XDG_STATE_HOME` and `XDG_DATA_HOME`; one variable no longer relocates
  every persistent file.
- This decision does not determine whether raw historical telemetry remains in
  SQLite or moves to an archival format.

## Considered Options

### Keep all persistent files under `XDG_CONFIG_HOME`

Rejected: a multi-gigabyte database and artifact store are not configuration,
and their growth and retention policies make the configuration directory
costly to manage.

### Put databases and artifacts together under `XDG_DATA_HOME`

Rejected: the operational database is application state and benefits from a
separate lifecycle from durable job outputs. Combining them recreates the
backup and retention coupling under a different parent directory.

### Move only the database

Rejected: leaving the durable artifact store under the configuration tree
preserves the largest category error and most of the practical inconvenience.

## More Information

- **References**: XDG Base Directory Specification; `internal/appdirs`
