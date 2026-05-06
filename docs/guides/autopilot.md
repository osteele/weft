# Autopilot

The **autopilot** is the engine that drives auto-placement and auto-launch
decisions. Any TUI with auto-mode enabled (`weft list`, `weft watch`, `weft
project watch`) takes a turn driving it; an unattended process can run it
directly with `weft autopilot run`. This guide covers the operator surfaces:
how to see what the autopilot is doing, how to interpret its blocked-state
diagnostics, and how to clear stuck state.

## Inspecting state

```bash
weft autopilot status              # idle | running | stale | paused
weft autopilot status --json       # machine-readable
weft autopilot status --quiet      # exit 0=idle, 10=running, 11=stale, 12=paused
```

**States.** `idle` means no pass is in flight and it's safe to run manual
commands without racing. `running` means a TUI or `autopilot run` loop is
currently driving a pass — wait or pause before issuing manual launches.
`stale` means a runner claimed the slot but its heartbeat has aged out
(usually a crashed TUI); the next runner will reclaim. `paused` is sticky
across restarts and means every runner is skipping its pass — see "Pause /
resume" below.

## Pause and resume

```bash
weft autopilot pause --reason "manual launch run"
# ... do work ...
weft autopilot resume
```

Pause is the right tool when you need to launch instances or restart jobs by
hand without the autopilot picking the same work in parallel. It is sticky:
restart and reboot do not clear it. Always remember to resume.

## Why is a job paused? — the runaway breaker

When the autopilot keeps placing jobs that fail without progress, it trips
the **runaway breaker** to stop spending. This includes repeated job-level
orphans and repeated infrastructure-side launch failures such as provider
status loss, bootstrap timeouts, and stale agent heartbeats. While the
breaker is tripped, any job in the affected scope reports

    blocked: paused: repeated launch failures without progress

Infrastructure-only trips may instead report `paused: repeated infrastructure
failures without progress`. The focused diagnostic is:

```bash
weft autopilot blocked
```

Output names every currently-tripped scope, when it tripped, and which
threshold caused the trip:

```
global (project=<all>)
  tripped 7h40m33s ago (2026-05-02T02:16:09+08:00)
  metrics: chain=2 orphaned=8 spend=$0.41 window=24h0m0s
  jobs (11): wj1238, wj1241, wj1656, wj1657, wj1662, wj1663, wj1664, wj1677, wj1678, wj1679, wj1680

Reset with: weft autopilot budget reset
Or rerun:   weft autopilot blocked --unblock
```

Reading the trip metrics:

| Field      | Meaning                                                        |
| ---------- | -------------------------------------------------------------- |
| `chain=N`  | Longest run of trailing-orphaned attempts in the window        |
| `orphaned=M` | Total orphaned attempts in the window                        |
| `spend=$X` | Dollars spent without any job completion in the window         |
| `window=…` | Policy window over which the metrics were computed             |

The metric whose threshold fired tells you the failure pattern. A high
`chain` with low `spend` is a tight loop of fast failures — usually a single
bad worker or a thin offer pool. A high `spend` with low `chain` is a
slow-burn issue: instances are coming up cleanly but never producing work,
e.g. user code stuck in a download loop. A very large `orphaned` is a wider
fleet symptom — provider outage, or a SKU-level survival problem.

`weft info wj<N>` also surfaces the trip directly inline:

```
Status:      queued
Blocked by:  runaway-breaker (global (project=<all>)), tripped 7h43m ago
             chain=2 orphaned=8 spend=$0.41 window=24h0m0s
             reset: weft autopilot blocked --unblock
```

so the operator who asks "why is wj1656 blocked?" gets the answer where
they already look.

JSON form for scripts:

```bash
weft autopilot blocked --json
```

returns `{"scopes": [{scope, campaign_id, project, tripped_at,
age_seconds, chain, orphaned, spend_cents, window, jobs}, …]}`.

## Resetting the breaker

After fixing the underlying cause (or accepting risk and proceeding
anyway), clear the trip so the autopilot resumes placement.

### CLI

