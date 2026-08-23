---
status: accepted
date: 2026-08-22
---

# 0016. Run inventory queue jobs from the submitted source closure

## Context and Problem Statement

Submission records an immutable, content-addressed source closure before the
job becomes visible to placement. Cloud execution consumes that closure, but
inventory queue dispatch historically re-read and rsynced the submitter's live
working tree. A file changed while a job waited for placement could therefore
make the executed bytes differ from the receipt's source pin. The inventory
worker's marker proved only that it received the later dispatch-time tree.

Source closures may contain several adjacent roots and large files diverted
into separate blob objects. Selecting only the first archive is not a valid
substitute for consuming the closure.

## Decision Outcome

Queue-runner inventory jobs with submit-time source pins execute from the
recorded source closure. Their queue payload carries the ordered roots,
canonical-tar identities, exact R2 keys, and blob references. The agent
validates the manifest and archive identities, materializes all roots and
blobs under a per-job parent, and runs from the first root.

The live working tree is not consulted after a pinned job is recorded. Rows
that predate submit-time pinning retain the rsync and marker path. An older
runner that does not understand the manifest receives a closure-receipt key in
the legacy tarball field, causing extraction to fail rather than silently
running an incomplete source tree.

### Consequences

- A receipt's source pin identifies the bytes an inventory queue runner uses.
- Source changes after submission do not affect already-recorded queue jobs.
- Inventory startup now depends on R2 reads for pinned sources, although
  content-addressed archives and blobs are cached on the host.
- Multi-root dependencies and diverted large files remain part of the
  execution identity.
- Historical unpinned rows remain runnable but cannot acquire a guarantee
  that did not exist when they were recorded.
- Slurm dispatch retains its existing compatibility path until it has an
  equivalent immutable materializer.

## Considered Options

### Revalidate and rsync the live tree

Rejected: matching before and after rsync narrows the race but does not make
the transferred bytes derive from the submitted object, and it repeats source
hashing at dispatch.

### Send only the first pinned archive

Rejected: it omits sibling roots and diverted blobs, so some jobs would run an
incomplete source closure without an integrity error.

### Keep the source pin as cloud-only provenance

Rejected: one receipt field with target-dependent execution semantics invites
false integrity conclusions and makes delayed inventory placement
nondeterministic.

## More Information

- **Builds on**: [0014](0014-key-source-archives-by-canonical-tar-identity.md)
- **References**: `docs/architecture/sync.md`;
  `specs/source-data-sync.allium`; `internal/ops/host_sync.go`;
  `internal/runner/runner.go`
