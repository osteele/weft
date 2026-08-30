---
status: accepted
date: 2026-08-30
---

# 0023. Prepare only the environment the command can import

## Context and Problem Statement

Weft detects project environment markers before a job starts. A project with
`pyproject.toml` and `uv.lock` therefore receives an automatic `uv sync` setup
phase.

uv assigns different environment ownership to a script with PEP 723 inline
metadata: direct `uv run script.py` executes in the script environment and
ignores the project environment. Preparing both environments downloads and
builds packages the job cannot import. It can also fail or exhaust the rental's
time or disk before an otherwise valid command begins.

Requiring `[tool.weft] isolated = true` to suppress project setup duplicates a
fact already fixed by the command and the script. The duplicate can be omitted
or drift from the invocation, while setup logs misleadingly make the unused
project packages look like runtime provenance.

## Decision Outcome

Weft prepares only an environment the job command can import. Environment
ownership follows the invoked tool's semantics rather than a duplicate Weft
annotation.

A simple direct `uv run script.py` invocation whose target contains PEP 723
metadata owns a script environment, so Weft skips automatic project `uv sync`
without requiring `[tool.weft] isolated = true`. When ownership cannot be
proved from a compound or ambiguous command, Weft retains detected project
setup. The explicit marker remains available for self-contained commands whose
ownership Weft cannot infer.

The living job-lifecycle specification owns the command forms Weft recognizes;
this record fixes the boundary those rules implement.

### Consequences

- A direct PEP 723 script no longer pays to build or download an unused project
  environment, and project setup cannot prevent that script from starting.
- Setup logs state why project synchronization was skipped, keeping setup
  activity distinct from runtime environment provenance.
- A PEP 723 script that needs project code must declare it in the script
  environment. Preparing the project environment first does not make its
  packages visible to the script.
- Conservative recognition leaves some redundant setup in unfamiliar command
  shapes. Extending those shapes belongs in the specification and tests.
- A workflow that relied on automatic project setup for side effects before an
  isolated script must express those side effects explicitly.

## Considered Options

### Require `isolated = true` for every script-owned environment

Rejected because it asks users to restate uv's execution semantics and permits
the annotation to disagree with the command that actually runs.

### Always prepare the project environment as a harmless prerequisite

Rejected because the environment is not importable by the script, while its
network, disk, time, and failure costs are real.

### Infer ownership whenever any PEP 723 script appears in a command

Rejected because another step in a compound command may genuinely require the
project environment. Unknown ownership retains setup rather than guessing.

## More Information

- **References**: `DirectPEP723ScriptOwnsSetupEnvironment` in
  `specs/job-lifecycle.allium` and the script-environment guidance in
  `docs/guides/workflow-guide.md`; uv's
  [script environment documentation](https://docs.astral.sh/uv/guides/scripts/).
