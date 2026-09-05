# Edge Command Parity

Status: **Proposal** — describes intended design, not current implementation.

One `weft` binary, one ledger. A host configured as an edge runs the same
commands an agent runs on the hub, and every one of them resolves to exactly one
of three outcomes. It never holds, creates, or reports from a database of its
own.

Related:

- [Edge Submission Protocol](../design/edge-submission-protocol.md) — the
  signed edge-to-hub channel this design builds on
- [0017](../decisions/0017-use-host-addressed-r2-mailboxes-for-isolated-inventory-hosts.md)
  — hub-to-host delivery over R2; the runner-state snapshot is the precedent for
  the hub view below
- [0026](../decisions/0026-isolate-edge-submissions-in-their-own-bucket.md)
  — why edge credentials reach no results
Three display-layer rules run through the design: sources fail independently
and degrade to stale-with-age; every displayed fact carries provenance; and an
empty result and an unknown result are different things.

## The problem

Today `role = "edge"` under `[edge]` is read by the `weft edge` family and by
nothing else. Every other command opens `~/.local/state/weft/jobs.db` through
`db.Open`, which creates the file and runs the schema if it is absent. On an
edge that is a second, empty ledger, and `weft jobs list` on it reports "no
jobs" with the same confidence it would on the hub. An agent cannot tell, from
the output, that it asked the wrong machine.

The requirement, stated once so the rest of the document can be checked
against it:

1. **No competing database.** An edge never opens or creates a local jobs or
   bugs database. There is one source of truth, and it is on the hub.
2. **Three outcomes only.** Every command on an edge either behaves as it
   does on the hub with the hub as its source; or reports that it is disabled
   on an edge; or reports that it is blocked right now because the hub cannot
   be reached from here.
3. **Callers are role-agnostic.** Agents, plans, skills, and documentation
   describe one set of commands with one set of semantics. The role shows up
   only at a blocked point, and there it is unmistakable.
4. **Empty is not unknown.** An edge never renders an unreachable or stale
   source as an empty result.

## Shape

Two one-directional channels over an object store, both already partly built,
and a classification that assigns every command to one of them or to neither.

```text
   hub ──── publishes ────►  hub view   ◄──── reads ──── edge   (mirror)
   hub ◄─── polls ───────── inbox       ◄──── writes ─── edge   (submit)
```

**Mirror** commands read the hub view. **Submit** commands write a signed
payload to the inbox and, where the hub's answer is needed for the command to
complete, wait for its acknowledgement. **Disabled** commands are refused by
role. **Local** commands touch no ledger and run anywhere.

The edge never connects to the hub. That is the security property of the
submission protocol, and this design keeps it: "the hub is not reachable" on an
edge always means the store is unreachable or the hub's most recent
publication is too old, never a failed connection to the hub itself.

## The hub view

The hub publishes a projection of its ledger to the store. The projection is
the hub's own versioned JSON — the same serializers behind `weft list --json`,
`weft info --json`, `weft host --json`, and `weft autopilot status --json` —
written as objects rather than to stdout. One serializer on both sides is what
makes parity a property rather than a goal: a mirror command on the edge
renders exactly the structure the hub would have rendered, from bytes the hub
produced.

```text
view/v1/manifest.json             published_at, hub host, schema version,
                                  deployment source digest, section stamps
view/v1/jobs/index.json           job rows, the `list --json` schema
view/v1/jobs/<wj>.json            job detail, the `info --json` schema
view/v1/jobs/<wj>/log.tail        bounded log tail, copied by the hub
view/v1/hosts.json                inventory and capability observations
view/v1/autopilot.json            autopilot status
view/v1/incidents.json            active placement incidents
view/v1/cost.json                 cost summaries
view/v1/bugs.json                 bug tracker rows
```

Every section carries its own `published_at` in the manifest. Sections fail
independently: a hub whose cost query errored still publishes jobs, and on the
edge `weft cost` is blocked while `weft jobs list` works.

**Publisher.** The daemon on the hub, which already owns sync and the autopilot,
publishes on its tick and on ledger change (the `dbwatch` source it already
has), and republishes the manifest on a heartbeat interval even when nothing
changed, so a quiet hub and a dead hub look different. The publisher is a hub
component. It runs in no other role.

