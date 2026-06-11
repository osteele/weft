# Sync Architecture

How source trees, data inputs, and results move between the laptop, on-prem
hosts, and cloud instances. Authoritative behavior lives in
`specs/source-data-sync.allium` (source snapshots, provenance, prestaging)
and `specs/upload-drain.allium` (agent-side result uploads).

## Source sync: the working tree, not commits

Weft ships the working directory's **filesystem state** — uncommitted edits
are included; files that exist only in another git/jj revision are not. This
is the single most common source of "works here, fails there" confusion; see
`docs/guides/workflow-guide.md` § "Source sync".

Two transport paths share the same snapshot semantics:

- **On-prem** (`internal/sync/sources.go`): rsync with `DefaultExcludes`
  (VCS dirs, caches, `.weft-source*.sha256` markers) plus
  `.gitignore`/`.weftignore`. rsync's delta transfer makes re-syncing an
  unchanged tree near-free, so there is no DB-side "recently synced" skip
  cache (the spec's `HostSourceState` is documented as superseded).
- **Cloud** (`internal/sync/r2upload.go`): a deterministic gzip tarball
  (`tarball.go` — zeroed mtimes/uids so identical trees hash identically),
  content-addressed in R2 as `sources/<sha256>.tar.gz` and deduped by an
  existence probe. Symlinks are shipped and re-created on extraction;
  extraction rejects tar-slip entries (absolute paths, `..` escapes,
  out-of-root symlink targets — `archive.go`).

**Size cap**: `MaxSourceTarballBytes` (500 MiB) bounds the uncompressed
snapshot. Overflow errors (`ErrSourceTooLarge`) are overlay-aware: a plain
tree overflow advises `.gitignore`/`.weftignore`; an overflow caused by
declared `local:` inputs names them and points at the asset store
(`weft data publish` / `--input asset:`), because overlays deliberately
bypass excludes. `EstimateSnapshotBytesWithInputs` provides the same answer
without staging copies, used by the pre-claim validation in the reuse path.

**`local:` input overlays** (`extras.go`, `r2upload.go`): declared
`local:path` inputs are staged into the snapshot even when gitignored, with
path-escape rejection. Overlay contents are counted once.

## Provenance

`ComputeSourceSHA256` hashes the tarball itself, so anything that changes
snapshot content (including symlink targets) changes the hash. Two markers
record what was synced (`provenance.go`):

- the **rolling** per-working-dir marker (`.weft-source.sha256`),
  overwritten by any sync to that directory; and
- the **per-job** marker (`.weft-source.<job>.sha256`), written at dispatch
  so a peer job re-syncing the same directory cannot invalidate this job's
  preflight.

The queue runner's preflight (`internal/runner/runner.go`) verifies the
per-job marker against the queued SHA before starting; persistent mismatch
escalates the job to **R2-isolated dispatch** (the job runs from its own
content-addressed tarball, skipping the shared rsync tree — see
`EscalateToR2IsolatedSource` in the spec and `internal/ops/host_sync.go`).

## Data inputs

Declared inputs (`--input`, `[tool.weft] inputs`) drive prestaging before
dispatch: HF models rsync from a donor host or download directly
(`internal/prestage`, `internal/dataloc`); artifacts and checkpoints resolve
through R2 (`internal/r2resolve`, `internal/cloudneeds`). On-prem staging of
cloud-produced artifacts is implemented; the reverse (host→R2 staging for a
cloud consumer of an on-prem artifact) is specified but not yet implemented
(`CloudDataStagingViaR2` in the spec).

## Results and the upload drain

On-prem jobs leave outputs in place (collected per `.weft.toml` output
dirs); cloud agents upload results to R2 under
`jobs/<id>/runs/<run>/artifacts/`, with the final log and completion record.

The agent-side **upload drain** (`internal/r2upload/drain.go`,
`cmd/agent/upload_drain.go`) supervises each rclone upload with five gates:
never-started, mid-transfer stall, heartbeat, slow-pace, and an absolute
ceiling. A failed drain writes an `upload-failure.json` marker carrying the
`StallKind` discriminator (`never_started` / `mid_transfer` / `heartbeat` /
`slow_pace`) that `weft log` and `weft instance diagnose` render. The marker
is written **before** any self-destruct decision; consecutive stalls trip an
instance self-destruct so a network-dead rental doesn't burn money.
Benchmark-tagged jobs get an upload barrier so background uploads finish
before the next job starts (`SharedWorkdirUploadBarrierBeforeNextJob`).

## File map

| Package | Role |
|---|---|
| `internal/sync` | Snapshot/tarball/extraction, rsync source sync, `local:` overlays, provenance markers, size estimation |
| `internal/syncorch` | Orchestrated sync passes: cloud result ingestion, completion markers |
| `internal/cloudsync` | Cloud-side sync helpers |
| `internal/r2upload` | Upload drain, stall detection, failure markers |
| `internal/r2`, `internal/r2keys`, `internal/dataplane` | R2 client and key conventions |
| `internal/prestage`, `internal/dataloc` | HF asset prestaging and locality |
| `internal/ops/host_sync.go` | Dispatch-time source/data staging for on-prem queues |
| `cmd/agent/upload_drain.go`, `cmd/agent/bgwork.go` | Agent-side drain wiring and background uploads |
