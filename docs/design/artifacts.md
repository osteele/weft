# Artifact Store

This document describes how weft captures, stores, lists, retrieves, and
stages job outputs. It documents the system as implemented today, names the
invariants the design intends to hold, identifies where the implementation
falls short of them, and records the design decision about whether R2 access
may remain optional for on-prem hosts.

Related material:

- `specs/job-lifecycle.allium` — normative rules for the cloud upload side
  (pre-registration, size caps, timeouts, the shared-workdir upload barrier)
  and the `artifact get --all` destination contract.
- `docs/guides/workflow-guide.md` — user-facing behavior of `--produces`,
  `--needs`, and output declaration.
- `docs/design/architecture.md` — overall system shape (CLI/TUI on the
  control machine, durable agents on hosts).

## Concepts

**Output** — a file a job writes that weft tracks. Outputs are discovered two
ways:

- **Convention**: anything written under `output/` or `outputs/` in the
  working directory (configurable via `output_dirs` in `.weft.toml`).
- **Declaration**: paths named via `--output local:<path>` /
  `--produces <path>`, or `outputs` in the script's `[tool.weft]` block.
  Declared paths may live outside the convention directories.

**Artifact** — an output that has been registered in a per-job manifest, on
the host at `~/.cache/weft/artifacts/<jobID>.json` (`$WEFT_ARTIFACT_MANIFEST`,
which job scripts may also append to directly). Declared outputs are
registered automatically at job start and again post-exit
(`ProducesPreRegisteredAtJobStart` in the spec).

**Producer / consumer** — a consumer job declares `--needs <path>:<job-id>`;
weft must materialize the producer's artifact on the consumer's host before
the consumer starts ("staging").

## The four places bytes (or claims about bytes) live

The subsystem maintains one durable store, one replication tier, one pointer
index, and one implicit source of truth. Understanding their differences is
the key to understanding the subsystem's failure modes.

| Store | Where | Contents | Durability |
|---|---|---|---|
| **Local artifact cache** | `~/.local/share/weft/artifacts/<jobID>/...` on the control machine (`$XDG_DATA_HOME/weft/artifacts/`) + `artifacts` DB table (path, stored path, size, sha256, attempt id) | Bytes copied from a host or downloaded from R2 | Durable; survives workdir reuse |
| **R2 per-run objects** | `jobs/<id>/runs/<run>/outputs/...` and `jobs/<id>/runs/<run>/artifacts/files/...` (+ `artifacts/manifest.json`) | Bytes uploaded by an agent during/after the run | Durable; keyed per run, immune to overwrite by later jobs |
| **Pointer records** | `host_data` rows of kind `job-output`, asset id `<jobID>/<relPath>` (`internal/ops/artifacts.go RecordJobOutputs`) | Host + path only — no bytes, no size, no hash | Declared-output pointers (including `--produces` entries) dereference into the immutable completion-time snapshot under `~/.cache/weft/artifacts/<jobID>/<run>/outputs/` on the host; pointers for **undeclared** convention outputs still dereference into the live working directory, which the next job may overwrite |
| **Host-live files** | The job's working directory on the host | Whatever is currently on disk | None — shared mutable state across jobs in the same directory |

Pointer records exist primarily for **placement** (data-locality scoring and
pre-staging cost estimation), but `artifact list` also surfaces them as
retrievable artifacts. That dual use was the source of the list/get
mismatch class of bugs; it is now mediated by the shared resolver (see
"Retrieval" below), which gives each pointer row an explicit retrieval
handle (the on-demand host sync).

## Capture: how bytes become durable

Capture behavior differs by target kind. This is the asymmetry at the heart
of the current design:

```
                         job completes (exit 0)
                                 │
        ┌────────────────────────┼─────────────────────────┐
        ▼                        ▼                         ▼
  cloud rental            on-prem host,             on-prem host,
  (agent + R2)            runner has R2             no R2
        │                        │                         │
  upload output/ +         same as cloud            snapshot declared
  manifest entries to      (uploadOutputDirs,       outputs + --produces
  per-run R2 keys;         manifest entries,        entries to the host's
  same-workdir barrier     workdir barrier)         per-run artifact dir;
  before next job                │                  pointer records point
        │                        │                  at the snapshot
   durable per run          durable per run                │
                                                    durable per run
                                                    (host-local); undeclared
                                                    convention outputs stay
                                                    lazy-sync only
```