**Log tails** are copied into the view by the hub, bounded in size, so an edge
reading a log never needs a credential on the results bucket. `weft log
--lines N` beyond the bound is blocked with a message naming the bound; `weft
log --follow` polls the tail with the view's freshness rule.

### Where the view lives

The view is hub-written and edge-read. Three placements are possible:

- **Its own bucket, with a read-only credential on the edge.** The edge holds
  write access to the inbox bucket and read access to the view bucket, and no
  access to results. Nothing an edge can write is anything an edge reads back.
- **The inbox bucket.** One bucket, one credential. But every edge can write
  there, so any edge can overwrite the view every edge reads. The submission
  protocol accepts this for acknowledgements because an ack confers nothing.
  The view is different in degree: an agent acts on it — marks work processed,
  resubmits, stops waiting — so a forged view steers autonomous behaviour even
  though it grants no capability.
- **The results bucket.** Ruled out by 0026.

**Recommendation: its own bucket.** The cost is one more bucket and one more
credential to provision per edge. The alternative saves that and reopens, for
status, the same "untrusted writer, trusted reader" shape the protocol document
names as the thing to never do twice. This is a boundary decision and is hard
to reverse once edges are provisioned, so it warrants a decision record when it
is made, alongside 0026.

### Freshness is the reachability signal

An edge cannot ask the hub whether it is alive. It can only read what the hub
last wrote and how long ago. So "the hub is reachable" is defined on the edge
as: the store answers, the manifest exists, and the section this command needs
was published within `[edge] view_stale_after` (default five minutes; the
publisher heartbeat must be well inside it).

| Observation | Outcome | What the user sees |
| --- | --- | --- |
| Store unreachable | blocked | `hub not reachable from this edge: view store unreachable: <error>` |
| No manifest | blocked | `hub not reachable from this edge: no hub view has been published` |
| Section older than bound | blocked | `hub not reachable from this edge: hub view for jobs is 47m old (bound 5m)` |
| Fresh section, zero rows | rendered | the same empty listing the hub would print |
| Fresh section | rendered | the hub's output, with a provenance line |

The fourth row is the one requirement 4 exists for. A fresh, empty index is a
true empty; a missing or stale index is unknown, and unknown is never drawn as
a table with no rows in it.

`--allow-stale` renders a stale section anyway, with its age on the first line
and in the JSON, for diagnosis. It is never the default and no skill uses it.

Every mirrored render ends with a provenance line, and the same fields appear
under `source` in JSON output:

```text
source: hub studio-hub via R2, published 12s ago
```

## Submission

Submit commands build the same request the hub would build locally, sign it,
and write it to the inbox under a payload kind. The hub's inbox poller admits it
through the protocol's verification and authorization steps and then executes
it **by calling the same internal function the local command calls**, with the
submitter's provenance attached. There is no second implementation of `run` or
`cancel` for edge work. The poller is a hub component and runs in no other
role.

Payload kinds, each registered on the hub and refused until it is:

| Kind | Commands | Notes |
| --- | --- | --- |
| `weft.job-submission/v1` | `run`, `launch` (project jobs), `plan submit` | Exists. Carries the working-tree closure through `internal/sync`. Not content-idempotent: two nonces are two jobs. |
| `weft.job-control/v1` | `cancel`, `kill`, `pause`, `resume`, `restart`, `mark-processed`, `mark-unprocessed`, `edit` | Names a job the hub already has. Idempotent per nonce. `restart` is bounded by the key's spend ceiling like a submission. |
| `weft.bug-report/v1` | `bug report`, `bug note` | States a fact; TTL-exempt per the protocol's per-kind policy. Runtime invariant detection in an edge binary uses this kind too. |
| `weft.plan-ended/v1` | `edge` internals | Exists. |

Authority remains where the protocol puts it: in the payload, checked against
hub configuration. A control request may act only on jobs the hub admitted from
the same plan's key, unless the hub's policy widens that; the default is
narrow.

### Identity across the gap

The hub assigns `wj` ids on admission; the edge holds a nonce until then. Two
rules keep this invisible in the common case:

