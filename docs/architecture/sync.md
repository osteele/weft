# Sync Architecture

Source trees, data inputs, and results move between the laptop, on-prem hosts,
and cloud instances. Authoritative behavior lives in
`specs/source-data-sync.allium` (source snapshots, provenance, prestaging)
and `specs/upload-drain.allium` (agent-side result uploads).

## Source sync: the working tree, not commits

Weft ships the working directory's **filesystem state**. Uncommitted edits are
included; files that exist only in another git/jj revision are not. This
is the single most common source of "works here, fails there" confusion; see
`docs/guides/workflow-guide.md` § "Source sync".

Two transport paths share the same snapshot semantics:

- **On-prem** (`internal/sync/sources.go`): rsync with `DefaultExcludes`
  (VCS dirs, caches, `.weft-source*.sha256` markers) plus
  `.gitignore`/`.weftignore`. rsync's delta transfer makes re-syncing an
  unchanged tree near-free, so there is no DB-side "recently synced" skip
  cache (the spec's `HostSourceState` is documented as superseded).
- **Cloud** (`internal/sync/r2upload.go`): submission captures and uploads an
  immutable source manifest before the job row becomes visible. After `local:` overlays are applied,
  regular files of at least 8 MiB are diverted into raw content-addressed
  `assets/<sha256>` objects. The residual tree becomes a deterministic gzip
  tarball (`tarball.go`, with zeroed mtimes/uids so identical trees have the
  same canonical tar stream), stored as
  `sources/v2/sha256/<canonical-tar-sha256>.tar.gz`. The gzip representation is
  produced in parallel but is not the identity. Large-file hashing and gzip
  use at most half of `GOMAXPROCS`, reduced further by the host's one-minute
  load average so source staging leaves capacity for other processes. Both
  object kinds are deduped by existence probes. Bootstrap and agent refresh
  extract the tarball, materialize each blob at its root-relative path, and
  verify each raw blob's SHA-256.
  Symlinks remain in the tarball; extraction rejects tar-slip entries
  (absolute paths, `..` escapes, and out-of-root symlink targets; see
  `archive.go`).
  Fresh launches and instance reuse consume the stored per-job manifest. Rows
  created before submit-time pinning have no pin and retain dispatch-time
  snapshot derivation.

  Attempts submitted by older clients retain their exact
  `sources/<gzip-sha256>.tar.gz` v1 key in bootstrap and manifest metadata.
  Retrieval consumes that recorded key verbatim; v1 objects are not rekeyed or
  deleted as part of the v2 transition.

**Size cap**: `MaxSourceTarballBytes` (500 MiB) bounds the uncompressed
residual tarball, after large-file diversion. Overflow errors
(`ErrSourceTooLarge`) are overlay-aware and explain that the remaining small
files must be reduced; overlays deliberately bypass excludes.
`EstimateCloudSourceTarballBytesWithInputs` provides the same residual answer
without staging copies, used by pre-claim validation in the reuse path.

**`local:` input overlays** (`extras.go`, `r2upload.go`): declared
`local:path` inputs are staged into the snapshot even when gitignored, with
path-escape rejection. Overlay contents are counted once.

## Provenance

`ComputeSourceSHA256` hashes the canonical uncompressed tar stream, so anything
that changes snapshot content (including symlink targets) changes the hash
without tying provenance to a particular gzip encoder. Two markers record what
was synced (`provenance.go`):

- the **rolling** per-working-dir marker (`.weft-source.sha256`),
  overwritten by any sync to that directory; and
- the **per-job** marker (`.weft-source.<job>.sha256`), written at dispatch
  so a peer job re-syncing the same directory cannot invalidate this job's
  preflight.

The queue runner's preflight (`internal/runner/runner.go`) verifies the
per-job marker against the queued SHA before starting; persistent mismatch
escalates the job to **R2-isolated dispatch** (the job runs from its own
content-addressed tarball, skipping the shared rsync tree; see
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
