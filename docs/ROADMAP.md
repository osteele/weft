# Roadmap

Planned and in-progress work. Completed work lives in the commit log, not here.

## Edge submission

The protocol core, key management, and diagnostics are in place. What remains is
everything that makes a submission actually become a job.

### End-to-end submission

**A signed submission from studio appears in the hub's job list, with the
submitting host and deployment source digest recorded on the job row.**

This is the acceptance criterion the feature exists to satisfy, and nothing
outside tests currently calls `edge.Submit` or `edge.Admit`. It needs three
pieces, and the first two should land together so the writer and its reader
arrive at the same time:

- **A submit command on the edge.** Tars the working tree through weft's
  existing source-closure machinery (`internal/sync`), uploads the payload,
  commits the pointer.
- **An inbox poller on the hub.** Reads `edge/v1/inbox/`, runs `edge.Admit`,
  creates the job row with its provenance, writes the acknowledgement. Until
  this exists `weft edge wait` cannot succeed, because nothing writes acks.
- **Provenance on the job row.** Submitting host and deployment source digest,
  stored where `weft info` can show them.

Two acceptance criteria are blocked behind this and cannot be checked before it:
that a retried submission with the same nonce creates no second job (proven at
package level, but "no second job" is not observable without job rows), and that
a submission refused for each distinct reason surfaces that reason to the
submitter.

### Verify the R2 transport against a real bucket

The filesystem transport is exercised on every test run. The R2 transport has
never run against a bucket, because the separate inbound bucket that decision
0026 requires does not exist yet. Provision it, then `weft edge doctor` against
it — that is the first real exercise of the R2 path, and no test can substitute
for it.

### Renewal loop

`Key.Project` is written and read by nothing. The protocol document marks the
project-ownership gate as specified-not-implemented for this reason. The loop
would renew a key's lease only while `research-site status --json` reports the
key's project still owned by the remote host and settled, skipping while a
handoff is in flight.

Until it exists, renewal is manual via `weft edge key renew`, and a plan's
authority lapses unless someone renews it.

## Artifact store hardening

### Remove standing write credentials from inventory hosts

An inventory host running `queue_transport = "r2_pull"` holds a standing
Object Read & Write credential on the results bucket, which it uses for three
things: queue command acknowledgements (`inventoryqueue.RequestKey`), runner
state snapshots (`inventoryqueue.StateKey`), and job result and artifact
upload.

That credential lets a compromised host rewrite any artifact in the results
bucket. Decision 0026 narrows what an *edge submission* credential can reach; it
does not touch this one, so the artifact store stays writable by a compromise of
any inventory host.

The replacement is presigned PUT URLs minted by the hub for exactly the keys the
host may write, delivered through the decision 0017 mailbox, which is already a
hub-to-host channel over R2. `PresignPutURL` exists and is used today for
instance probe and stage keys (`internal/campaign/lifecycle.go`). The host would
then need read access and nothing more.

Two things make this more than a credential swap, and both want thinking through
before it starts:

- **Silent failure modes.** A host that cannot acknowledge a queue command gets
  that command re-delivered, because mailbox delivery is at-least-once and the
  daemon checks acknowledgements before re-appending. A host that cannot publish
  a state snapshot reads as *status unknown* rather than as an error. A partial
  migration would present as an intermittently wedged host, not as a permission
  failure.
- **URL lifetime versus job duration.** A presigned URL expires; a job does not
  necessarily finish first. Result upload needs either a lifetime that bounds
  the longest job or a way to obtain a fresh URL mid-job.

This earns a decision record when it is designed, not before.

### Reconcile the two spend limits

`[edge] max_spend_usd` gates edge submission admission; `spending_limit`
("maximum cost per job in dollars") predates it. Two similarly-named knobs with
an undocumented division is a hazard on its own. When the poller lands, decide
whether the edge ceiling feeds the existing limit or the two are explicitly
distinguished, and say which in both config comments.

### Cumulative spend accounting

A plan's spend ceiling is enforced per submission with no running total, so a
plan holding a live key can spend up to its ceiling once per submission. Capping
a plan's total spend would need accounting the hub does not keep.

## Telemetry storage

### Columnar sidecar for telemetry, if the tables outgrow SQLite

The transactional tables and the telemetry tables have different shapes and are
currently in one file. Job, attempt, and launch state is small, is written by
several processes at once, and depends on 21 triggers and foreign keys to hold
its invariants. Telemetry is append-mostly and read by scans: `lifecycle_events`
413k rows, `host_contention_obs` 124k, `job_timeseries` 123k,
`job_telemetry_samples` and `job_telemetry_gpus` 48k each, in a 126 MB database.

Nothing needs doing yet. Measured on 2026-09-09, a full `GROUP BY event_kind`
over all of `lifecycle_events` takes 85 ms and a grouped min/max/count over the
same 413k rows takes 347 ms. Those are the queries a columnar store would
accelerate, and they are already fast enough to be uninteresting.

The trigger to act is telemetry reaching tens of millions of rows *and*
interactive aggregates becoming a bottleneck — both, not either. The move at
that point is a separate columnar store (DuckDB or Parquet files) holding
telemetry only, with the transactional state staying in SQLite. `bugs.db` is
the existing precedent for splitting a store by workload rather than by table
count.

Replacing SQLite outright is not the move and should not be reconsidered
without new information. DuckDB permits one read-write process and no mixing
with read-only attachments, which is incompatible with the CLI, TUI, daemon,
autopilot, and `weft web` all writing the same file; it has no triggers, so
every in-engine invariant would become application code holding under
concurrent writers; and its Go driver requires cgo, which would put a C
toolchain in the cross-compilation path that currently produces agent binaries.
DuckDB's `sqlite_scanner` already reads `jobs.db` in place for ad-hoc analysis,
which covers the exploratory case without changing anything.
