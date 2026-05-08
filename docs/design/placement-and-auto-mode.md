# Placement and Auto Mode

This note records the design boundary for placement and autopilot. Operator
instructions belong in the guides:

- [Placement guide](../guides/placement.md): how automatic placement engages,
  how `--gpu`, `--input`, and reserved tags steer it, and how to read score
  reasons.
- [Autopilot guide](../guides/autopilot.md): how to inspect, pause, unblock,
  and run the automation loop.

The formal inventory placement model lives in
[`specs/inventory-placement.allium`](../../specs/inventory-placement.allium).
This design note should stay focused on rationale and ownership boundaries.

## Boundary

Placement is the decision primitive: given job constraints and current resource
state, choose an inventory host, reuse an existing rental instance, launch a new
rental instance, or leave the job unplaced with reasons.

Autopilot is the automation loop around that primitive. It repeatedly asks the
planner for work that can be placed or launched, then applies those decisions
without an operator pressing keys in the TUI. It is intentionally not the
source of truth for eligibility or scoring rules.

## Why Inventory Comes First

Inventory hosts are evaluated before rental capacity because they are already
paid for, have stable local queues, and usually have better data locality. This
default can still spill to rentals when no inventory host matches or when the
rental path is materially better under the active strategy.

`rental` and `inventory` are explicit routing overrides. They exist so users
can make cost-domain decisions that the scorer should not reinterpret:

- `rental` skips local placement and moves the job directly into rental reuse
  or launch workflows.
- `inventory` keeps the job on known hosts and forbids rental launch.

Provider tags such as `provider:vastai` are rental-domain constraints, so they
also bypass inventory placement.

## Why Autopilot Is Single-Runner

Auto mode can be driven from more than one surface: grouped job lists, watch
TUIs, and `weft autopilot run`. Those surfaces must not simultaneously assign
the same unplaced jobs or launch duplicate instances.

The current design uses a singleton `autopilot_state` row as the coarse
coordination point. A runner claims the row, heartbeats while a pass is active,
then releases it. Pause is stored on the same row so every runner observes the
same sticky external stop signal.

Headless `weft autopilot run` is DB-invalidation driven: after a pass it waits
for the shared SQLite DB/WAL/SHM change source before planning again. Its
adaptive or fixed interval is a maximum wait and triggers cloud sync; if sync
finds no local updates, the runner keeps waiting rather than replanning
unchanged jobs.

Cloud sync itself is guarded by the shared `sync:cloud:reconcile` provider
lease in the sync orchestrator, so TUIs, monitor paths, and headless autopilot
do not all query cloud providers at once. A process that loses the lease still
runs the DB-local reconciliation fallback and observes provider changes through
the next database notification from the process that won.

Older per-scope leases remain useful for UI feedback and local contention, but
the singleton row is the correctness boundary: only one autopilot pass should
be in flight across Weft processes.

## Why Placement Reasons Are User-Facing

Placement rejection and scoring reasons are not only debug traces. They are how
the operator learns whether a job is waiting because of a hard constraint
(`no GPU with >=24GB`), an intentional routing choice (`rental-tagged job skips
local placement`), or a soft ranking factor (`2 jobs queued (~60m drain)`).

For that reason, reason strings should stay stable enough for users to
recognize, while the Allium spec owns the behavior behind them.

## Implementation Map

- Inventory scoring: `internal/placement/placement.go`
- GPU generation matching: `internal/placement/gpugen.go`
- Rental strategy planning and reuse scoring: `internal/campaign/strategy_plan.go`
- Autopilot runner state: `internal/orchestration/autopilot_runner.go`
- Autopilot persistence: `internal/db/autopilot_state.go`
- Job-move / placement-intent coordination:
  [`specs/job-move.allium`](../../specs/job-move.allium)
