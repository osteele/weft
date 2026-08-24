---
status: accepted
date: 2026-08-24
---

# 0020. Keep unattributed sessions inclusive and unproven project roots unassigned

## Context and Problem Statement

The unprocessed-job inbox groups work by logical project ownership and exact
submitter session. Both dimensions can be absent, but absence means something
different for each one.

An absent submitter session is positive evidence that no session owns the job.
An absent project root is missing evidence about which filesystem project owns
the job. Treating these nulls uniformly would either hide orphaned work or
invent project membership.

Working directories identify where commands ran, not which project owns them.
The logical project name is overridable, so neither a bare name nor an execution
directory proves the canonical owning path.

The grouped surface also needs a disposition that tells a consumer whether to
direct attention toward program output or toward the execution environment.
The raw attempt status cannot support that distinction. Among non-zero-exit
attempts where a specific forensic rule fired, 421 were marked failed and one
was marked completed. Attempts carrying only fallback reasons or no reason had
mixed raw statuses, and the determinant of that marking is unexplained.

## Decision Outcome

Include jobs with an absent submitter session in an explicit unattributed
session bucket. Absence is the evidence: unattributable and unowned by a
session are the same state.

Exclude jobs with an absent project root from every rooted per-project count,
and emit them in an explicit null-root bucket. Absence here is lack of evidence
about membership. Assigning such a job to project P would invent the fact the
grouped surface claims to report.

Derive project roots from the local submission directory, canonicalize them to
absolute paths with symlinks resolved, and accept them only when the root
basename exactly matches the resolved logical project name. Legacy backfill
uses the same basename proof after resolving the stored working directory and
finding its repository root. A fallback to the execution directory is rejected:
a fallback is a guess wearing a fact's clothes.

Classify each grouped inbox row into exactly one of `completed_ok`,
`completed_error`, `infra_suspected`, or `dead`. Make the cut on whether the
recorded forensic reason exactly equals an allowlisted machine-generated
constant, not on the raw attempt status. The allowlist contains the documented
signal, resource, timeout, driver, prewarm, artifact-staging, torch CUDA
preflight, and CUDA hardware-fault reasons. It excludes user-attributable torch
environment/import preflight failures and dynamic `signal_*` reasons.

Treat every other non-zero or absent exit reason as `completed_error`, including
free-form prose and log dumps. This deliberately under-claims infrastructure
suspicion: mistakenly asking someone to read program output is less harmful
than directing them to inspect a host for a failure in their own program.

### Consequences

- Orphaned session work remains visible and actionable.
- Consumers can distinguish no rows from rows whose ownership evidence is
  incomplete.
- Explicit project-name overrides commonly retain a null project root until
  another authoritative source supplies ownership.
- Some legacy jobs remain outside rooted per-project totals even though their
  bare project names look plausible.
- Grouped output must preserve raw session ids and explicit null buckets.
- Disposition consumers receive a conservative infrastructure signal grounded
  in an exact forensic reason, not an unexplained attempt-status marking.
- New machine-generated infrastructure reasons must be added explicitly before
  the grouped surface will classify them as `infra_suspected`.

## Considered Options

### Drop rows with either null dimension

Rejected: dropping an absent session hides precisely the orphaned work the
surface exists to reveal, and dropping an absent project root makes unknown
membership indistinguishable from no such jobs.

### Assign null roots from working directories

Rejected: the working directory is execution location, not logical ownership,
and explicit project overrides prove that the two can differ.

### Group by bare project name

Rejected: names are not canonical cross-system identities and can collide or
refer to an owner unrelated to the execution repository.

### Split error dispositions by raw attempt status

Rejected: the marking's determinant is unexplained for the 6,224 non-zero-exit
attempts with fallback or absent forensic reasons. Although a specific forensic
rule predicts failed rather than completed by 421 to 1, that evidence supports
cutting directly on the forensic rule; it does not give the raw status a stable
user-facing meaning.

## More Information

- **References**: [Job lifecycle specification](../../specs/job-lifecycle.allium)
