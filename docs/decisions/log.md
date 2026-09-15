# Decision log

One-line entries for decisions that foreclosed something without being hard to
reverse. Newest last. Entries are written once and left alone; a change of
position is a later entry or a full record.

Form: *In the context of X, facing Y, we decided for Z, and neglected W, to
achieve Q, accepting D.*

- **2026-09-04** — In the context of the edge submission frame, facing the
  choice of whether to carry an algorithm identifier, we decided to fix Ed25519
  with no `alg` field anywhere, and neglected algorithm agility, to remove
  downgrade and algorithm-confusion attacks entirely, accepting that changing
  algorithm later requires a protocol version bump and a coordinated rollout of
  both ends.

- **2026-09-04** — In the context of edge submission idempotency, facing the
  risk that a retry loop submits the same paid GPU rental several times, we
  decided to make one ULID serve as nonce, object key, and idempotency key at
  once, and neglected a separate submission-id compared after the fact, to make
  a duplicate submission impossible by construction rather than by a check
  someone must remember to write, accepting that the object key is then opaque
  and an operator cannot read a submission's contents from its name.

- **2026-09-04** — In the context of edge submission authority, facing the
  choice between a plan-length key retired on completion and a short renewed
  lease, we decided on a short window slid forward while the hub observes the
  plan running, and neglected a `retired_at` field, to make expiry rather than
  a live credential the default outcome when a plan is abandoned or a session
  crashes, accepting that the hub's availability becomes a liveness dependency
  of every executing plan.

- **2026-09-09** — In the context of preventing jobs from hanging before their
  user command starts, facing whether to reuse the rental-lifetime
  `--max-time`, we decided on a distinct per-job `--wall-time` that applies to
  inventory and rental execution, and neglected overloading the instance
  lifetime policy, to bound setup and command time with one portable contract,
  accepting another duration field in submission metadata and runner wire
  formats.

- **2026-09-09** — In the context of inventory agent fleet visibility, facing
  the choice between live SSH fan-out and durable local observations, we
  decided for independently timestamped deployment and runner records with a
  freshness-bounded cached status command, and neglected network probes on the
  read path, to make fleet inspection fast and available while hosts are
  offline, accepting eventual consistency and explicit `unknown` or
  `stale-observation` states.

- **2026-09-10** — In the context of a CLI whose expected users are agents,
  facing timestamps that depend on the observer's workstation, we decided for
  UTC by default with explicit timezone markers and an opt-in local or named
  timezone, and neglected local time as the CLI default, to make times
  comparable across machines, accepting different default clock displays
  between the CLI and the local-time TUI.

- **2026-09-15** — In the context of the autopilot's blocker-recheck policy,
  facing an unrecognized dispatch blocker that the deliberately one-sided
  string fallback maps to the hottest cadence, we decided to let an unchanged
  blocker's retry interval decay with the age of its failure run and to stop
  counting undispatchable jobs as work in flight, and neglected typing this
  blocker as `precondition` so it would map to `recheck_none`, to bound the
  cost of every condition nobody has classified rather than one condition at a
  time, accepting up to ten minutes' added latency when a blocker does clear on
  its own.
