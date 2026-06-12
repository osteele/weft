# Proposal: General Compatibility Model

Status: proposed (June 2026). Companion to
[docs/architecture/compatibility.md](../architecture/compatibility.md), which
documents the current system and its gaps.

## Motivation

The compatibility subsystem answers "can this job run with this image on this
host/offer?" on four surfaces (submit fast-fail, image selection, candidate
screening, failure diagnosis) — but every requirement it can express is an
NVIDIA-stack property. The wj2902/wj2872 incident is the canonical failure: a
CPU-only PySR job was placed on an Ubuntu 20.04 host, where the Julia runtime
(downloaded at run time by juliapkg) required `GLIBCXX_3.4.30` that the
host's libstdc++ lacks. All four surfaces passed it through, the diagnosis
came back empty, and the feedback loop (rerouting the retry) was the human
operator.

The structural cause is not "missing glibc check"; it is that each axis is
hand-woven through the system. Requirements live in three vocabularies
(`RuntimeFloor`, GPU constraints, per-axis job columns), resolved by three
divergent paths (roadmap: "Unify constraint resolution"), checked by
axis-specific code at each surface, and diagnosed by patterns that don't
reference the requirement model at all. Adding an axis today means touching
submit persistence, placement, campaign image logic, and the pattern table
separately — so axes don't get added, and the first non-CUDA incompatibility
sailed through.

## Goals

- Adding a compatibility axis (glibc, CPU ISA, Python version, kernel
  feature, …) is a **registry entry plus data**, not a cross-cutting change.
- One resolution path and one checker serve all four surfaces, with uniform
  provenance ("required X ≥ v, from <origin>; host has v′").
- Runtime failures **feed back** into the model instead of evaporating into
  prose.

Non-goals: solving static inference of arbitrary native requirements (it is
unsolvable in general — code can download anything at run time); replacing
the GPU-class subsumption logic with a generic comparator; changing user-facing
CLI flags.

## The model

Three small concepts, replacing the current per-axis plumbing:

**Requirement** — `{axis, comparator, value, origin, hardness}`. A job's
requirement *set* is produced by a single resolver from the existing layered
sources, preserving today's semantics exactly: inferred sources max-merge,
explicit sources replace and may lower, provenance is retained per
requirement. (This is `RuntimeFloor` generalized from two hardcoded fields to
a list.) `hardness` distinguishes proven requirements from heuristic ones
(e.g. learned-from-failure, below) so surfaces can choose fail-open or
fail-closed per requirement rather than per code path.

**Facts** — `{axis → value}` for each execution environment. Three producers:

- *Hosts*: probed by `weft host discover` (which already SSHes in) and cached
  in the host YAML — OS release, glibc version, max GLIBCXX symbol, kernel,
  CPU flags join the existing driver/CUDA/GPU facts.
