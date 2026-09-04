---
status: accepted
date: 2026-09-04
---

# 0027. A signature authenticates the writer and never authorizes the work

## Context and Problem Statement

An edge submission is signed, and the signature covers the payload by digest.
The payload carries an execution target and a spend ceiling. Those fields are
therefore non-forgeable by anyone who cannot sign.

This makes a simplification look obviously correct: the constraints are inside
the signature, so a hub that has verified the signature could act on them
directly and skip its own allowlist and spend checks. The checks look
redundant — the same values, verified twice.

They are not redundant, and nothing in the code shows why. A maintainer reading
`Authorize` sees a function re-validating fields that a cryptographic check has
already protected, and removing it makes every test pass.

## Decision Outcome

Authorization is a separate step from verification, runs on verified content,
and is never skipped or short-circuited because a signature was valid.

The signature answers *who wrote this*. It cannot answer *may this run*,
because the threat it does not cover is a compromised edge: such an attacker
signs whatever it likes with a key the hub accepts, and every field in the
envelope and payload becomes an attacker's choice carrying a perfectly valid
signature. The allowlist, the spend ceiling, and the payload digest check are
what remain in that case. They are the entire remaining defense, which is why
they must not be folded into the step that would already have succeeded.

Concretely: the authoritative spend ceiling is granted hub-side, attached to
the key at mint time, and never transmitted; a submission may carry a
*downward-only* request, which the hub honors when it is smaller than the grant
and refuses when it is larger, rather than clamping it. Remaining authority
fields live in the payload rather than the shared envelope; the execution-target
allowlist is hub configuration that no submission can extend; and the hub's own
host can never be a target.

The safety comes from the hub-side comparison, not from the field's absence:
the edge does transmit a requested ceiling. What the grant's placement buys is
that the *authoritative* value has no representation the submitter controls, so
the comparison always has something trustworthy on one side of it.

An unset grant permits nothing rather than everything. A hub with no configured
maximum refuses spending outright, because the alternative — treating "unset" as
"unlimited" — would make the one check standing between an autonomous session
and real money fail open on a fresh install.

Refusal messages must name which of the two failed. "Signature valid; target
`laptop` is not an allowed execution target" is actionable. "Rejected" sends an
operator to look at keys.

### Consequences

- Two checks that read the same values remain in the code permanently, and will
  keep looking removable. This record is the reason they are not.
- Hub configuration must be maintained separately from what edges request, and
  the two drift: an edge can be refused for asking for something it was
  previously granted.
- A submission can be refused despite a valid signature, which is confusing
  unless the message distinguishes the cases — hence the wording requirement,
  which is a maintenance burden on every new refusal path.
- Adding an authority field to any payload adds a hub-side check. There is no
  path where signing alone is sufficient.

## Considered Options

### Trust the signed constraints and skip the hub-side checks

Rejected: it is correct only against an attacker who cannot sign, and the
design's stated threat model includes a compromised edge that can. Under that
threat the signature verifies and every constraint is the attacker's. This is
the credible option — it is simpler, it is what the fields' non-forgeability
suggests, and it fails only in the case the mechanism exists for.

### Clamp an over-ceiling submission to the hub maximum instead of refusing

Rejected: it runs the work under a ceiling the submitter did not choose,
proceeding on a guess about intent, and it can spend the full cap before
failing. Refusing fails immediately and diagnosably, which matters more when no
human is watching. An edge that does not know the hub's configuration sends no
ceiling and receives the hub's.

## More Information

- **Builds on**: [0026](0026-isolate-edge-submissions-in-their-own-bucket.md)
- **References**: `docs/design/edge-submission-protocol.md`;
  `internal/edge/authorize.go`; `internal/edge/verify.go`
