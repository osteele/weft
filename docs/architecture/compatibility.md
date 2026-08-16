# Compatibility Architecture

How weft decides whether a job can run with a given Docker image on a given
host or cloud offer. One body of knowledge — requirement floors, curated
version tables, host capability records — serves four surfaces:

1. **Submit-time fast-fail**: reject jobs that are impossible by construction.
2. **Image selection and requirement computation**: choose a Docker image for
   cloud jobs and derive the driver/CUDA floors the rental must satisfy.
3. **Host and offer screening**: hard-filter placement candidates.
4. **Failure diagnosis**: classify runtime failures and suggest fixes.

This document maps the moving parts and their connections. Constraint
*derivation* (sources, precedence, persistence) is covered in detail in
[placement.md](placement.md) § Constraint derivation; the placement specs
(`specs/inventory-placement.allium`, `specs/campaign-lifecycle.allium`) are
authoritative for screening behavior.

## The modeled axes

Most requirements the system can currently express are NVIDIA-stack
properties. A narrow OS-toolchain slice covers known libstdc++/glibc runtime
failures on on-prem hosts:

| Axis | Job-side source | Environment-side fact | Enforced at |
|---|---|---|---|
| CUDA toolkit floor | torch pin (family floor plus operational raises), curated library floors, `[tool.weft]`, `.weft.toml [cloud]`, `--cuda-driver-min` | host `cuda_version`, image `NVIDIA_REQUIRE_CUDA` label, offer CUDA | submit fast-fail, screening, image selection |
| NVIDIA driver floor | backfilled from CUDA floor via `imagereq.MinDriverForCUDA`, or explicit `min-driver` | host `nvidia_driver`, offer driver | screening |
| Compute-capability cap/floor | torch wheel kernel tables (`dataloc`), `gpu-arch-max` | GPU model → cap via `internal/gpucatalog` | screening, offer filtering |
| GPU class | `--gpu`/`--gpu-class`, script metadata | host/offer GPU model (normalized class) | screening |
| GPU memory | `--gpu-mem`, script metadata | per-GPU VRAM | screening |
| GLIBCXX floor | curated dependency floors (`pysr`, `juliacall`), observed diagnosis | host `glibcxx_max` | screening, diagnosis |

