---
status: accepted
date: 2026-08-15
decision-date: 2026-06-11
---

# 4. Keep R2 optional for on-prem hosts

## Context and Problem Statement

Weft's artifact store allows on-prem hosts to participate with no Cloudflare R2
access at all. For those hosts, job outputs were recorded as *pointer records*
into the live working directory rather than captured anywhere.

That produced a family of durability and retrieval defects: no completion-time
capture on the no-R2 path, so outputs could be overwritten when the working
directory was reused; retrieval refusing to use the paths the no-R2 design did
provide; and transfer-layer errors surfacing as "not found".

The question raised was whether R2-optionality was itself the cause — whether
requiring R2 on every host would buy enough simplicity to justify dropping the
constraint.

## Considered Options

### Require R2 everywhere

What it buys:

- One capture path — every job, everywhere, uploads per-run keys at completion,
  and the pointer-record retrieval surface disappears.
- One retrieval ladder — cache → R2, unifying `get`, `list`, and `sync`.
- Staging becomes a host-side pull everywhere, deleting the control-machine
  relay and its lease machinery.

What it costs:

- **Credential distribution** — R2 write credentials on every on-prem host,
  including shared or institutionally managed machines where weft runs as an
  unprivileged user. A leaked host is a leaked bucket.
- **Restricted egress** — some on-prem hosts have limited or no outbound
  internet access (the constraint that motivates the HF mirror and offline
  workflows). For those, "require R2" means "weft cannot run", not "weft is
  simpler".
- **Cost and waste** — multi-GB outputs would round-trip through R2 even when
  producer and consumers share a host or LAN.
- **Failure coupling** — job completion would depend on R2 availability.

### Keep R2 optional, keep pointer-only capture

Preserves participation but leaves the durability defects unfixed.

### Keep R2 optional, add host-local completion-time snapshots

## Decision Outcome

**Keep R2 optional, drop pointer-only capture.**

The analysis that decided it: the open bugs are not caused by R2 being optional.
They are caused by the no-R2 path having *no completion-time capture at all*, by
retrieval refusing to use the paths that design provides, and by transfer-layer
defects independent of who holds credentials. Durability requires a
completion-time snapshot; it does not require that the snapshot live in R2. A
host-local snapshot uses nothing but the filesystem the job already wrote to and
provides the same overwrite immunity.

The revised baseline:

- **Capture (all hosts, no credentials needed)** — at successful completion,
  declared outputs and `--produces` manifest entries are snapshotted to
  `~/.cache/weft/artifacts/<jobID>/<run>/outputs/...` on the host, before the
  working directory can be reused.
- **Replication (R2-enabled hosts and rentals)** — the existing per-run R2
  upload continues unchanged as a second, off-host copy. Rentals must still have
  R2: their disks vanish at termination, so for them R2 *is* the snapshot.
- **Retrieval ladder** — local cache → R2 per-run objects → on-demand host sync
  over SSH. Every rung either succeeds, falls through, or reports its real
  error; a full miss names each source checked.
- **Staging ladder** — host-side R2 pull where credentials exist, host-to-host
  scp when both ends are on-prem, control-machine relay as universal fallback.

### Consequences

- The participation property is preserved: a host needs only SSH access and a Go
  binary to take part in the artifact store.
- Every capture path for declared outputs produces a durable per-run snapshot
  somewhere, which is what durability actually required.
- The cost is retained complexity that requiring R2 would have deleted: the
  multi-rung retrieval and staging ladders, and the control-machine relay with
  its lease renewal and ranged resume.
- Host cache disk is finite, so the snapshot store needs a size cap and
  retention policy. Outputs above the cap must be recorded as pointer rows
  carrying an explicit `uncaptured` marker rather than silently omitted.
- Capture of *undeclared* convention-directory outputs remains unaddressed by
  this decision.

## Implementation status

Capture and the retrieval ladder are implemented. Still outstanding at the time
of writing: the snapshot size cap and retention policy, the `uncaptured` marker,
capture of undeclared convention outputs, the staging ladder, and an explicit
warning when the host-live retrieval rung is the one that answers.
[`docs/design/artifacts.md`](../design/artifacts.md) is the current record.

## More Information

- [`docs/design/artifacts.md`](../design/artifacts.md) — full artifact store
  design, invariants, component map, and the source of this decision.
- `specs/job-lifecycle.allium` — normative rules for the cloud upload side.
- [`docs/guides/workflow-guide.md`](../guides/workflow-guide.md) — user-facing
  behavior of `--produces`, `--needs`, and output declaration.
