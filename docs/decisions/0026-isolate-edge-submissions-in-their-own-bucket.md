---
status: accepted
date: 2026-09-04
---

# 0026. Isolate edge submissions in their own bucket

## Context and Problem Statement

An edge host submits work to the hub by writing objects to an R2 store, because
it has no inbound route to the hub and must not acquire one. The submission
path therefore requires write credentials on the edge.

Studio already holds write credentials for the results bucket, which it uses to
push artifacts. Reusing that bucket for inbound submissions would mean one
credential covering both, so a compromised edge could rewrite completed
artifacts — results attributed to jobs that did not produce them, in a store the
hub treats as authoritative.

The natural smaller change is to keep one bucket and separate the two uses by
key prefix, granting the edge a token scoped to the submission prefix.

## Decision Outcome

Inbound edge submissions live in a second R2 bucket with its own API token,
separate from the results bucket. An edge holds write credentials for the
submission bucket and none for results.

Prefix separation does not work, because R2 API tokens are scoped per bucket
rather than per prefix. There is no token that grants write access to
`edge/v1/` while withholding it from the rest of the bucket, so a prefix split
would be a naming convention rather than a boundary — and one that reads like a
boundary to anyone who did not check.

The bucket is the only isolation unit R2 actually offers, so it is the one used.

### Consequences

- A second bucket to provision, and a second credential to distribute, rotate,
  and revoke. Edge deployment gains a step that cannot be skipped.
- Configuration, status surfaces, and any store-health check must now cover two
  buckets and report them separately; a single "R2 is fine" answer is no longer
  meaningful.
- The two stores fail independently, which is the point, but means an edge can
  be able to submit while unable to read results, or the reverse. Callers must
  not infer one store's health from the other's.
- A compromised edge cannot use its *submission* credential to touch results.
  That asymmetry is what the second bucket buys, and it is narrower than
  "a compromised edge cannot touch results": a host that is also an inventory
  host holds a separate standing write credential on the results bucket for
  queue acknowledgements, state snapshots, and artifact upload. This decision
  does not remove that credential, so on such a host the artifact store remains
  writable by a compromise. Closing that requires migrating those writes to
  presigned URLs minted by the hub, which is separate work.

## Considered Options

### One bucket, separated by key prefix

Rejected: R2 API tokens are bucket-scoped, not prefix-scoped, so the edge's
token would necessarily grant write access to results as well. This is the
smaller change and the one to reach for by default, which is exactly why the
reason it fails is worth recording — the split would look like isolation while
providing none.

### Reuse the results bucket without separation

Rejected: it makes artifact integrity depend on every edge host staying
uncompromised, which is the assumption the signing design already declines to
make.

## More Information

- **Builds on**: [0004](0004-keep-r2-optional-for-on-prem-hosts.md),
  [0017](0017-use-host-addressed-r2-mailboxes-for-isolated-inventory-hosts.md)
- **References**: `docs/design/edge-submission-protocol.md`; `internal/edge/transport_r2.go`