```bash
weft autopilot budget reset
```

Clears the global runaway-breaker trip (campaign=`<none>`, project=`<all>`).
Source recorded as `CLI`. Equivalent to:

```bash
weft autopilot blocked --unblock
```

which lists the trips it's about to clear, then resets in the same
invocation.

Per-campaign trips are cleared by their campaign-scoped resume flow; the
budget reset only manages the global scope.

### TUI

In any TUI with auto-mode enabled (`weft list`, `weft watch`, `weft project
watch`):

1. Press `$` to open the run-rate / daily-cap prompt.
2. Press `h` or `d` to edit the hourly target or daily cap. `Enter` saves and
   returns to the main screen.
3. Press `H` or `D` to clear the hourly target or daily cap. `Enter` or `Esc`
   closes the confirmation.
4. Press `r` to reset the runaway breaker.

This calls `campaign.ResetGlobalRunawayBreaker(db, "TUI")` and re-triggers
an autopilot pass immediately. The footer hint reminds you of the binding:
`set run-rate + daily cap (H/D clear; r resets breaker)`.

## Running autopilot without a TUI

```bash
weft autopilot run                    # adaptive cooldown
weft autopilot run --interval 30s     # fixed cooldown between passes
weft autopilot run --once             # single pass and exit
weft autopilot run --once --json      # machine-readable per-pass output
```

`--once --json` is the right tool for cron jobs and ops scripts: each line
is one JSON pass record with `outcome`, `placed`, `rebalanced`,
`launched`, and `blocked_reasons`. `outcome` is one of `progress` (work
done), `idle` (nothing to do), `blocked` (work waiting but breaker
tripped), `error`, `paused`, `busy` (another runner holds the slot).

## Rebalancing queued work

The autopilot can move queued jobs between already-running cloud instances
when the move improves the projected queue drain score without exceeding the
cost ceiling. Configure the behavior in `.weft.toml`:

```toml
[autopilot]
rebalance_enabled = true
rebalance_cost_ceiling = 1.10
rebalance_strategy = "fast"          # cheap | balanced | fast
rebalance_score_epsilon = 0.01       # require 1% relative improvement
```

`fast` is the default and weighs time heavily while keeping some cost pressure.
`balanced` uses equal cost and time weights. `cheap` mostly prefers dollar
savings, but still considers queue drain time. `weft rebalance --strategy ...`
uses the same choices for manual dry-runs or `--yes` applies.

Manual scale-out uses the same hands-off coordination. `weft instance new`
launches one additional instance with a precomputed initial job list and opens
move intents for those jobs before launch, so autopilot and manual rebalance do
not also try to place them while the new instance is bootstrapping. The `n`
binding in `weft uj` uses the same orchestration path.

Jobs tagged `cpu-intensive` (deprecated alias: `compute-intensive`) are
conservative about cloud reuse and new rentals. Autopilot only routes them to
an existing rental when the instance has at least `WEFT_COMPUTE_CPU_CORES`
effective CPU cores, default `16`; unknown CPU metadata is treated as not
eligible. For new rentals, autopilot prefers an eligible on-prem host unless
the rental estimate completes at least 30 minutes sooner.

## Concurrency

The autopilot enforces a single-runner-at-a-time rule via the
`autopilot_state` row. A TUI claims the slot, drives one pass, releases.
External commands that walk through placement (`weft autopilot run`,
`weft autopilot blocked`) safely co-exist: the read-only commands don't
claim the slot, the writers do. If you start `weft autopilot run` while a
TUI is open, the writers serialise — no double-placement.

If you need an immediate halt (the autopilot is mid-pass and you want it
out of the way), wait for the active runner to finish (`weft autopilot
status` shows the heartbeat age) before issuing manual commands. Pause
does not interrupt a pass already in flight.

## Related

- See [Cloud GPU Instances](instances.md) for the launch flow the autopilot
  drives.
- See [Campaigns](campaigns.md) for the batching concept that scopes some
  trips.
- See [Debugging](debugging.md) for the broader troubleshooting context.
