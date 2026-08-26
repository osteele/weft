---
status: accepted
date: 2026-08-25
---

# 0021. Keep host capability observations advisory

## Context and Problem Statement

Host capabilities are operator declarations and hard placement constraints.
They are not evidence that the named service is installed, authenticated, or
usable. An undeclared but usable capability is invisible to placement; a
declared but unusable capability admits a dispatch that fails on arrival.

External tools can probe those services more accurately than Weft because they
own the meaning of their labels and know how to test authentication. Their
findings can also be stale, incomplete, or wrong. Treating a negative finding
as placement state would give every prober authority to remove a host from
rotation, including when the failure belongs to the observer or its network.

Weft needs to preserve findings with source and age while keeping operator
declarations and observer evidence distinguishable.

## Decision Outcome

Store host capability observations separately from declared capabilities. Each
observation records host, normalized label, observed boolean, source,
observation time, and opaque detail. The `(host, label, source)` identity keeps
independent observers separate and lets each source replace its own prior
finding.

Observations are advisory. They are reported to machine consumers but never
add, remove, or filter the declared capability set used by placement. Weft does
not probe capabilities itself, interpret observation detail, or infer a finding
from the absence of an observation.

### Consequences

- Consumers can compare placement declarations with timestamped, attributable
  findings without reading Weft's private database.
- A stale or broken observer cannot silently drain a host.
- Weft will knowingly dispatch to a host whose declared capability an observer
  has recorded as absent; avoiding that dispatch requires an operator to change
  the declaration or a consumer to choose not to submit.
- Consumers must decide how much to trust each source and how quickly an
  observation becomes stale.
- Declared configuration and observed evidence remain separate stores and can
  disagree indefinitely.
- Opaque detail remains useful for humans but cannot become a compatibility
  interface for automated decisions.
- Detail is open as a stage, not as an outcome. It is kept unstructured to
  reveal what recorders reach for, and is to be closed into a vocabulary once
  that is known, migrating existing rows. A single deployment makes that
  migration cheap, which is what makes the open field affordable rather than a
  debt; the 256-byte cap is what keeps it affordable, by bounding what anyone
  can come to rely on before it closes.

## Considered Options

### Make observations change eligibility

Rejected: negative findings would give a faulty observer terminal-in-effect
authority over host availability, while positive findings would bypass the
operator's declared admission boundary.

### Probe capabilities inside Weft

Rejected: capability labels do not define a generic usability protocol, and
the clients that own each label are better placed to test it.

### Store observations in host YAML

Rejected: host discovery rewrites those files wholesale, mixing ephemeral
evidence with operator configuration and discarding observations on discovery.

## More Information

- **References**: [Inventory and placement specification](../../specs/inventory-placement.allium)