- **Submit commands wait a bounded time for admission.** `weft run` on an edge
  blocks for up to the class's admission expectation (from `[edge] expect`,
  default sixty seconds) and, once the ack arrives, prints exactly what the hub
  prints: the job id. Only if no ack arrives inside the bound does it print the
  nonce, say that the id is assigned on admission, and name the resume command
  (`weft edge wait <nonce>`). A missing ack is unknown, never failure; the
  submission stands.
- **Every job-addressing command accepts a nonce.** `weft status
  01JBQ…` resolves the nonce through its ack object and continues as `weft
  status wj123`, or reports that the submission has not been admitted yet.

Refusals are surfaced to the submitter through the ack, naming the failed check
as the protocol requires: "signature valid; target `hub-laptop` is not an
allowed execution target", not "rejected".

## Classification

Every command in the tree is assigned one mode. The assignment is a table in
code, and a test walks the cobra tree and fails on any command without an
entry, so a new command cannot land unclassified and fall through to whatever
the default happens to be.

| Mode | Commands |
| --- | --- |
| **mirror** | `list`, `status`, `info`, `show`, `log`, `diagnose`, `project`, `incidents`, `cost`, `host` (show), `autopilot status`, `campaign list/show`, `instance list/status`, `session`, `source`, `artifact list/status`, `bug list/show`, `export`, `estimation` (read-only views), `blackboard` (already reads R2) |
| **submit** | `run`, `launch` (project jobs), `plan submit`, `cancel`, `kill`, `pause`, `resume`, `restart`, `mark-processed`, `mark-unprocessed`, `edit`, `bug report/note` |
| **disabled** | `autopilot run/pause/resume`, `daemon`, `place`, `start instance`, `instance launch/terminate`, `campaign launch/terminate`, `move`, `rebalance`, `replan`, `cordon`, `cleanup`, `db`, `host discover/setup`, `queue`, `provider`, `runpod`, `budget`, `retrain`, `r2` (low-level writes), `secret`, `slack`, `channel`, `sky`, `dashboard`, `narrate`, `new` |
| **local** | `help`, `completion`, `aliases`, `edge *`, `plan validate/show`, `artifact` operations that already go through R2 with the edge's own credentials |

The disabled set is the set of commands that exercise hub authority: placement,
instance lifecycle, host inventory, the daemon, and local-state administration.
Each entry carries a one-line reason, printed verbatim: `disabled on an edge:
placement and instance lifecycle are decided on the hub`.

Two commands need a note. `secret` is local by construction, but an edge's
local secrets never reach a job the hub runs, so on an edge it is disabled with
that reason rather than silently accepting values nothing will read. `dashboard`
is disabled now and could become a mirror later; it is a display layer, and
nothing prevents it from reading the view, but it is not in the first cut.

## Enforcement

Classification is policy. Two invariants make the policy hard to bypass.

- **`db.Open` refuses in the edge role.** It returns an error naming the
  role and the command, and never creates the file. A command that reaches the
  ledger on an edge through a path the table missed fails loudly with `edge
  role: no local job database; this command was not routed through the hub
  view`, which is a bug report, not an empty listing. The bugs database gets the
  same treatment.
- **The root command gates by mode before any `RunE`.** Disabled commands
  never start. Mirror and submit commands are handed a runtime that carries the
  view reader or the signer, and nothing else.

The runtime invariant is the backstop; the gate is the path. The order matters:
the gate fails with a message about the command, the invariant fails with a
message about the code.

## Signalling a blocked point

Requirement 3 says a caller learns the role only at a blocked point. So the two
blocked outcomes are distinguishable by machine and by eye, and they are the
only place the word "edge" appears in ordinary command output.

| Outcome | Exit code | First line of stderr | JSON |
| --- | --- | --- | --- |
| disabled on an edge | 20 | `disabled on an edge: <reason>` | `{"edge": {"outcome": "disabled", "reason": …}}` |
| hub not reachable | 21 | `hub not reachable from this edge: <cause>` | `{"edge": {"outcome": "blocked", "cause": …, "view_age_s": …}}` |

