# Sync Architecture

Source trees, data inputs, and results move between the laptop, on-prem hosts,
and cloud instances. Authoritative behavior lives in
`specs/source-data-sync.allium` (source snapshots, provenance, prestaging)
and `specs/upload-drain.allium` (agent-side result uploads).

## Inventory queue transport

Inventory hosts normally receive queue commands by direct SSH append. A host
configured with `queue_transport = "r2_pull"` instead uses a host-addressed R2
mailbox under `inventory/v1/hosts/<host>/`. The controller writes versioned
command envelopes to `inbox/`; the host daemon, using outbound access only,
appends each command to the same local JSONL queue used by SSH dispatch, writes
an acknowledgement, and removes the inbox object. Delivery is at least once;
queue operations and attempt IDs provide idempotence across a crash between
the local append and acknowledgement.

The daemon also replaces `state.json` with a timestamped runner-state envelope
every two seconds. The controller accepts it only while fresh and treats a
missing, malformed, wrong-host, wrong-version, or older-than-30-seconds snapshot
as unknown. It never infers an empty queue from stale R2 data. Completed jobs
continue through the existing inventory result markers and completion records.

This transport does not perform placement and does not introduce a relay: the
controller still owns the job database and writes directly to the selected
host's mailbox. It reuses the submit-time R2 source closure described below, so
dispatch requires neither SSH nor rsync. Legacy unpinned jobs and inventory
jobs with `--needs` still require the direct-host staging path.

### Agent identity and queue compatibility

The queue runner writes three identity fields to `default.state.json`: its
source fingerprint, the highest queue-command protocol it understands, and the
state timestamp. Direct SSH status probes and R2 runner-state envelopes cache
these fields in the local database. Successful agent deployment verification
separately records the fingerprint that ran after installation. The deployed
and running observations have independent timestamps; a missing observation
means the controller has not confirmed that fact.

Every new queue command carries `protocol_version`. Version 0 remains the
legacy unversioned schema; current writers emit version 1. Before appending a
command, the controller requires runner state that reports a compatible
protocol. Missing state and older protocols defer dispatch with an instruction
to run `weft queue update HOST`. If an agent encounters a command from a newer
protocol, it leaves its cursor at that command and exits with a compatibility
error so an updated agent can process the command. Unsupported operations fail
the same way instead of being discarded.

`weft host agent-status` projects the cached observations for every inventory
host without contacting any host. It resolves the desired fingerprint for each
host's OS and architecture, and reports `current` only when that fingerprint
matches the deployed and running fingerprints, the runner protocol is
compatible, and the running observation is at most two minutes old. Older
running observations are `stale-observation`, not evidence that the recorded
version is still running. `--json` emits schema version 2; each host row carries
its target-specific desired version or a resolution error, plus the source
timestamps.

## Source sync: the working tree, not commits

Weft ships the working directory's **filesystem state**. Uncommitted edits are
included; files that exist only in another git/jj revision are not. This
is the single most common source of "works here, fails there" confusion; see
`docs/guides/workflow-guide.md` § "Source sync".

The submit-time source closure is the execution identity on every queue-runner
target:

- **Inventory queue runners** (`internal/ops/host_sync.go`): jobs with a
  submit-time pin receive the complete source manifest in their queue payload.
  The agent verifies the manifest hash and each canonical-tar hash, downloads
  every root and diverted blob, and runs from a per-job directory. Changes to
  the submitter's working tree after `weft run` therefore cannot change the
  executed source. Queue runners that predate manifest support fail closed on
  the closure-receipt compatibility key rather than running a partial root.
  Rows created before submit-time pinning retain the legacy rsync path with
  `DefaultExcludes`, `.gitignore`, and `.weftignore`.
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
  Symlinks whose targets resolve within the root remain in the tarball.
  Out-of-root links (absolute targets, or relative ones leaving the tree)
  are dereferenced at creation: the target's content is snapshotted as
  plain entries under the same source excludes as in-tree entries, so
  trees that vendor out-of-tree content as absolute
  symlinks (e.g. `~/.claude/skills`) produce self-contained archives
  instead of failing extraction on the remote host. Extraction still
  rejects tar-slip entries (absolute paths, `..` escapes, and out-of-root
  symlink targets; see `archive.go`).
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

For pinned inventory jobs, the queue runner's preflight
(`internal/runner/runner.go`) verifies and materializes the immutable manifest.
For legacy rsync jobs, it verifies the per-job marker against the queued SHA;
persistent mismatch escalates the job to the older single-tarball
**R2-isolated dispatch** fallback.

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
| `internal/inventoryqueue` | Versioned host-addressed inventory mailbox and runner-state protocol |
| `internal/prestage`, `internal/dataloc` | HF asset prestaging and locality |
| `internal/ops/host_sync.go` | Dispatch-time source/data staging for on-prem queues |
| `cmd/agent/upload_drain.go`, `cmd/agent/bgwork.go` | Agent-side drain wiring and background uploads |
