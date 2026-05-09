# Iterative Weft Improvement

Weft keeps enough local job metadata and source snapshots to turn repeated
failures into product improvements. Use this loop when a job series took too
long to diagnose, when a project has many near-duplicate submissions, or during
a regular maintenance pass.

## Mine Recent Jobs

Start with the recent-history commands:

```bash
weft job anomalies --recent 100
weft job churn --recent 200
weft job recommend --recent 100
```

`anomalies` ranks jobs that look worth reviewing: failed or dead terminal
states, non-zero exits, retries, multiple attempts, suspicious cloud outcomes,
and common Hugging Face cache or offline-mode metadata gaps.

`churn` groups jobs that look like iterations of the same script or command.
These groups are usually where a small metadata, environment, source, or
placement difference explains a long debugging session.

`recommend` summarizes recurring patterns from the same signals. Treat the
output as a triage prompt, not as proof; confirm each recommendation against
the actual job metadata, logs, and source.

All three commands support `--json` for scripts and notebooks.

## Inspect A Candidate

For each promising anomaly or churn group, compare the job metadata first:

```bash
weft job inspect wj1877 --json
weft job diff wj1876 wj1877
```

If metadata does not explain the behavior, inspect the uploaded source
snapshot:

```bash
weft source ls wj1877
weft source cat wj1877
weft source diff wj1876 wj1877
```

Use logs to connect the submitted metadata and source to the observed failure:

```bash
weft log wj1877
```

## Classify The Finding

Classify each issue before changing code:

- User mistake that better docs or examples could prevent.
- Submission validation gap that should fail before a job is launched.
- Diagnostic gap where Weft had the signal but hid the useful cause.
- Placement or queueing gap where Weft chose a poor host or rental without
  showing enough context.
- Runtime environment gap such as cache ownership, offline mode, or missing
  environment overrides.

Good improvements usually move the diagnosis earlier: reject malformed
metadata, warn about suspicious host defaults, surface queue depth before
placement, or add a short recipe to the workflow guide.

## Turn Findings Into Changes

Prefer small, testable changes:

- Add a focused CLI validation or warning for a repeated failure mode.
- Improve the wording of an existing error before adding a new command.
- Add a guide recipe when the correct fix is workflow knowledge.
- Add a regression test using normalized job records or metadata parsing.
- Link command details from the reference instead of expanding the README.

After implementing the change, run the same mining command again against the
same recent window. The best result is that the next review points to fewer
manual mysteries and more concrete follow-up actions.