- **Cloud rentals and R2-enabled on-prem runners** snapshot at completion:
  `uploadArtifactManifestEntries` publishes declared paths first, then
  `uploadOutputDirs` walks the convention directories with `--update`, both
  under per-run R2 keys (`cmd/agent/runinstance.go`, wired for on-prem via
  `cmd/agent/inventory_r2.go` when `run-queue` is started with
  `--r2-bucket`). A declared workdir-relative path beneath an effective
  convention output directory uses its `outputs/` key as its sole payload
  object. Its manifest name and path remain logical references to that object.
  Absolute paths, paths outside convention directories, and declarations with
  a custom artifact root retain `artifacts/files/` backing. Lexical duplicates
  and paths covered by a declared ancestor directory share one upload. A
  per-workdir barrier
  (`SharedWorkdirUploadBarrierBeforeNextJob`) prevents the next job in the
  same directory from starting while the prior job's upload walk is still
  running.
- **On-prem runners without R2** get a host-local completion-time snapshot
  for declared outputs: when the terminal transition is observed,
  `RecordJobOutputs` (`internal/ops/artifacts.go`) copies each `--output
  local:` path and each `--produces` manifest entry to
  `~/.cache/weft/artifacts/<jobID>/<run>/outputs/<relPath>` on the host over
  SSH, then records the `host_data` pointer against the snapshot path. The
  snapshot needs no R2 credentials; if the copy fails, no pointer into the
  mutable working directory is recorded (that would just re-open the race).
  Bytes reach the control machine only when it later runs `weft artifact
  sync` (SSH manifest fetch + scp, `internal/artifacts/sync.go`) or the
  convention-output rsync-back (`cmd/artifact.go syncJobOutputs`).

**Undeclared** convention-directory outputs on no-R2 hosts remain lazily
captured, and lazy capture is a race: the window between completion and the
first sync is unbounded, and any later job using the same working directory
and the same relative output path closes it destructively (the original
wb10 defect, now closed for declared outputs and `--produces` entries).

## Retrieval: list, get, cat, sync

### The shared resolver

Listing and retrieval consume one resolution authority:
`cmd/artifact.go resolveJobArtifacts` materializes the union of the three
artifact sources — the local cache (`artifacts` table), `host_data`
job-output pointer records, and a live listing of the job's per-run R2
prefixes (any target kind — inventory runners with `--r2-bucket` upload to
the same prefixes as rentals) — once per invocation. Each resolved row
carries its own retrieval handle (cache rows the stored local path, cloud
rows the listed R2 object key, host pointer rows the on-demand-sync
identity), and one matcher (`resolvedArtifact.matchesToken`: name, exact
path, basename, path suffix, with the `artifacts/` display prefix
canonicalized) serves both sides. `weft artifact list` prints exactly these
rows; `get`/`cat` (`deliverArtifactToken`) retrieve through the same rows,
so a listed row cannot resolve against a different store than the one that
produced it. Before this unification, list and get/cat re-listed R2
independently with different matchers, which produced four separate
list/get mismatch reports (wb11, wb16, wb20, wb40). The contract is
`ArtifactResolution` in `specs/job-lifecycle.allium`.

`weft artifact get` / `cat` resolve in this order:

1. the local artifact cache (`db.FindArtifactByNameOrPath`, scoped to the
   latest run first, then job-wide) — the fast path; serving cached bytes
   does not depend on R2 reachability. For terminal jobs, if the cache row
   was written at or before job end and R2 lists the same canonical path,
   retrieval uses the R2 object so final overwrites beat incremental
   snapshots;
2. the shared resolver's rows, matched with the shared matcher; cloud rows
   download the R2 object the listing named, with a per-transfer idle
   timeout (`--timeout`, default 2m of no progress) and a progress status
   line for large files; host pointer rows trigger the on-demand sync
   below and serve the freshly cached entry;
3. for tokens no listing source has recorded, an on-demand sync
   (`syncArtifactOnDemand`) that runs the same manifest-fetch/rsync path as
   `weft artifact sync` and then serves from the cache. On no-R2 hosts this
   reads the completion-time host snapshot for declared outputs (pointer
   records dereference into it); only undeclared convention outputs still
   read the live working directory and inherit the overwrite race.

When every rung misses, the error names each source checked.

`weft artifact sync` (per job or sweep) fetches the host manifest over SSH
and scp's each entry into the cache; if the manifest is missing it falls
back to convention-output rsync; for launch jobs it downloads the R2
manifest and files instead.

### Run-ID resolution

Artifacts in R2 are keyed by run (attempt). The authoritative resolution
policy lives in `internal/r2resolve.NeedR2Key`: try the latest run, then
run zero (legacy), then **list the actual `runs/` subdirectories in R2 and
probe each** — the last step is what finds artifacts uploaded by an attempt
that was later superseded or canceled, where `latest_run_id` has moved past
the run that actually produced the bytes.