A plan can branch on the code. A skill can grep the first line. Neither needs
to know the role in advance, and neither is ever handed an empty table.

## Documentation

The command reference documents each command once, with its hub semantics,
and says nothing about roles. One section, "Running on an edge", carries the
disabled table above, the two blocked signals, and the provenance line. Skills
(`weft-submit`, `weft-manage`) keep the habits they teach, because the habits
are the same. `weft --help` in the edge role marks disabled commands inline so
an agent reading help on the edge sees the boundary without opening the docs.

## Configuration

```toml
[edge]
role = "edge"
submitter_host = "studio"
plan_id = "plan-42"
signing_key_dir = "…"
view_stale_after_minutes = 5

[edge.inbound]      # write: submissions          (exists)
bucket = "weft-edge-inbox"

[edge.view]         # read-only: the hub view      (new)
bucket = "weft-edge-view"
```

The hub side gains `[edge.view]` with a write credential and a publish
interval. A hub with no `[edge.view]` configured publishes nothing and says so
in `weft edge doctor`; an edge whose hub has never published sees the "no hub
view has been published" outcome, which is the correct first thing for a
half-configured deployment to say.

`weft edge doctor` on an edge gains a view check: manifest present, age, and
per-section ages. On a hub it reports the publisher's last successful publish
and any section that failed.

## Phasing

Each phase leaves the edge in a state that satisfies requirements 1, 2, and 4
on its own. Requirement 3 is met fully at phase 3.

1. **Gate and invariant.** Classification table, tree test, `db.Open` refusal,
   the two exit codes. Every mirror and submit command reports "hub not
   reachable: no hub view has been published" because there is none. Nothing
   on an edge can create a database or print an empty ledger after this phase.
2. **Hub view and mirror reads.** Publisher in the daemon; `list`, `status`,
   `info`, `log`, `host`, `autopilot status`, `incidents` read it. Parity is
   tested by rendering the same view bytes through the hub's local path and the
   edge's mirror path and diffing.
3. **Submission end to end.** The roadmap's three pieces: submit command,
   inbox poller, provenance on the job row. `weft run` reaches parity through
   the bounded admission wait; `weft edge wait` starts succeeding.
4. **Control and fact kinds.** `weft.job-control/v1` and
   `weft.bug-report/v1`; the corresponding commands move from blocked to
   submit.

## Tests

- Tree classification: every command has a mode; the test lists any that
  do not.
- `db.Open` in the edge role returns the refusal and leaves no file.
- Freshness: the five observation rows above, each asserted on outcome, exit
  code, and the absence of any rendered table in the blocked cases.
- Parity: hub output and mirrored output from identical view bytes are
  byte-equal above the provenance line, for each mirror command.
- Submission identity: a `run` whose ack arrives inside the bound prints the
  job id; one whose ack does not prints the nonce and the resume command; a
  status query by nonce resolves once the ack exists.
- Refusal surfacing: each distinct admission refusal reaches the submitter's
  terminal naming the failed check.

## Decisions this proposal implies

Two are hard to reverse and would be recorded when made, not before:

- **The view lives in its own bucket, read-only to edges.** Rejected
  alternative: the inbox bucket, saving a bucket and a credential at the cost of
  letting any edge overwrite what every edge reads.
- **An edge never opens a local ledger.** A deliberate absence. Rejected
  alternative: a read-through cache in a local SQLite file, which would make
  mirror reads faster and offline-tolerant, and would be a second database the
  moment anything wrote to it.

The projection schema is a spec, not a record: it accretes sections and belongs
in a spec file with a checker, per the rule that a schema without a checker
rots.

## Open questions

- **Scope of the view.** One view for all edges, or one per submitting host
  carrying only that host's jobs plus shared inventory. One view is simpler and
  matches the single-operator deployment this is built for; per-host views
  would matter if edges belonged to different people.
- **Control authority.** Whether a plan's key may cancel jobs it did not
  submit. The narrow default above is the safe one; widening it is hub policy.
- **View size.** The jobs index for a hub with thousands of historical rows.
  The index can carry a bounded recent window and let `list` with an explicit
  wider filter be blocked with a message, or the publisher can shard by
  project. Decide when the row count makes it a problem.
