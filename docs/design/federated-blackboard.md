# Federated R2 Blackboard

This note describes a coordinator-free placement path where cloud agents use R2
as a shared blackboard. It complements the existing autopilot design rather
than replacing it.

Related design notes:

- [Placement and Auto Mode](placement-and-auto-mode.md)
- [Job Move Protocol](job-move-protocol.md)
- [Placement Telemetry](placement-telemetry.md)
- [`specs/status-sync.allium`](../../specs/status-sync.allium)

## Goals

The first version lets cloud agents discover and reserve unplaced rental work
when autopilot is not actively driving placement. It must not require remote
agents to access the local SQLite database, and it must not introduce an
always-on coordinator service.

SQLite remains the source of truth. R2 carries exported job specs, claims,
assignments, agent heartbeats, and events. A claim is only a reservation; a
job may start only after a local Weft process reconciles the claim, creates the
authoritative DB attempt/run id, and sends the existing grace job payload.

This intentionally means v1 does not provide full laptop-off dispatch of newly
claimed jobs. It provides the R2 protocol, observability, and the foundation for
opportunistic reuse and orphan recovery whenever any local Weft process is
available to reconcile.

## R2 Layout

Blackboard objects live under a versioned control-plane prefix:

```text
blackboard/v1/state/autopilot.json
blackboard/v1/jobs/<jobID>/spec.json
blackboard/v1/jobs/<jobID>/claim.json
blackboard/v1/jobs/<jobID>/assignment.json
blackboard/v1/agents/<agentID>/heartbeat.json
blackboard/v1/events/<timestamp>-<id>.json
```

`spec.json` is written by local Weft processes for rental-eligible unplaced
jobs. It contains the command, directory, project, tags, resource constraints,
inputs, and a DB generation timestamp.

`claim.json` is written by a cloud agent using a conditional R2 PUT. The claim
names the agent, launch id, claim kind, creation time, renewal time, and expiry.
The object is the per-job contention boundary.

`assignment.json` is written only after local reconciliation succeeds or rejects
the claim. Accepted assignments contain the DB-created run id and launch id.
Rejected assignments contain a message.

`heartbeat.json` is optional for correctness and useful for observability. It
records agent id, launch id, phase, current jobs, and coarse free capacity.

`events/` is append-style diagnostic history. It should not be used for
correctness.

## Claim Protocol

Agents only claim when the exported autopilot state is absent, idle, or stale.
They must not claim when the exported state is `running` or `paused`.

Claims use R2 conditional writes:

```text
create claim: PUT claim.json with If-None-Match: *
renew claim:  PUT claim.json with If-Match: <current etag>
take expired: HEAD/GET claim.json, verify expires_at, then PUT with If-Match
```

R2 precondition failure is normal contention. The losing agent moves to another
candidate or backs off.

The local reconciler validates each claim before assignment:

- Autopilot is still inactive and unpaused.
- The job still exists, is unplaced, and is rental-eligible.
- There is no open move or placement intent for the job.
- The claiming launch can still accept work.
- Dependency and runaway-breaker rules still allow the job.

If validation passes, the reconciler calls the existing DB assignment path so
`job_attempts.id` remains the run id used by R2 completion markers. It then
writes `assignment.json` and submits the existing grace job request to the
claiming launch. The agent waits for assignment before running.

## Implementation Phases

1. R2 substrate and observability:
   - Add conditional PUT / ETag helpers to the R2 client.
   - Add blackboard key constructors and JSON schemas.
   - Add `weft blackboard status` to count specs, claims, assignments, agents,
     events, and expired claims.

2. Local publisher and reconciler:
   - Export rental-eligible unplaced jobs to `spec.json`.
   - Export autopilot state to `state/autopilot.json`.
   - Scan claims, validate them against SQLite, write assignments, and emit
     oplog plus blackboard events.

3. Cloud-agent reservation loop:
   - Publish agent heartbeats.
   - Poll specs between jobs or while in grace.
   - Locally filter candidates, claim one job with CAS, renew while waiting,
     and run only accepted assignments.

4. Orphan recovery:
   - Publish orphan-reclaim candidates for jobs whose latest launch/agent is
     stale or terminal.
   - Reuse the same claim and assignment protocol with
     `claim_kind=orphan_reclaim`.

## Independent Benefits

The early pieces are useful even before agents claim jobs:

- The R2 layout becomes documented and discoverable instead of incrementally
  implied by call sites.
- Conditional PUT support gives Weft a general object-level CAS primitive for
  future R2 control protocols.
- `weft blackboard status` gives operators a safe way to see pending claims,
  expired reservations, and stale heartbeats.
- Agent heartbeats and event objects improve diagnosis of cloud jobs that are
  alive but not making progress.
- The control-plane/data-plane key split becomes clearer as new R2 objects are
  added under `internal/controlplane` instead of ad hoc string literals.

## Non-goals

V1 does not make R2 the authoritative job database, does not let agents start a
job from a claim alone, and does not require an always-on coordinator.

On-prem agents are out of scope for v1. They can participate later if they get
R2 credentials and a local blackboard loop, but the first version is cloud-only
because cloud agents already depend on R2 and already use the grace protocol.