Axes that are **not** generally modeled: kernel version, CPU ISA extensions,
Python version, non-NVIDIA accelerators, and arbitrary OS/userland package
requirements beyond the curated glibc/libstdc++ slice. See [Known
gaps](#known-gaps).

## The requirement model

`placement.RuntimeFloor` (`internal/placement/torch_compat.go`) is the
resolved minimum NVIDIA runtime requirement for a job, carrying provenance
(`CUDAOrigin`) so every surface can say *where* a floor came from ("Driver
floor: >=570 (CUDA >=12.8, from torch 2.9.1+cu128 operational floor)").
Inferred sources max-merge; explicit sources replace (and may lower) the
floor; the driver floor is derived from the CUDA floor unless explicitly
given. The shared value type is `cloud.ImageRequirements`
(`MinCUDAVersion`, `MinDriverVersion`), used for job floors and image-label
requirements alike.

Host-side scalar version floors are also represented as
`compat.Requirement` values (`internal/compat`): CUDA, NVIDIA driver, and
GLIBCXX all use the same version-min checker and violation formatter during
on-prem screening. The legacy scalar fields on `placement.Constraints`
remain synchronized as compatibility shadows while other placement surfaces
still consume them directly.

The compute-capability bounds travel separately: `MaxComputeCapForJob` /
`MinComputeCapForJob` resolve from script metadata or the torch pin, and the
cap is persisted on the job (`max_compute_cap`, three-state encoding — see
placement.md).

## Curated knowledge tables

The model leans on hand-maintained tables — "facts about the NVIDIA world"
that change only when new releases ship:

| Table | Location | Content |
|---|---|---|
| CUDA toolkit → minimum driver | `internal/imagereq` | e.g. CUDA 12.4 → driver 550 |
| Torch wheel arch bounds | `internal/dataloc` | per (torch version, CUDA variant): highest/lowest compute capability with shipped kernels |
| Library CUDA floors | `dataloc.LibraryMinCUDAFromDeps` | curated package → toolkit floor (e.g. vLLM) for requirements no lockfile exposes |
| Library toolchain floors | `dataloc.LibraryToolchainFloorFromDeps` | curated package → libstdc++ floor (e.g. PySR/Juliacall → GLIBCXX 3.4.30) |
| GPU model → compute capability | `internal/gpucatalog` | name/class → cap, generation grouping |
| Image name conventions | `internal/campaign/imagecompat.go` | parse `nvidia/cuda:<ver>-<flavor>` / `pytorch/pytorch:...` into CUDA version + flavor; supremum/compatibility relations |

The library-floor table is the template for requirements that static analysis
cannot see: when a package's native requirements are invisible to wheel
metadata, a curated entry keyed on the dependency name supplies the floor.

## Surface 1: submit-time validation

`campaign.ValidateCUDADriverMinOverride` (called from `cmd/run.go` on submit
and retry) validates explicit CUDA-floor override syntax. Submit does not
reject pinned CUDA images whose tag is older than a Python dependency floor:
modern torch/vLLM wheels bundle their CUDA user-space libraries, so the hard
compatibility gate is the host or rental driver floor, backed up by the
agent-side torch CUDA preflight.

## Surface 2: image selection and requirement computation

For cloud jobs, `campaign.ResolveJobImageSettings` resolves the image with
precedence script `[tool.weft] image` > matching
`.weft.toml [cloud.image-overrides]` entry > `.weft.toml [cloud] image` >
framework aliases > auto-selection (see below) > default.
`SplitGroupsByImage` then groups jobs per launch:

- **Auto-selection**: follows which environment will import torch, not merely
  whether the project depends on it. The project uv env owns torch when the
  project declares CUDA packages and the script neither sets `isolated` nor
  declares its own PEP 723 torch/CUDA dependencies; only then does a
  preinstalled `pytorch/pytorch` image help, so only then is one selected. A
  PEP 723 script with inline `dependencies` resolves on the worker in an
  isolated uv env, so the project lockfile describes an env the job never
  imports — those jobs get an `nvidia/cuda` base image instead. Either way the
  CUDA version comes from the resolved runtime floor, so weft cannot select an
  offer for one CUDA version and provision an image built for another.
- **Variant**: `-devel` when a dependency JIT-compiles kernels at run time and
  therefore needs `nvcc` (vLLM/flashinfer), `-runtime` otherwise.
- **Version lookup**: the smallest tabled image version ≥ the required CUDA
  *within the same major*; no cross-major substitution. An unmapped floor such
  as 12.7 takes the 12.8 image rather than falling back to the default.
- **Auto-upgrade**: a GPU-class constraint that implies a newer CUDA (e.g.
  Blackwell → CUDA ≥ 12.8), or a `-runtime` image where the dependencies need
  `nvcc`, upgrades the image — unless the project torch pin governs the runtime
  env, in which case the wheel cannot use a different runtime and the pin wins.
- **Merging**: jobs with mutually compatible images share an instance via
  `imageSupremum` (base < runtime < devel; nvidia/cuda < pytorch/pytorch).
- **Aliases**: legacy/private SGLang runtime names normalize to the public
  `lmsysorg/sglang:v0.5.10.post1` image before grouping or probing.

`campaign.ApplyImageMetadataRequirements` fetches the chosen image's OCI
config (`imagereq.Resolve`), parses its `NVIDIA_REQUIRE_CUDA` label, and
raises the group's driver/CUDA floors accordingly (raise-only merge, driver
backfilled from CUDA). These floors then gate offer selection.

Submit-time rental jobs also probe the resolved image manifest with configured
registry credentials. Missing tags, unauthorized private registries, and other
manifest/config fetch failures stop submission before a rental is launched.

Because every supported image is Ubuntu 22.04-based, image selection also
*silently normalizes the OS axis* for cloud jobs — an asymmetry discussed
under Known gaps.

## Surface 3: host and offer screening

`placement.CheckHostGPUConstraints` applies the hard gates per host — GLIBCXX
toolchain floor, GPU class, per-GPU memory, arch cap/floor, CUDA/driver floors
— and produces prefixed rejection reasons that flow into `placement_reasons` /
`placement_blocked`. The cloud path applies GPU bounds to offers
(`campaign.filterOffersByTorchArch` plus driver/CUDA checks). When CUDA/driver
floors are required, Vast.ai offers marked with the datacenter forward-compat
driver stack are excluded on consumer NVIDIA GPUs because that combination has
failed with CUDA forward-compat error 804. Fail-open vs fail-closed semantics
per bound, scoring, and observability are covered in
[placement.md](placement.md).

Torch-derived GPU-runtime constraints apply **only to jobs that request a
GPU**; CPU-only jobs ignore CUDA/driver and compute-cap floors, but still
screen against known host toolchain floors such as GLIBCXX.

## Surface 4: failure diagnosis

`internal/remediation` classifies failed runs from log content:

- `failurePatternRules` (`patterns.go`) is an ordered table of regex rules,
  each with a pattern ID, category (`environment` / `code` / `data`), a
  confidence score, a human-readable message and solution, and an optional
  structured-details extractor (e.g. the OOM rule extracts requested MiB and
  device ID; the CUDA-symbol rule extracts the undefined symbol and library).
- Ordering encodes specificity: `cuda_lib_symbol_mismatch` precedes the
  generic `module_not_found` rule that would otherwise swallow it.
- Data-asset patterns are checked before code patterns
  (`DiagnoseFromLog`) because they are remediable: weft can stage the
  missing asset and retry.
- Two entry points: `DiagnoseFromLog` returns nil when nothing matches (used
  by display-time backfill from cached logs); `DiagnoseFailedAttemptFromLog`
  always returns a diagnosis, falling back to pattern `unknown` (used by the
  post-run remediation path).

Diagnoses are persisted as JSON on `job_status.error_diagnosis` /
`job_attempts.error_diagnosis` and rendered by `weft info`, the TUI, and
`weft explain`.

Diagnosis is **mostly advisory**: it produces prose for humans. A
`cuda_driver_too_old` match does not taint the host or attach a floor to the
job for its retry. One hard feedback path exists for images: several
consecutive pre-start infrastructure failures for the same image block fresh
rental placement for that image until a later launch reaches OnStart/agent
readiness and breaks the chain.

## Known gaps

These are documented debt; the redesign direction is in
[docs/planning/compatibility-model.md](../planning/compatibility-model.md).

**Narrow OS-toolchain modeling.** Host records include OS release, glibc, and
max `GLIBCXX_*` facts; PySR/Juliacall infer a `GLIBCXX_3.4.30` floor; and
`GLIBCXX_*` / `GLIBC_*` loader failures have diagnosis patterns. This covers
the wj2872 incident class, but it is still a curated slice, not a general OS
package or native-binary inference model.

**Partial unified requirement representation.** On-prem scalar version floors
share `compat.Requirement` / `compat.FactSet` / `compat.Violation`, but the
broader system still has multiple vocabularies: `RuntimeFloor` /
`ImageRequirements` for CUDA derivation and image labels, GPU constraints for
class/memory/caps, and per-axis persisted job columns. The remaining work is
to persist resolved requirement sets and move cloud offer/reuse paths onto
the same checker.

**Most diagnoses do not close the loop.** Runtime-discovered incompatibilities
are not converted into host facts or job requirements, so a retry can be
routed straight back to the incompatible host; outside the image pre-start
breaker, the feedback loop is the human operator.

**Unmatched failures vanish.** When no pattern matches on the post-run path
the `unknown` diagnosis is stored, but display-time backfill
(`DiagnoseFromLog`) returns nil — the user sees no classification at all for
a novel failure signature.

## File map

| Package | Role |
|---|---|
| `internal/placement` | `RuntimeFloor` resolution with provenance, torch operational floors, compute-cap bounds, host screening (`torch_compat.go`, `placement.go`) |
| `internal/imagereq` | OCI image-label fetching, `NVIDIA_REQUIRE_CUDA` parsing, CUDA→driver floor table |
| `internal/campaign` | Image resolution/selection/merging (`campaign.go`, `imagecompat.go`), submit fast-fail, offer filtering |
| `internal/dataloc` | Torch pin scanning, CUDA variant/family floors, torch wheel arch tables, library CUDA floors, PEP 723 metadata |
| `internal/gpucatalog` | GPU model → compute capability catalog |
| `internal/inventory` | Host capability records (`~/.config/weft/hosts/*.yaml`) |
| `internal/remediation` | Failure pattern table, diagnosis entry points, persistence model |
