# Allium specifications

These files are executable behavioral specifications for Weft's scheduling,
placement, synchronization, and lifecycle contracts. They target Allium
language version 3 and are validated with Allium CLI 3.5.3.

Install the validator and run the project check with:

```bash
cargo install allium-cli --version 3.5.3 --locked
just check-specs
```

`check-specs` fails on parser, type, reference, trigger, and other error-level
diagnostics. It also fails on any warning class except the three allowlisted
declaration-boundary categories (`definition.unused`, `entity.unused`, and
`externalEntity.missingSourceHint`). Those warnings identify intentionally
exported boundary entities and value types that are not consumed within the
same specification module; their counts remain visible in every check.

The migration from the repository's earlier draft dialect retains clauses that
Allium 3.5.3 cannot express as `-- legacy:` comments beside the nearest formal
rule or invariant. These comments are design guidance, not executable clauses.
New behavior should use supported Allium syntax and should not add new legacy
markers.

Use `allium check specs` when working directly with the CLI. Note that the
upstream command exits with status 1 for warnings as well as errors, so use the
project check in automation.
