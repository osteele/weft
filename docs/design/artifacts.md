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
| **Local artifact cache** | `~/.config/weft/artifacts/<jobID>/...` on the control machine + `artifacts` DB table (path, stored path, size, sha256, attempt id) | Bytes copied from a host or downloaded from R2 | Durable; survives workdir reuse |
| **R2 per-run objects** | `jobs/<id>/runs/<run>/outputs/...` and `jobs/<id>/runs/<run>/artifacts/files/...` (+ `artifacts/manifest.json`) | Bytes uploaded by an agent during/after the run | Durable; keyed per run, immune to overwrite by later jobs |
| **Pointer records** | `host_data` rows of kind `job-output`, asset id `<jobID>/<relPath>` (`internal/ops/artifacts.go RecordJobOutputs`) | Host + relative path only — no bytes, no size, no hash | **Not durable** — dereferences into the live working directory, which the next job may overwrite |
| **Host-live files** | The job's working directory on the host | Whatever is currently on disk | None — shared mutable state across jobs in the same directory |

Pointer records exist primarily for **placement** (data-locality scoring and
pre-staging cost estimation), but `artifact list` also surfaces them as if
they were retrievable artifacts. That dual use is the source of the
list/get mismatch class of bugs (see "Divergences" below).

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
  upload output/ +         same as cloud            write manifest +
  manifest entries to      (uploadOutputDirs,       pointer record only;
  per-run R2 keys;         manifest entries,        bytes stay in the
  same-workdir barrier     workdir barrier)         shared working dir
  before next job                │                         │
        │                        │                   capture deferred to
   durable per run          durable per run         a later lazy sync
                                                    (rsync on demand)