The `cmd/artifact.go` retrieval paths delegate to this policy:
`artifactRunIDCandidates` wraps the recorded `[latest_run_id, 0]` pair and
appends runs discovered via `r2resolve.ListRunIDsFunc` (used by the shared
resolver's R2 listing and by manifest sync), and `resolveCloudOutputKey`
delegates key resolution to `r2resolve.NeedR2Key`. Superseded-run artifacts
are therefore visible to `artifact list`/`get` exactly as they are to
staging (`ArtifactRetrievalCoversAllRuns` in the spec).

## Staging: materializing `--needs` on the consumer's host

For a consumer queued on an on-prem host whose producer ran on a rental,
the per-host sync sweep stages bytes before dispatch
(`internal/ops/host_sync.go stageArtifactNeedsForHost`):

1. One SSH probe collects marker/file/staging-file state for all pending
   needs on the host.
2. Per need, a DB lease (`needs-stage:<host>:<marker>`, owner `host:pid`,
   TTL 15m) guards against duplicate concurrent transfers.
3. Size-match short-circuits: if on-host bytes already match the R2 object
   size, skip the transfer.
4. Otherwise: download from R2 **to a temp dir on the control machine**
   (5m idle timeout), scp to `<dest>.weft-staging` on the host, `mv` into
   place, write the satisfied marker.

On-prem→on-prem needs skip this path; the producer's queue runner writes
the satisfied marker directly at completion.

The control-machine relay exists because hosts are not assumed to have R2
credentials (see the decision section). Its weaknesses are structural:
every attempt restarts the R2 download from byte zero (no resume, although
the R2 client supports range requests); the bytes traverse the control
machine's downlink *and* uplink; an interrupted process (laptop sleep, TUI
exit) abandons a lease that blocks other processes for up to 15 minutes;
and a transfer slower than ~1.1 MB/s outlives its own lease, re-enabling
the duplicate-transfer race the lease exists to prevent. Progress is
visible only as low-level `queue.dispatch.deferred` lifecycle events.

## Intended invariants

These are the properties the design is meant to guarantee. They are stated
here so spec rules and tests can be derived from them.

1. **Retrievability**: every row `weft artifact list` prints is retrievable
   by `weft artifact get` (possibly via an automatic sync step), or is
   visibly labeled as lost.
2. **Per-job durability**: once a job completes successfully, its declared
   outputs survive subsequent jobs reusing the same working directory.
3. **Attribution**: bytes stored or uploaded under job J's keys were
   written by job J (no cross-attribution from workdir sharing).
4. **Error fidelity**: a retrieval or staging failure surfaces its real
   cause ("download stalled after 412 MB", not "artifact not found").
5. **Run completeness**: retrieval can find artifacts from any run that
   uploaded them, not only the latest recorded attempt.

## Known divergences (as of 2026-07-02)

| Invariant | Divergence | Tracker |
|---|---|---|
| Per-job durability | Undeclared convention-directory outputs on no-R2 hosts have no completion-time capture (declared outputs and `--produces` entries are snapshotted); the snapshot size cap / GC from the revised baseline is not implemented | wb10 (residual) |
| Attribution | The shared-workdir barrier serializes only sequential jobs within one agent process; concurrent same-workdir jobs (multi-GPU hosts) can still cross-attribute uploads (recorded as an open question in `specs/job-lifecycle.allium`) | (spec `invariant Attribution`, wj2226 incident) |

Resolved (see the corresponding rules in `specs/job-lifecycle.allium`):
retrievability (list and get/cat consume the shared resolver — contract
`ArtifactResolution`, rule `ArtifactListGetConsistency`; `get`/`cat`/`--all`
fall back to R2 for any job kind and to an on-demand host sync, and name
every source checked on a miss); per-job durability for declared outputs
(completion-time host-local snapshot —
`CompletedOutputsSurviveWorkdirReuse`); single-backing publication for
declared convention outputs (`DeclaredConventionOutputHasSingleBacking`);
error fidelity (transfer failures
propagate their real cause instead of "not found", and large single-file
downloads show progress — `ArtifactRetrievalErrorFidelity`); run
completeness (retrieval delegates to `r2resolve`, finding superseded-run
uploads — `ArtifactRetrievalCoversAllRuns`).

## Decision: must R2 remain optional for on-prem hosts?

> Recorded as [ADR 0004: Keep R2 optional for on-prem hosts](../decisions/0004-keep-r2-optional-for-on-prem-hosts.md).
> This section remains the detailed record; the ADR is the citable summary.

**Constraint under review.** The current design allows on-prem hosts with no
R2 access. The question is whether that constraint is compatible with fixing
the durability and staging defects, or whether requiring R2 everywhere would
buy enough simplicity to justify dropping it.