- *Images*: OCI labels (today's `NVIDIA_REQUIRE_CUDA`) plus curated facts
  keyed on image name conventions (ubuntu22.04 → its glibc/libstdc++).
- *Offers*: provider metadata (driver, GPU model), as today.

The key conceptual move is **environment composition**: where a job actually
runs is a *stack* — image facts layered over host/offer facts. The image
supplies userland facts (libstdc++, CUDA toolkit, Python); the metal supplies
kernel-adjacent facts (driver, GPU, ISA). Today the image and host checks are
separate code paths; in the model they are one check against the composed
fact set. This makes the on-prem/cloud asymmetry explicit and inspectable:
on-prem bare-metal execution composes *no* image layer, which is precisely
why host OS facts are load-bearing there.

**Violation** — the single checker evaluates a requirement set against a
composed fact set and returns `{axis, required, actual, origin}` records. All
four surfaces render the same violations: submit-time fast-fail formats them
as errors, screening as rejection reasons, `weft info` as provenance lines,
and diagnosis as explanations. A missing fact (host file predates the probe)
yields "unknown", and the requirement's hardness decides pass-or-reject —
matching today's documented fail-open arch cap vs fail-closed floors.

## The axis registry

Each axis declares, declaratively:

- value type and comparator (version-min, version-max, set-membership;
  GPU class keeps its custom subsumption comparator — the registry admits
  special comparators rather than forcing scalars);
- an optional **host probe** (shell fragment run by `host discover`);
- an optional **image fact source** (OCI label parser or curated table);
- optional **inference rules** (lockfile/PEP 723 scanners, curated
  package→requirement tables — `LibraryMinCUDAFromDeps` is the existing
  template);
- optional **diagnosis signatures**: log regexes that (a) classify the
  failure and (b) parse the *actual requirement* out of the error text.

The glibc axis then costs: one probe (`ldd --version`, max `GLIBCXX_` symbol
from libstdc++), one curated package entry (pysr/juliacall → official Julia
binaries → GLIBCXX ≥ 3.4.30), one diagnosis signature
(``version `GLIBCXX_x.y.z' not found``). No placement, campaign, or submit
code changes.

## Diagnosis closes the loop

Diagnosis signatures that parse a concrete requirement from a failure log
emit a machine-readable **observation**, not just prose:

- **Job requirement discovery**: "this job needs GLIBCXX ≥ 3.4.30" — attached
  to the job (hardness: observed) so the retry screens on it. This is how the
  unsolvable static-inference problem gets a practical answer: the first
  failure of a novel requirement converts it into a constraint, and a curated
  registry entry can later promote it for all jobs sharing the dependency.
- **Host fact correction**: when the probe axis exists but the host fact was
  stale or missing, the observation updates/flags the host record.

Observations surface in `weft info` with their provenance ("observed from
failed attempt #1"), distinguishable from declared or inferred requirements.

## Migration path

Each phase is independently shippable and useful.

1. **Quick win (no new model)**: add the OS-toolchain host probes to
   `host discover`; add the PySR/Juliacall → GLIBCXX floor inference; add
   GLIBCXX/GLIBC diagnosis patterns with structured details; hard-screen when
   both job floor and host fact are known (fail-open on missing host facts
   until hosts are re-discovered). This fixes the incident class without the
   refactor.
2. **Unify resolution** (subsumes roadmap "Unify constraint resolution"):
   introduce Requirement/Facts/Violation types and re-express the existing
   CUDA/driver/arch checks through the single resolver and checker,
   behavior-preserving. On-prem scalar version floors (CUDA, NVIDIA driver,
   GLIBCXX) use the unified checker without schema changes. Full resolution
   unification persists the resolved requirement set (one JSON column;
   existing per-axis columns remain as query shadows), makes placement trust
   it, and moves the reuse path to the same checker.
3. **Registry-fy the knowledge**: move the curated tables (CUDA→driver,
   torch arch bounds, library floors, image facts) behind axis declarations.
4. **Feedback**: diagnosis observations become persisted requirements/fact
   corrections; retries screen on them.
5. **New axes as needed**: CPU ISA (AVX-512 wheels), Python version, kernel
   features, disk — each a registry entry.

## What it buys

- The next novel incompatibility is caught at one of the four surfaces — or
  at worst converted into a constraint after its first failure, instead of
  recurring.
- One vocabulary and one checker: rejection reasons, fast-fail errors, and
  diagnoses can never disagree about what was required and why.
- The three-paths divergence (submit vs placement vs reuse) collapses.
- Curated knowledge becomes data with one shape, easier to review and extend.

## Risks

- **Over-generalization**: comparators differ genuinely (GPU class
  subsumption, generation names). Mitigation: custom comparators per axis;
  GPU class/memory may stay special-cased through phase 2 and join the
  registry last, or never — the registry must earn each migration.
- **Schema churn**: persisted requirement sets must coexist with the
  existing per-axis columns that external consumers (estimator, lab
  notebooks) read. Keep columns as synchronized shadows, as the
  execution-targets migration already does.
- **Probe drift**: host facts go stale after OS upgrades. The diagnosis
  feedback path (fact corrections) is the safety net; probes re-run on
  `host discover`.