```

- **Cloud rentals and R2-enabled on-prem runners** snapshot at completion:
  `uploadOutputDirs` walks the convention directories and
  `uploadArtifactManifestEntries` uploads declared paths, both under per-run
  R2 keys (`cmd/agent/runinstance.go`, wired for on-prem via
  `cmd/agent/inventory_r2.go` when `run-queue` is started with
  `--r2-bucket`). A per-workdir barrier
  (`SharedWorkdirUploadBarrierBeforeNextJob`) prevents the next job in the
  same directory from starting while the prior job's upload walk is still
  running.
- **On-prem runners without R2** perform no completion-time capture at all.
  Completion writes the manifest, the completion record (with discovered
  output file list), and a pointer record. Bytes are copied only when the
  control machine later runs `weft artifact sync` (SSH manifest fetch + scp,
  `internal/artifacts/sync.go`) or the convention-output rsync-back
  (`cmd/artifact.go syncJobOutputs`).

Lazy capture is a race: the window between completion and the first sync is
unbounded, and any later job using the same working directory and the same
relative output path closes it destructively (bug wb10).

## Retrieval: list, get, cat, sync

`weft artifact list <job>` prints the union of three sources:

1. cached artifacts (`artifacts` table),
2. pointer records (`host_data` job-output rows), and
3. for launch (cloud) jobs only, a live listing of the job's per-run R2
   prefixes.

`weft artifact get` / `cat` resolve in this order:

1. the local artifact cache (`db.FindArtifactByNameOrPath`, scoped to the
   latest run first, then job-wide);
2. the job's R2 per-run objects (any target kind — inventory runners with
   `--r2-bucket` upload to the same prefixes as rentals), matched by exact
   path, basename, or path suffix, downloaded with a per-transfer idle
   timeout (`--timeout`, default 2m of no progress) and a progress status
   line for large files;
3. for jobs on inventory hosts, an on-demand sync (`syncArtifactOnDemand`)
   that runs the same manifest-fetch/rsync path as `weft artifact sync` and
   then serves from the cache.

When every rung misses, the error names each source checked. The remaining
gap relative to the target ladder is the host-local per-run snapshot
(rung 3 currently reads the live working directory, so it inherits the
wb10 overwrite race until completion-time snapshots land).

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

The `cmd/artifact.go` retrieval paths do not use this package; they
reimplement resolution as `[latest_run_id, 0]` only (`jobAttemptRunIDs`),
in four call sites. Artifacts from superseded runs are therefore invisible
to `artifact list`/`get` even though staging would find them.

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

## Known divergences (as of 2026-06-11)

| Invariant | Divergence | Tracker |
|---|---|---|
| Per-job durability | No completion-time capture on no-R2 on-prem hosts; lazy sync races with workdir reuse. Fix decided below (host-local snapshot), not yet implemented | wb10 |
| Attribution | The shared-workdir barrier serializes only sequential jobs within one agent process; concurrent same-workdir jobs (multi-GPU hosts) can still cross-attribute uploads | (spec guidance, wj2226 incident) |
| — | Declared artifacts under `output/` upload twice (outputs prefix + artifact-files prefix). Listing and `get --all` now collapse the two spellings, but the agent still uploads the payload twice | wb20 (upload side) |

Resolved 2026-06-11 (see the corresponding rules in
`specs/job-lifecycle.allium`): retrievability (`get`/`cat`/`--all` now fall
back to R2 for any job kind and to an on-demand host sync, and name every
source checked on a miss — `ArtifactListGetConsistency`); error fidelity
(transfer failures propagate their real cause instead of "not found", and
large single-file downloads show progress —
`ArtifactRetrievalErrorFidelity`); run completeness (retrieval delegates to
`r2resolve`, finding superseded-run uploads —
`ArtifactRetrievalCoversAllRuns`).

## Decision: must R2 remain optional for on-prem hosts?

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
stays, but it stops meaning "no capture". The revised baseline:

- **Capture (all hosts, no credentials needed)**: at successful completion,
  the queue runner snapshots declared outputs and manifest entries to
  `~/.cache/weft/artifacts/<jobID>/<run>/...` on the host, before the
  working directory can be reused. Snapshots are subject to a size cap and
  retention policy (configurable; oldest-first GC), since host cache disk
  is finite. Convention-directory outputs above the cap are recorded as
  pointer rows with an explicit `uncaptured` marker rather than silently.
- **Replication (R2-enabled hosts and rentals)**: the existing per-run R2
  upload continues unchanged, as a second, off-host copy. Rentals must
  still have R2 — their disks vanish at termination, so for them R2 *is*
  the snapshot.
- **Retrieval ladder (control machine)**: local cache → R2 per-run objects
  (any job kind, not just launch jobs) → host snapshot via SSH →
  host-live path as a last resort, with a warning that the bytes may have
  been overwritten. Every rung either succeeds, falls through, or reports
  its real error.
- **Staging ladder**: host-side R2 pull when the host has credentials;
  host-to-host scp when producer and consumer are both on-prem and the
  producer snapshot exists; control-machine relay (with lease renewal
  during transfer and ranged resume) as the universal fallback.

This keeps the property that a host needs only SSH access and a Go binary
to participate, while making every capture path produce a durable per-run
snapshot somewhere.

## Component map

| Concern | Code |
|---|---|
| Manifest format, env vars, local store paths | `internal/artifacts/artifacts.go` |
| SSH manifest fetch + scp sync into cache | `internal/artifacts/sync.go` |
| Artifact DB rows, name/path/suffix lookup | `internal/db/artifacts.go` |
| CLI: sync/list/get/cat/add/prune-local | `cmd/artifact.go` |
| Pointer records at completion | `internal/ops/artifacts.go` |
| Cloud/inventory upload (outputs, manifest, barrier) | `cmd/agent/runinstance.go`, `cmd/agent/bgwork.go`, `cmd/agent/inventory_r2.go` |
| Run-ID → R2 key resolution policy | `internal/r2resolve/resolve.go` |
| R2 key layout | `internal/r2keys/` |
| Consumer `--needs` staging | `internal/ops/host_sync.go` |
| R2 client (idle-timeout downloads, range reads) | `internal/r2/client.go` |
