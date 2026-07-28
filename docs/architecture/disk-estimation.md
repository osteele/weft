# Disk Estimation

Weft uses disk estimates in two places with different risk profiles:

- **Fresh rental launch sizing** chooses the requested container disk for a new
  instance.
- **Rental reuse admission** decides whether an already-running instance has
  enough remaining disk for one more job.

Fresh launch sizing can rely on total requested disk and safety floors. Reuse
admission must be stricter because it is working with remaining free space on a
mutable disk after previous jobs, caches, uploads, and cleanup.

## Fresh launch sizing

`internal/campaign.EstimateGroupDisk` computes a group-level disk request from:

- declared and post-mortem observed inputs, with Hugging Face cache expansion;
- cached `uv sync` manifests, or a lockfile package-count fallback when no
  manifest exists;
- fixed base Docker/workspace overhead;
- CUDA setup overhead when project dependencies include torch, vLLM, JAX,
  TensorFlow, SGLang, or NVIDIA CUDA runtime wheels;
- reduced CUDA setup overhead when the selected image is `pytorch/pytorch:*`;
- explicit `--runtime-disk` / `[tool.weft] runtime-disk`;
- empirical historical peak disk, when all jobs in the group have usable
  history;
- explicit `--disk` / `[tool.weft] disk` as a hard floor;
- `DefaultMinDiskGB` as the final minimum;
- explicit `--disk-max` / `[tool.weft] disk-max` as a hard ceiling, applied
  last.

Historical telemetry above the plausibility bound is rejected and surfaced as
an anomaly instead of being clamped silently.

## The disk ceiling (`--disk-max`)

Every other input above can only push the estimate up. `--disk-max` is the one
that can pull it down, and it is applied after all of them — including
`DefaultMinDiskGB` and an explicit `--disk` floor. Declaring both a floor and a
ceiling is contradictory; the ceiling wins, because letting the floor win would
make the ceiling unusable on exactly the jobs that set both.

This exists because disk is the only estimated quantity that acts as a *hard
placement filter*: `MinDiskGB` becomes a server-side `disk_space>=N` predicate
in the vast.ai offer search. A conservative over-estimate therefore does not
just cost money — it shrinks the offer pool before scoring runs, and can empty
it. A user who knows their real footprint (for example, one who constrains
Hugging Face downloads with `allow_patterns` so the estimator's whole-repo
`usedStorage` figure overstates what is actually fetched) needs a way to say so.

The ceiling is always explicit and never inferred. When it binds, weft emits a
`disk estimate capped by user-declared ceiling` warning recording the estimated
size, the cap, and the affected job IDs, so a later `disk_full` on that instance
is attributable to the cap rather than to the estimator.

Across a group the effective ceiling is the *minimum* of the ceilings declared
by its jobs — honoring one job's ceiling means the shared instance cannot exceed
it. Jobs that declare no ceiling do not contribute.

## Reuse admission

`internal/campaign.MatchJobToInstance` checks whether an existing rental can
accept a job. Its disk gate estimates only the incremental disk that the target
instance still needs:

- inputs not already provisioned on the instance;
- cached `uv sync` manifest bytes when available;
- otherwise a conservative setup fallback: full CUDA setup headroom for CUDA
  projects on CUDA-only images, reduced headroom only when a PyTorch image can
  plausibly use the system-torch shortcut;
- explicit runtime scratch headroom;
- explicit total disk floors that exceed the target instance's recorded disk.

The reuse gate intentionally treats uncertain PyTorch shortcut eligibility as
full CUDA setup cost. A false rejection launches or waits for another rental; a
false acceptance can terminate a shared rental with `disk_full`.

## Image-aware setup costs

PyTorch images can avoid reinstalling torch and NVIDIA runtime wheels only when
the runner can use the system-torch shortcut: create `.venv` with
`--system-site-packages`, set `UV_PYTHON` to the image Python, and run
`uv sync --no-install-package ...` for torch/CUDA packages. If the project
forces an incompatible Python version, the runner falls back to plain `uv sync`
and the disk estimator must budget for the full wheel install.

CUDA-only images, including `nvidia/cuda:*devel*`, provide CUDA tooling but not
the project torch wheel. Torch jobs on these images need enough free disk for
the full wheel stack unless a cached manifest proves a smaller setup footprint.

## Known Gaps

Launch grouping can still merge jobs whose images are compatible but whose
setup disk economics differ, such as vLLM jobs that need a CUDA devel image and
later torch jobs that would be cheaper on a PyTorch image. The current reuse
guard prevents unsafe reuse in that situation; future work should make grouping
price or split these combinations before launch.