**What requiring R2 on every host would buy:**

- One capture path: every job, everywhere, uploads per-run keys at
  completion. The pointer-record retrieval surface disappears.
- One retrieval ladder: cache → R2. `get`, `list`, and `sync` unify.
- Staging becomes host-side pull everywhere; the control-machine relay and
  its lease machinery are deleted.

**What it would cost:**

- **Credential distribution**: R2 write credentials on every on-prem host,
  including shared or institutionally managed machines where weft runs as
  an unprivileged user. A leaked host is a leaked bucket.
- **Restricted egress**: some on-prem hosts have limited or no direct
  outbound internet access (the same constraint that motivates the HF
  mirror/offline workflows). For those hosts "require R2" means "weft
  cannot run", not "weft is simpler".
- **Cost and waste**: multi-GB outputs (representation tensors,
  checkpoints) would round-trip through R2 even when the producer and all
  consumers are on the same host or LAN.
- **Failure coupling**: completion would depend on R2 availability.

**Analysis.** The open bugs are not caused by R2 being optional. They are
caused by the no-R2 path having *no completion-time capture at all* (wb10),
by retrieval refusing to use the paths the no-R2 design provides (wb16),
and by transfer-layer defects that are independent of who holds credentials
(wb20). Durability requires a completion-time snapshot; it does not require
that the snapshot be in R2. A host-local snapshot — copying declared and
discovered outputs into a per-job directory under `~/.cache/weft/artifacts/`
on the host's own disk at completion — provides the same overwrite immunity
using nothing but the filesystem the job already wrote to.

**Decision: keep R2 optional, drop pointer-only capture.** R2-optionality
stays, but it no longer means "no capture". The revised baseline:

- **Capture (all hosts, no credentials needed)** — implemented for declared
  outputs: at successful completion, `RecordJobOutputs` snapshots declared
  outputs and `--produces` manifest entries to
  `~/.cache/weft/artifacts/<jobID>/<run>/outputs/...` on the host (copied
  over SSH when the control machine observes the terminal transition),
  before the working directory is reused, and records the pointer against
  the snapshot path. Still planned: a size cap and retention policy
  (configurable; oldest-first GC), since host cache disk is finite, with
  outputs above the cap recorded as pointer rows carrying an explicit
  `uncaptured` marker rather than silently; and capture of undeclared
  convention-directory outputs.
- **Replication (R2-enabled hosts and rentals)**: the existing per-run R2
  upload continues unchanged, as a second, off-host copy. Rentals must
  still have R2 — their disks vanish at termination, so for them R2 *is*
  the snapshot.
- **Retrieval ladder (control machine)** — implemented: local cache → R2
  per-run objects (any job kind, not just launch jobs) → on-demand host
  sync via SSH, which serves declared outputs from the host snapshot
  (pointer records dereference into it) and undeclared convention outputs
  from the host-live path. Every rung either succeeds, falls through, or
  reports its real error, and a full miss names each source checked. Still
  planned: an explicit bytes-may-have-been-overwritten warning when the
  host-live rung is the one that answers.
- **Staging ladder** (not yet implemented): host-side R2 pull when the host
  has credentials; host-to-host scp when producer and consumer are both
  on-prem and the producer snapshot exists; control-machine relay (with
  lease renewal during transfer and ranged resume) as the universal
  fallback.

This keeps the property that a host needs only SSH access and a Go binary
to participate, while making every capture path for declared outputs
produce a durable per-run snapshot somewhere.

## Component map

| Concern | Code |
|---|---|
| Manifest format, env vars, local store paths | `internal/artifacts/artifacts.go` |
| SSH manifest fetch + scp sync into cache | `internal/artifacts/sync.go` |
| Artifact DB rows, name/path/suffix lookup | `internal/db/artifacts.go` |
| CLI: sync/list/get/cat/add/prune-local | `cmd/artifact.go` |
| Shared list/get/cat resolver | `cmd/artifact.go` (`resolveJobArtifacts`, `deliverArtifactToken`) |
| Completion-time snapshot + pointer records | `internal/ops/artifacts.go` (`RecordJobOutputs`, `snapshotRemoteJobOutput`) |
| Cloud/inventory upload (outputs, manifest, barrier) | `cmd/agent/runinstance.go`, `cmd/agent/bgwork.go`, `cmd/agent/inventory_r2.go` |
| Run-ID → R2 key resolution policy | `internal/r2resolve/resolve.go` |
| R2 key layout | `internal/r2keys/` |
| Consumer `--needs` staging | `internal/ops/host_sync.go` |
| R2 client (idle-timeout downloads, range reads) | `internal/r2/client.go` |
