---
status: accepted
date: 2026-08-26
---

# 0022. Attach capability behaviour to the namespace prefix

## Context and Problem Statement

Most host capability labels are inert: a job requires one, a host declares it,
and eligibility is exact string equality. One class is not. A required
capability under `agent:` also consumes an agent concurrency slot, admitted
through `AuthenticatedAgentCapacityAvailable` and bounded by the host's
`agent_concurrency` limits.

That behaviour is selected by matching the label's prefix, in
`internal/placement/target.go` and `internal/placement/placement.go`. The name
after the prefix is never interpreted.

A second class of behaviour will eventually be wanted — a tool that is
installed and usable on a host is the same kind of fact as an authenticated
agent CLI, but must not contend for slots meant for interactive agents. At that
point there are two ways to carry the behaviour, and the choice decides how
much weft has to know about the world.

## Decision Outcome

Behaviour attaches to the namespace prefix. A capability label's prefix
selects weft-side behaviour for the whole class; the name inside the namespace
stays opaque to weft.

Weft does not hold a per-capability table mapping labels to semantics, and must
not acquire one.

### Consequences

- Weft holds one fact per namespace instead of one fact per capability. It can
  admit `agent:kimi` without knowing what kimi is, and a new agent CLI needs no
  weft change at all.
- A namespace weft does not branch on is inert by construction. A new prefix
  therefore carries no behaviour until weft is changed to recognise it, which
  makes "what does this label do" answerable from the prefix alone.
- Behaviour is class-granular and cannot be varied per label. A capability that
  needs semantics differing from its namespace siblings has no home, and the
  answer is a new namespace rather than an exception.
- Every site that prefix-matches is part of the contract. Prefix matching is a
  protocol only while those sites remain enumerable; once the prefix is
  load-bearing in places nobody can list, this decision has decayed into drift.
- The prefix becomes part of the label's meaning, so renaming a namespace is a
  breaking change to every job that requires a label in it and every host that
  declares one.

## Considered Options

### A per-capability behaviour table

A map from capability label to its semantics — slot consumption, admission
rule, any future property — consulted at eligibility time.

Rejected because it requires weft to hold a fact about each capability. That is
domain knowledge about tools weft does not own and cannot version: weft would
need an entry for codex, for claude, for each tool added later, and each entry
would be a claim about something outside weft that nothing in weft can verify.
The same reasoning declines a probe hook that would let a capability report its
own version.

The table is also the shape a contributor reaches for when adding the second
behaviour, because it is the smaller local change. It is worth naming as
rejected for that reason: the mistake is not obviously a mistake at the moment
it is made.

### Explicit per-host configuration of slot semantics

Declaring, per host, which capabilities consume slots.

Rejected because it makes an operator responsible for a fact about weft's
internals, and allows two hosts to disagree about what the same label means.

## More Information

**Builds on** [0021](0021-keep-host-capability-observations-advisory.md), which
keeps observed capability evidence separate from declared capabilities.

**References** `CapabilityNamespaceProtocol` in
`specs/inventory-placement.allium`, which states what each namespace means today
and what a new behavioural namespace must specify before its branch is added.
That rule gains clauses as namespaces are added and so lives in the spec; this
record fixes the position it elaborates.
