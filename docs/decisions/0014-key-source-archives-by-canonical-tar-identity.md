---
status: accepted
date: 2026-08-22
---

# 0014. Key source archives by canonical tar identity

## Context and Problem Statement

Source snapshots were keyed by the SHA-256 of their gzip representation. That
coupled the durable identity to a serial gzip encoder: changing the encoder or
its concurrency changed the key even when the extracted source tree was
identical. Source staging for a large working tree consequently spent most of
its local time on one CPU before it could discover whether R2 already held the
snapshot.

R2 is a byte-object store, not a content-addressable filesystem. Weft must
choose and persist the source identity while preserving access to source
objects named by clients that predate any new scheme.

## Decision Outcome

New source archives use the SHA-256 of the deterministic, uncompressed tar
stream as their identity and live at
`sources/v2/sha256/<canonical-tar-sha256>.tar.gz`. The gzip representation is a
standard gzip stream and may be produced in parallel without changing that
identity. Closure receipts use the corresponding v2 namespace and describe
each object's `identity_sha256`.

Compression and independent large-file hashing use an environment-bounded
worker count. The upper bound is half of `GOMAXPROCS`; Weft reduces it further
using the one-minute system load average and reserves one CPU for other work.
If load cannot be read, the half-budget cap still applies.

The v1 namespace, `sources/<gzip-sha256>.tar.gz`, remains immutable and
readable. Job attempts retain the exact R2 source key in their bootstrap or
manifest metadata, and all retrieval paths consume that key verbatim. Weft
does not reconstruct historical keys with the current key function. Raw
diverted source blobs remain at `assets/<raw-sha256>` and continue to use and
verify SHA-256 over their bytes.

### Consequences

- The first v2 submission of a tree already cached only under v1 uploads one
  new archive; later v2 submissions deduplicate by canonical tar identity.
- Old and new source objects coexist, and historical job attempts remain
  inspectable as long as their recorded objects are retained.
- The gzip library, compression settings, and worker count can change without
  changing a v2 source key.
- Source upload diagnostics report the worker limit and hashing duration.
- The one-minute load average is deliberately conservative and can lag a
  sudden load change.
- SHA-256 remains part of the source protocol even though a faster digest
  exists, avoiding a second compatibility migration before measurements show
  digest computation is material.

## Considered Options

### Keep hashing gzip bytes

Rejected: it makes compression representation part of source identity and
prevents parallel compression from remaining a transparent implementation
detail.

### Use a non-cryptographic hash

Rejected: a collision in a shared content-addressed store silently binds a job
to the wrong source. Compression does not provide collision resistance or an
independent integrity identity.

### Adopt BLAKE3 immediately

Rejected for now: BLAKE3 is fast and parallel, but gzip was the measured serial
bottleneck, SHA-256 is already hardware-accelerated on the target systems, and
the protocol and dependency change would add migration cost without measured
benefit.

### Rewrite or alias v1 objects

Rejected: historical attempts already carry exact v1 keys. Keeping those
objects and accepting both key shapes is simpler and safer than translating
old metadata or rewriting immutable history.

## More Information

- **References**: `docs/architecture/sync.md`;
  `specs/source-data-sync.allium`; `internal/sync/tarball.go`;
  `internal/dataplane/keys.go`
