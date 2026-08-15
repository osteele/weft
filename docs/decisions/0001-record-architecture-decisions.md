---
status: accepted
date: 2026-08-15
---

# 1. Record architecture decisions

## Context and Problem Statement

Weft's design documentation records outcomes well and rationale unevenly. Three
patterns exist today:

1. **Full reasoning, embedded in a subsystem document.** `docs/design/artifacts.md`
   § "Decision: must R2 remain optional for on-prem hosts?" states the constraint
   under review, what changing it would buy and cost, an analysis, and an outcome.
   It is an ADR that happens to live inside a 341-line document about something else.
2. **Outcome plus a benefits list.** `docs/design/architecture.md` § "Design
   Decisions" gives three or four bullets for tmux, SQLite, and Bubble Tea, with
   no rejected alternatives and no consequences.
3. **Asserted with no rationale anywhere.** The retirement of the coordinator
   daemon is stated in five places and explained in none.

The third pattern is the costly one. A decision that survives only as a
deprecation banner cannot be evaluated later: a reader cannot tell whether the
forces that motivated it still hold, so it gets either cargo-culted or reversed
by accident. Pattern 2 degrades into pattern 3 as the benefits list ages out of
contact with the code.

A related problem is addressability. `docs/design/coordinator-architecture.md`
can only be deprecated by editing a banner into it, because its name is its only
identity. There is nothing short to cite in a commit message.

## Considered Options

- **Status quo** — keep recording decisions inline in design documents.
- **Numbered ADRs** ([Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions) /
  [adr-tools](https://github.com/npryce/adr-tools) / [MADR](https://adr.github.io/madr/)).
- **Date-prefixed decision notes** (`2026-06-05-retire-coordinator.md`).

## Decision Outcome

Record consequential architectural decisions as numbered ADRs in
`docs/decisions/`, following MADR's directory choice and adr-tools' filename
convention.

### Scheme

```
docs/decisions/NNNN-lowercase-hyphenated-decision.md
```

- **`NNNN`** — four-digit, zero-padded, monotonically increasing. The number is
  allocation order, not decision date; retroactive ADRs are normal and get the
  next free number.
- **Title** — states the position taken, not the question asked.
  `0004-keep-r2-optional-for-on-prem-hosts`, not `0004-should-r2-be-optional`,
  so that `ls docs/decisions/` reads as a list of positions.
- **Frontmatter** — `status`, `date` (when the ADR was written), `decision-date`
  (when the decision was actually taken, if different), and `supersedes` /
  `superseded-by` where applicable.
- **Status** — one of `proposed`, `accepted`, `deprecated`, `superseded`.

### Rules

- **Never rename, never renumber.** The filename is a permanent address. If the
  title turns out to be wrong, fix the `#` heading and leave the file alone.
- **Supersede, don't edit.** Reversing a decision means a new ADR with a new
  number and `supersedes: NNNN`. The only change to the old file is its `status`
  line and a `superseded-by` key. Records are history, not current-state docs.
- **Link, don't relocate.** Where a decision already has a good home inside a
  larger document, the ADR carries the decision and links out for detail rather
  than moving the document's content.

### What qualifies

An ADR is warranted when the decision is architectural (shapes a boundary,
protocol, or dependency), contested (a competent engineer could have chosen
otherwise), and durable. Ordinary implementation choices, roadmap items, and
proposals do not qualify — `docs/planning/` remains the home for prospective
work.

### Consequences

- Numbered filenames look foreign next to the repo's otherwise topical
  kebab-case naming (`facade-core.md`, `estimation-boundaries.md`). Accepted:
  the number buys stable identity, citability, and expressible supersession,
  which topical names cannot provide.
- Decisions become citable in commit messages and code comments the way
  `weft bug` IDs already are.
- The scheme is compatible with `adr-tools` (`adr new`, `adr new -s N`) if
  automation is ever wanted; the directory is just a config path.
- Two records of the same decision can drift — the ADR and the design document
  it links to. Mitigated by keeping the ADR short and pointing at the design
  document for mechanism.

Rejected: **date-prefixed notes**, because they sort by write date rather than
identity, are verbose to cite, duplicate a field that belongs in frontmatter,
and read badly for retroactive records where the write date and the decision
date are months or years apart.

## More Information

- [Michael Nygard, *Documenting Architecture Decisions*](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
- [adr-tools](https://github.com/npryce/adr-tools)
- [MADR](https://adr.github.io/madr/) (current release 4.0.0, 2024-09-17)
