# Campaigns

A **campaign** is a batch record grouping cloud instances launched together
in one `weft start instance` invocation. Most users don't need to think
about campaigns — they exist primarily as a unit of bulk operations
(watch, terminate, list) and to associate post-launch retries with the
original launch.

For the primary instance documentation — launching, monitoring,
configuration, grace periods, interruptible jobs, lifecycle — see
[Cloud GPU Instances](instances.md).

## What a campaign is

When you run `weft start instance` (or `weft instance launch`), weft groups
unplaced jobs by GPU constraints, finds offers, provisions one instance per
group, and records all of them under a single **campaign** row. The
campaign is the batch; the instances are the units of execution.

If a job is preempted or its instance fails, the relaunch is also
associated with the same campaign (and links to its predecessor attempt),
so the campaign captures the full history of the original launch.

For benchmark coverage campaigns that should avoid reusing the same Vast.ai
physical machine, launch with `weft start instance --distinct-machines`. See
[Distinct physical machines](instances.md#distinct-physical-machines) for
syntax and `--avoid`.

## Campaign-scoped commands

Most operations are per-instance (see [instances guide](instances.md#managing-instances)).
The following operate on a whole batch:

```bash
weft campaign list                      # List campaigns with instance counts
weft campaign show <campaign-id>        # Detailed view with instances and jobs
weft campaign watch <campaign-id>       # Watch only this batch (incl. relaunches)
weft campaign terminate <campaign-id>   # Terminate every instance in the batch
weft campaign cancel <campaign-id>      # Alias for terminate
weft campaign stats                     # Aggregate instance statistics
weft campaign survival                  # Survival model: posteriors and machine penalties
```

`weft campaign launch` is a deprecated alias for `weft start instance` and
remains for compatibility. New code, scripts, and skills should use
`weft start instance` (or `weft instance launch`).

## Project scope

`weft project launch` launches instances for unplaced jobs in the current
directory's project, and recorded campaigns can be filtered to that
project's jobs:

```bash
weft project launch --yes --watch       # Launch unplaced jobs for cwd's project
weft start instance --project myproj    # Explicit project filter
```

Project-scoped watch and listing similarly filter by project.

## See also

- [Cloud GPU Instances](instances.md) — the primary instance guide
- [Workflow Guide](workflow-guide.md) — end-to-end usage
- [Cloud Instance Debugging](cloud-instance-debugging.md) — investigating
  failed instances
