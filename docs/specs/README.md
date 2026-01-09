# TLA+ and PlusCal Specs

This directory contains formal specs that model CLI/TUI behavior and state
transitions. Specs are written in PlusCal inside `.tla` modules and translated
via `pcal.trans` before model checking.

## Running TLC

From the repo root:

```
just tla-check
```

`just tla-check` runs the helper script `internal/scripts/run_tla_checks.sh`,
which:

- regenerates any `.cfg` files from matching `.cfg.script` helpers
- translates PlusCal algorithms (when present)
- runs TLC for each `.tla` that has a matching `.cfg`

If a tracked `.cfg` changes after regeneration, the script exits with a
non-zero status so you can commit or ignore the updated file.

## Running Apalache

From the repo root:

```
just tla-apalache
```

`just tla-apalache` runs `internal/scripts/run_tla_apalache.sh` to translate
PlusCal specs and then invoke Apalache using constants/invariants extracted
from each spec's `.cfg` file.

## Environment Variables

Both scripts accept the following overrides:

- `JAVA_BIN`: path to the Java executable
- `TLA_JAR`: path to `tla2tools.jar`

Example:

```
JAVA_BIN=/opt/homebrew/Cellar/openjdk/25.0.1/bin/java \
TLA_JAR=~/lib/tla2tools.jar \
just tla-check
```

## Spec Files

Each spec has the following optional companions:

- `<Spec>.cfg.script`: a small shell script that emits the TLC config
- `<Spec>.cfg`: generated config (may be ignored in `.gitignore`)

