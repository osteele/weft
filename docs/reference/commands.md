# Command Reference

This is the canonical CLI reference for `weft` commands. For end-to-end workflows, see [Workflow Guide](../guides/workflow-guide.md) and [Cloud GPU Instances](../guides/instances.md).

## Commands

### weft provider

Inspect and edit which cloud GPU providers are eligible for rental placement.

```bash
weft provider list [--json]
weft provider offers [GPU...] [--provider vastai|runpod] [--min-gpu-mem GB] [--min-survival FRACTION] [--need N]
weft provider enable vastai|runpod
weft provider disable vastai|runpod
weft provider reset vastai|runpod
```

`enable`, `disable`, and `reset` update `~/.config/weft/config.toml`.
`reset` removes that provider's `enabled` key so it returns to the default
policy.

Provider enablement is tri-state:

- `enabled = true`: search this provider
- `enabled = false`: do not search this provider
- unset: automatic/default policy

For compatibility, when no provider has an explicit `enabled` setting, Weft
searches Vast.ai and leaves RunPod inactive. Once any provider is explicitly
enabled or disabled, Weft searches only providers with `enabled = true`.

RunPod launches use community cloud by default. Set
`[runpod] cloud_type = "secure"` in `~/.config/weft/config.toml`, or pass
`weft start instance --runpod-cloud-type secure` for one launch.

`weft provider offers` is a read-only capacity report. It searches live rental
offers, joins them with Weft's survival model, and reports how many candidates
clear a survival floor. Use `--need N` for host-distribution or anti-affinity
runs that need N distinct machines. Vast.ai offers expose machine IDs, so Weft
can count distinct machines directly. RunPod exposes GPU-type stock labels
instead of physical machine counts, so RunPod rows may report `unknown` for
multi-machine needs even when stock is present.

Examples:

```bash
# See current provider policy
weft provider list

# See which RunPod community GPU types can plausibly satisfy five 20GB jobs
weft provider offers --provider runpod --runpod-cloud-type community --min-gpu-mem 20 --min-survival 0.4 --need 5 rtx_3090 rtx_4090 l40 l40s rtx_a6000

# Use only RunPod, even if Vast.ai was previously enabled
weft provider enable runpod
weft provider disable vastai

# Use both providers
weft provider enable vastai
weft provider enable runpod

# Disable rental placement across all providers
weft provider disable vastai
weft provider disable runpod

# Return Vast.ai to the legacy default policy
weft provider reset vastai
weft provider reset runpod
```

### weft run

Queue a job on a remote host for managed execution.

```bash
weft run [flags] [host] <command...>
```

The host is optional; the local placement engine automatically selects the best
host based on GPU constraints, data locality, current utilization, and queue
depth.

By default, jobs are added to a queue and scheduled by the queue runner. It can run multiple jobs on a host while keeping total CPU usage under a target cap. Use `--immediate` (`-i`) to start a job immediately.

Use `start <job-id>` to start a queued job immediately.

**Flags:**
- `-i, --immediate`: Start job immediately instead of queuing
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-m, --message TEXT`: Description of the job (for logging and queries)
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--tag TAG`: Tag to attach to the job (can be repeated). Most tags are user-defined; reserved scheduler tags such as `rental`, `inventory`, `benchmark-isolation`, `exclusive`, `interruptible`, and `cpu-intensive` are described in the [Placement guide](../guides/placement.md#reserved-tags).
- `--draft`: Record the job locally in draft status (never contacts the host until you later promote it)
- `-f, --follow`: Follow log output after starting (requires `--immediate`)
- `--allow`: Stream the job log live and stay attached (requires `--immediate`)
- `--from ID`: Copy settings from existing job ID (allows overriding)
- `--timeout DURATION`: Kill job after duration (e.g., "2h", "30m", "1h30m")
- `--after, --depends-on ID`: Start job after another job succeeds. For inventory jobs this is enforced by the per-host queue; for rental jobs it is a placement gate — the downstream is held back from `weft start instance` selection until the upstream succeeds, but co-location is not forced
- `--after-any ID`: Start job after another job completes, success or failure. Same gating behavior as `--after` for rentals
- `--kill ID`: Kill a job by ID (synonym for `weft kill`)
- `--input ASSET`: Declare a data input. Accepts HF refs (`hf:model-id`), project-relative directories (`local:data/conllu/`), or absolute/tilde paths. HF assets influence placement scoring and trigger downloads; `local:` paths are synced via rsync before the job runs
- `--output ASSET`: Declare a data output (e.g., `checkpoint:llama-ft-v1`, `local:cache/representations/`, or project output directories). Recorded on successful completion for downstream jobs; repeat it for extra files or directories outside the conventional `output/` and `outputs/` directories
- `--gpu CLASS`: GPU constraint with optional memory (e.g., `a100`, `ampere+`, `nvidia>=24GB`, `h100-pcie`, `h100-hbm3`)
- `--gpu-class CLASS`: Require a specific GPU class, variant, or generation (e.g., `a100`, `gh200`, `h100-sxm`, `ampere+`)
- `--gpus N`: Require exactly N GPUs on one host or rental instance
- `--gpu-mem GB`: Requested GPU memory in GB (weft adds `+2GB` headroom by default, except when the value matches a known hardware ceiling — see below)
- `--gpu-mem-strict`: Use exact `--gpu-mem` matching (disable default `+2GB` headroom)
- `--interconnect any|pcie|nvlink|nvlink-uniform`: Multi-GPU topology requirement
  (`any` is the default for `--gpus N`). `nvlink` requires NVLink to be present;
  `nvlink-uniform` additionally requires every participating GPU pair to be NVLink
  connected, which above two GPUs means SXM/NVSwitch parts only. See
  [placement](../guides/placement.md) for why a bridged host satisfies the first
  and not the second.
- `--nvlink-required`: Alias for `--interconnect nvlink`
- `--same-host`: Require all requested GPUs on one host; this is currently the only supported multi-GPU launch semantic
- `--cpu-cores N`: Require at least N effective CPU cores/vCPUs on rental offers
- `--provider vastai|runpod`: Restrict rental placement to one cloud provider. Use this for provider-specific testing; omit it for normal automatic provider selection.
- `--runpod-cloud-type community|secure`: For RunPod-bound jobs, choose the RunPod cloud type for this job. A non-empty value records the job as RunPod-bound without changing global defaults.
- `--max-hourly-rate USD`: Reject rental offers whose total recurring rate, including storage for the requested disk size, exceeds this amount. A group in which every job has an explicit rate cap may launch beyond the autopilot's global hourly soft target, provided the selected offer satisfies every cap.
- `--max-spend USD`: Set a hard spend limit for the rental instance that runs this job.
- `--max-time DURATION`: Set a hard lifetime for that rental instance (for example, `2h30m`).
- `--min-survival FRACTION`: Set the minimum accepted Weft learned end-to-end survival probability, from `0` to `1`; `0` disables the floor. This is distinct from the provider reliability score filtered by `[campaign] reliability` (default `0.95`).
- `--grace-period DURATION`: Override how long the instance remains available after a job failure; `0` disables the grace period.

Use `0` to clear `--max-hourly-rate`, `--max-spend`, or `--max-time` when
overriding values copied with `--from`. Use `default`, `clear`, or `auto` to
remove a `--grace-period` override. If the hourly-rate or survival filters
remove every offer, the job remains queued and `weft diagnose` reports the
constraint. `weft diagnose job JOB_ID` also performs a read-only live-market
counterfactual analysis: it holds the requested GPU class fixed and reports
which one-factor relaxations expose additional or viable offers.

**Hardware-ceiling auto-strict.** When `--gpu-class` (or `--gpu`) names a
specific model and `--gpu-mem` matches that model's actual capacity (A100
40GB/80GB, H100 80GB, H200 141GB, RTX 4090 24GB, RTX 3090 24GB, and other
catalogued sizes), weft treats the value as the hardware ceiling and skips
the `+2GB` headroom — adding it would over-constrain the search filter
past what the hardware reports. So `--gpu-class a100 --gpu-mem 80` and
`--gpu a100>=80GB` produce the same `gpu_ram>=80` filter. Non-ceiling
values like `--gpu-mem 60` still get the headroom (effective 62GB). The
catalogued models live in `internal/vastai/hardware_memory.go`. Jobs
persisted under the older behavior (where the headroom was applied at
submit time, baking 82 into the DB for an A100 80GB request) are
recognized at filter time and rolled back automatically. No manual
`weft restart` is required.
- `--produces PATH`: Artifact path this job produces (repeatable, e.g., `output/model.pt`)
- `--needs PATH:VERSION`: Artifact path:version this job needs (repeatable, e.g., `output/model.pt:100`; if the producer is on a live reusable rental, autopilot co-locates the consumer on that rental queue so it starts after the producer completes; otherwise rental/ephemeral producer artifacts are staged from cloud artifact storage, including producers that completed hours or days earlier — no need to pre-fetch with `weft artifact get` and pass `--input local:`)
- `--dry-run`: Show placement scores without submitting the job. For rental offer cost, survival, and memory-headroom previews, use `weft start instance --dry-run` or `weft instance new --dry-run` after the job is queued.
- `--no-sync`: Skip immediate on-prem dispatch sync. Submit-time cloud source
  pinning still runs.
- `--wait`: Wait for the job to complete before returning

**Script metadata:** Python scripts can declare resource requirements inline
using a [PEP 723](https://peps.python.org/pep-0723/) `[tool.weft]` table.
These are applied as defaults — CLI flags take precedence. See
[Workflow Guide § Script metadata](../guides/workflow-guide.md#script-metadata).
For Vast.ai instance launches, metadata also supports `vast-cap-add` to request
extra container capabilities (for example, `["SYS_ADMIN"]`).
Set `interruptible = true` in `[tool.weft]` to opt a script into interruptible
cloud placement (equivalent to tagging the job with `--tag interruptible`). The
key `preemptible` and tag `preemptible` are accepted as synonyms.

**OOM history:** When a job fails with a GPU out-of-memory error, weft records
the GPU capacity. On subsequent submissions of the same command, the minimum
effective GPU memory requirement is automatically raised above the capacity that
caused the OOM, preventing the same failure from repeating. An explicit
`--gpu-mem` flag overrides this floor.

If an explicit-host run can't reach the host, the CLI records the job locally
with that host target and defers it to the remote queue. Any full host sync can
append the saved entry later: `weft sync`, the daemon sync loop, and TUI
background sync all dispatch queued jobs whose destination is already known.
This still happens while autopilot is paused, because autopilot pause stops new
placement decisions and instance creation, not sync dispatch to a known host.

Draft job records (`--draft`) stay local and are not dispatched. Submission
still uploads and records their source pin so later activation cannot pick up a
different working tree. When you’re ready to discard the draft, run `weft job
draft <id>` to toggle the status (or do it from the TUI, described below).

**Examples:**
```bash
# Queue a job (default behavior)
weft run deepthought 'python train.py'

# Let autopilot rent 8 H200s within explicit price and lifetime limits
weft run --tag rental --gpu h200 --gpus 8 \
  --max-hourly-rate 33.20 --max-spend 83 --max-time 2h30m \
  --min-survival 0.6 --grace-period 0 'python train.py'

# Start a queued job immediately
weft start wj123

# Start immediately instead of queuing
weft run -i deepthought 'python train.py'

# With description (recommended)
weft run -m "Training GPT-2 with lr=0.001" deepthought 'with-gpu python train.py --lr 0.001'

# Explicit working directory
weft run -C /mnt/code/LM2 deepthought 'with-gpu python train.py'

# Start immediately and follow log output
weft run -i -f -m "Training run" deepthought 'python train.py'

# Stay attached to live output (Ctrl+C detaches, job keeps running)
weft run -i --allow -m "Training run" deepthought 'python train.py'

# Set environment variables
weft run -e CUDA_VISIBLE_DEVICES=0 -e BATCH_SIZE=32 deepthought 'python train.py'

# Tag jobs for later filtering
weft run --tag exp-012 --tag notebook-sync deepthought 'python train.py'

# Run a job exclusively (waits until no other jobs are running, blocks others while running)
weft run --tag exclusive deepthought 'python large_model.py'

# Run a benchmark (exclusive + waits for system-wide idle: low CPU, RAM, GPU, VRAM;
# auto-placement skips hosts marked shared = true)
weft run --tag benchmark-isolation deepthought 'python bench_encode.py'

# Force rental placement (legacy alias: --tag cloud)
weft run --tag rental --gpu a100 'python train.py'

# Keep a job on inventory hosts only (legacy alias: --tag on-prem)
# Inventory-tagged benchmark jobs may still use shared inventory hosts
weft run --tag inventory --gpu a100 'python train.py'

# Run job after another succeeds
weft run --after wj42 deepthought 'python eval.py'

# Run cleanup job after another completes (success or failure)
weft run --after-any wj42 deepthought 'python cleanup.py'

# Artifact-based dependencies (file-path deps instead of job-ID deps)
# Job 100: training — declares it will produce a checkpoint
weft run --produces output/model.pt -m "Fine-tune Llama-3 8B" 'python train.py'

# Job 101: eval — depends on job 100's checkpoint
weft run --needs output/model.pt:100 -m "Eval on MMLU" 'python eval.py'

# Job 100 fails. Retry as job 102, replacing version 100:
weft run --produces output/model.pt:100 -m "Fine-tune (retry)" 'python train.py --resume'
# Job 101 automatically picks up job 102's output — no re-editing needed

# Rental producer + downstream dependency (cross-host)
weft run --tag rental --gpu nvidia \
  --produces output/model.pt \
  -m "Train on rental GPU" 'python train.py'
# Job 1046 queued on a rental instance

weft run --gpu nvidia \
  --needs output/model.pt:1046 \
  --after wj1046 \
  -m "Eval" 'python eval.py'
# Downstream job waits until 1046 completes and artifact upload is available,
# then stages output/model.pt from cloud artifact storage before execution.

# Kill a job
weft run deepthought --kill 42
```

The `--produces`/`--needs` flags let you build multi-step pipelines without
hard-coding job IDs into downstream commands. When a producer fails and you
retry it with a version suffix (`:100`), all consumers keyed to that version
pick up the replacement automatically.

Dependency behavior differs by artifact location:
- Inventory-host artifacts are staged from the producing host when needed.
- Rental/ephemeral producer artifacts are staged from cloud artifact storage.
- If a dependency target is not ready yet (producer still running or artifact
  not uploaded), the downstream queued job stays deferred until it is available.

### weft estimation

Inspect or rebuild the command-level predictor models.

```bash
weft estimation status
weft estimation train [--if-schema-changed]
weft retrain [--if-schema-changed]
```

`status` reports whether the predictor is ready, rebuilding in the background,
or blocked by a schema mismatch. `train` and the top-level `retrain` alias
force an immediate rebuild from the configured job databases.

### weft data

Query data locality and request HF asset downloads onto specific hosts.

```bash
weft data where <asset-ref>
weft data fetch <asset-ref> --host <host> [--revision <rev>]
weft data requests [--host <host>]
```

Supported asset refs for `weft data` are:
- `hf:<repo-id>` for Hugging Face models
- `hf-dataset:<repo-id>` for Hugging Face datasets

`where` looks up the local inventory database and shows which hosts are known
to have the asset, including the last seen time and discovered cache path.

`fetch` creates a persistent download request record, runs the download on the
target host, rescans the HF cache, and updates the local inventory on success.
It uses `huggingface-cli download` and checks free space on the target HF cache
volume before starting the transfer. `localhost` is treated as a special case
and runs locally instead of over SSH.

`requests` shows past and current download requests recorded by the CLI. Use it
to audit which host was asked to download what, and whether the request
completed or failed.

**Flags:**
- `--json`: Emit JSON instead of a table
- `weft data fetch --host HOST`: Host that should cache the asset; may be an inventory host or `localhost`
- `weft data fetch --revision REV`: HF revision to download (default: `main`)
- `weft data requests --host HOST`: Filter recorded requests by host

**Examples:**
```bash
# Find where a model is currently cached
weft data where hf:EleutherAI/pythia-160m

# Download a model to a specific host
weft data fetch hf:EleutherAI/pythia-160m --host cool100

# Download a model onto the local machine
weft data fetch hf:EleutherAI/pythia-160m --host localhost

# Download a dataset revision to a host
weft data fetch hf-dataset:HuggingFaceFW/fineweb --host cool30 --revision main

# Show request history
weft data requests
weft data requests --host cool100
```

#### weft data publish

Publish a single local or remote file as a portable named asset. Consumers use
`--input asset:<name>` and Weft stages the file into the consumer workdir at
the recorded target path.

```bash
weft data publish <local-path> --name <name> [--target-path <path>]
weft data publish --host <host> <remote-path> --name <name> --target-path <path>
weft data publish <host>:<remote-path> --name <name> --target-path <path>
```

Remote publish requires `--target-path` because an absolute path on another
host does not define where the file should appear in the consumer workspace.
Directories are not supported.

### weft db

Manage the local jobs database (`~/.local/state/weft/jobs.db`, or
`$XDG_STATE_HOME/weft/jobs.db`): take consistent
snapshots, prune old snapshots.

Configuration remains under `~/.config/weft` (or `$XDG_CONFIG_HOME/weft`).
Durable local artifacts live under `~/.local/share/weft/artifacts` (or
`$XDG_DATA_HOME/weft/artifacts`). When changing any XDG base-directory
override, stop all Weft processes first and reinstall the launchd daemon so
the service records the same absolute overrides as the CLI.

#### Automatic pre-migration backups

Whenever `weft` opens a database that needs a schema migration, it first
writes a snapshot to `~/.local/state/weft/backups/jobs.db.pre-migration-v<N>-<timestamp>.db`
using SQLite's `VACUUM INTO`. The previous on-disk schema version is encoded
in the filename. The 3 most recent pre-migration backups are kept; older ones
are pruned automatically. Skipped on first init (empty DB) and on read-only
opens.

If a migration corrupts state, recover by stopping all `weft` processes and
copying the most recent pre-migration backup over `~/.local/state/weft/jobs.db`.

#### `weft db snapshot`

```bash
weft db snapshot [--out PATH]
```

Take a consistent point-in-time snapshot of the database. The destination is a
self-contained `.db` file (no `-wal`/`-shm` companions) that can be opened
directly by `sqlite3` or by analysis tools.

**Flags:**
- `--out PATH` — destination file (default:
  `~/.local/state/weft/backups/jobs.db.snapshot-<timestamp>.db`)

**Examples:**
```bash
# Default path under ~/.local/state/weft/backups/
weft db snapshot

# Custom destination, e.g. for offline analysis
weft db snapshot --out ~/Analysis/jobs-2026-04-30.db
```

For periodic backups, drive this from `launchd` or `cron` — weft does not
ship a backup daemon. A daily snapshot via launchd is one plist.

#### `weft db gc`

```bash
weft db gc [--keep N] [--apply]
```

Prune snapshots in `~/.local/state/weft/backups/`, keeping the N newest. Defaults
to dry-run; pass `--apply` to delete. Considers all `.db` files in that
directory, including pre-migration backups and manual snapshots.

**Flags:**
- `--keep N` — number of newest snapshots to keep (default: 10)
- `--apply` — actually delete (without it, only previews)

**Examples:**
```bash
# Preview what would be deleted (keep 10 newest)
weft db gc

# Keep only the 5 most recent and delete the rest
weft db gc --keep 5 --apply

# Delete every snapshot
weft db gc --keep 0 --apply
```

### weft sky

Submit or mirror SkyPilot managed jobs while keeping Weft as the local job
ledger.

```bash
weft sky submit [flags] <command>
weft sky import --project NAME [--cwd DIR] [--job wjID] [--task ID-OR-NAME] [--command COMMAND] <sky-job-id>
weft sky sync [--project NAME]
```

SkyPilot jobs get normal Weft job IDs (`wj...`) and appear in
`weft job list`, project views, `--unprocessed` queries, `weft log`, and
`weft cancel`. SkyPilot remains the external executor: it owns cloud resource
selection, cluster lifecycle, retries, and raw task execution. Weft does not
create rental instance rows, run its cloud agent, collect R2 outputs, or report
direct rental costs for these jobs.

`weft restart` / `weft retry` and `weft edit` refuse SkyPilot-backed jobs:
creating or editing a local attempt would not change the managed execution.
Move, unplace, launch-new, draft, pause, and resume controls are likewise not
available. Submit a new SkyPilot job instead. Local bookkeeping remains
available through the dedicated tag, project, and processed commands.

`weft log wj...` requests the bound SkyPilot task's retained output and exits;
add `--follow` to keep following it. Normal tail/follow views send the line
limit to SkyPilot; grep filters that bounded view incrementally while
following. `--full` and ranges fetch the complete retained stream. SkyPilot
uses zero to mean the entire retained log, so its follow view requires a
positive line limit. When a managed job has multiple tasks, Weft keeps the task ID in the
binding so logs and status refresh address the same task.

`weft kill` / `weft cancel` first refreshes the SkyPilot queue. Because
SkyPilot cancellation is managed-job-wide, Weft refuses a task-qualified
cancel when sibling tasks are visible or task membership cannot be confirmed.
After a successful request, the job remains in its last confirmed nonterminal
status with a secondary “cancel requested” marker until SkyPilot positively
reports a terminal outcome. The CLI and both job-list TUIs use this same path.

Weft does not collect artifacts from SkyPilot-backed jobs. `weft artifact sync`
refuses an explicitly selected SkyPilot job and excludes it from bulk
outstanding sync; use executor-managed storage for those outputs.

For Weft-owned rentals, queue jobs with `weft run --tag rental ...` and launch
them with `weft start instance`, `weft instance new`, or the autopilot. Weft
records the rental lifecycle in `wi...` instance rows and reports it through
`weft info` and `weft instance audit`; it does not currently expose a
`weft run --managed` flag that guarantees a dedicated one-rental lifecycle per
single job.

**Common flags:**
- `--project NAME`: associate the mirrored job with a Weft project
- `--cwd DIR`: working directory/source mount recorded for the job
- `--job wjID`: rebind an earlier unconfirmed submission to its original Weft job
- `--task ID-OR-NAME`: select one task within a multi-task managed job; the
  managed-job ID and task are matched together
- `--command COMMAND`: record the task command for a new import when the
  installed SkyPilot queue output does not expose it. Current SkyPilot releases
  require this for new imports; recovery with `--job` keeps the original Weft
  command.
- `-m, --message TEXT`: local Weft description
- `--gpu` / `--gpu-class`: SkyPilot accelerator class
- `--gpu-count N`: accelerator count
- `--gpu-mem GB`: currently rejected for SkyPilot submissions because
  SkyPilot task YAML has no standalone GPU-memory constraint; select a GPU
  class with `--gpu` instead
- `-e, --env KEY=VALUE`: environment variable in the generated task
- `-t, --tag TAG`: local Weft tag for filtering and processed bookkeeping

Examples:

```bash
weft sky submit --gpu a100 -m "SkyPilot training" 'python train.py'
weft sky sync
weft job list --unprocessed
weft log wj123
weft cancel wj123

weft sky import --project calibration --command "python train.py" 42

# Recover a submit whose outcome was unknown, without creating a second wj row.
# Repeating this exact command is safe.
weft sky import --job wj123 42
```

### weft artifact

Track and retrieve job outputs through a durable local artifact store.

Artifacts are declared by writing a manifest on the remote host. The CLI
syncs those files into `~/.local/share/weft/artifacts/` (or
`$XDG_DATA_HOME/weft/artifacts/`) so they survive
remote cleanup.

**Manifest format:**
```json
{
  "job_id": 2073,
  "artifact_root": ".",
  "artifacts": [
    {"name": "selectivity_results", "path": "output/selectivity_results.json"},
    {"name": "probe_ckpt", "path": "runs/roberta-base/probe.pt"}
  ]
}
```

**Environment variables available to job scripts:**
- `WEFT_JOB_ID` — the job ID
- `WEFT_ARTIFACT_MANIFEST` — path to the artifact manifest (default: `~/.cache/weft/artifacts/<job-id>.json`)
- `WEFT_ARTIFACT_ROOT` — artifact root directory (default: `.`)
- `RJ_JOB_ID`, `RJ_ARTIFACT_MANIFEST`, `RJ_ARTIFACT_ROOT` — legacy aliases (same values)

**Examples:**
```bash
# Sync artifacts for job 2073 into the local store
weft artifact sync wj2073

# Sync all outstanding artifacts across jobs
weft artifact sync

# List cached artifacts and cloud outputs
weft artifact list wj2073

# Retrieve by name or path
weft artifact get wj2073 selectivity_results -o ./results.json
weft artifact get wj2073 output/selectivity_results.json -o ./results.json

# Write artifact or output file to stdout
weft artifact get wj2073 selectivity_results -o -
weft artifact cat wj2073 selectivity_results | jq '.metric'

# Resolve latest job by tag
weft artifact get --tag exp-012 --latest selectivity_results -o ./results.json
```

#### Automatic output collection

Jobs automatically track files written to `output/` or `outputs/` in the
working directory. On successful completion, the runner records discovered files
in the completion record and syncs them back:

- **On-prem jobs**: Auto-syncs outputs via rsync, with no size cap. If you
  need to retrieve outputs explicitly, use `weft artifact sync`.
- **Cloud jobs**: All outputs are uploaded to R2 regardless of size. Both
  convention-based output directories (`output/`, `outputs/`) and explicit
  `--produces` paths are uploaded with no size gate. The per-rclone-invocation
  timeout scales with payload size at a 1 MB/s floor (5-minute minimum), so
  multi-GB checkpoints are supported.

Customize output directories, cloud image, and project-specific source
excludes in `.weft.toml`:

```toml
[outputs]
dirs = ["results/"]          # Default: ["output/", "outputs/"]

[sync]
exclude_dirs = ["data"]      # Additional project-specific source excludes

[cloud]
image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"  # Override default Docker image
min_driver = "535"                              # Optional NVIDIA driver floor
min_cuda = "12.9"                               # Optional CUDA compatibility floor
image_pull_secret = "ghcr.io"                   # Optional [registry] key

[cloud.image-overrides]
"train*.py" = "ghcr.io/example/train:cuda129"   # Script-specific image default
```

Use `weft artifact list <job-id>` to see discovered outputs and
`weft artifact sync <job-id>` to pull them from the remote host on demand.

#### Reclaiming local disk with `prune --local`

`weft artifact prune --local` (synonym: `weft artifact prune-local`) deletes
local files that can be restored later from R2 or the local artifact store,
freeing disk without losing anything recoverable. Defaults to dry-run; pass
`--apply` to delete.

**Flags:**
- `--local` — target local restorable files (required; the only supported mode today)
- `--apply` — delete files (without it, only previews)
- `--dir PATH` — directory scope (default: current directory)
- `--older-than DURATION` — restrict to files older than e.g. `7d`, `48h`, `7`
- `--since WHEN` — restrict to files modified since `YYYY-MM-DD`, RFC3339, or `"24h ago"`
- `--include-outputs` / `--include-artifacts` — toggle categories (both on by default)
- `--remove-empty-dirs` — clean up empty dirs after deletion
- `--recursive auto|on|off` — recurse into project subdirectories. Default
  `auto`: when `--dir` is itself a project (its path appears as a job's
  working_dir), prune that project; otherwise prune each child directory that
  has jobs. `on` always recurses; `off` always treats `--dir` as a single scope.

**Examples:**
```bash
# Preview what would be deleted in the current project
weft artifact prune --local --older-than 7d

# Apply the deletions
weft artifact prune --local --older-than 7d --apply

# Sweep every project under ~/code in one command (auto-recursion)
cd ~/code && weft artifact prune --local --apply
```

When run without `--apply` in an interactive terminal outside of a coding-agent
context, the command previews the deletions and then asks:

```
Apply these deletions? [y/N]
```

Answering `y` or `yes` applies the deletions immediately; any other answer (or
running in a non-TTY / agent context) prints the `weft artifact prune --local
--apply …` command needed to apply them later.

When the command spans more than one project (auto-recursion or `--recursive
on`), each project prints its own `== <project> ==` section and per-project
summary, followed by a final cross-project rollup:

```
== Total (3 projects) ==
Deleted: 42 file(s), reclaimed 4.2 GiB (4,512,233,984 bytes) across 3 projects
```

In dry-run mode the rollup reads `Would free: …` instead.

While scanning, a single-line progress indicator on stderr reports the current
phase (loading jobs, scanning R2 outputs, walking output directories). It is
cleared when the scan completes and is suppressed on non-TTY output.

The command:
- Creates a job ID and adds it to the remote queue (or starts immediately with `-i`)
- Queue runner schedules queued jobs in FIFO order (subject to CPU allotments)
- Saves job metadata and logs to `~/.cache/weft/logs/` on the remote host
- Records the job in a local SQLite database (`~/.local/state/weft/jobs.db`)
- Captures exit code when job completes
- Sends Slack notification on completion (if configured)
- Returns immediately (non-blocking)
- Prints the job ID and instructions for starting immediately or monitoring

### weft plan submit

Submit a YAML job execution plan that can mix one-off jobs, parallel groups,
and queue-backed series.

```bash
weft plan submit plan.yaml
weft plan submit --host studio plan.yaml   # provide default host via CLI
weft plan submit - < generated-plan.yaml   # read from stdin / heredoc
```

Plan files must start with `version: 1` to opt into the current schema and
remain compatible with future releases.

Plan files support an optional `kill` list, single `job` entries, `parallel`
groups, and `series` groups. Every block and job may declare `id`, `alias`,
`depends_on`, and `continue_on_failure`. The CLI resolves these references into
a DAG, auto-generating IDs (`block0`, `block0.job0`, etc.) when missing, so you
can declare multi-phase pipelines in a single YAML file. Provide `--host <name>`
to supply a default host for jobs that omit it, and add `--wait <duration>` to
keep the CLI around and report which jobs finished. See
[Job Plans](job-plans.md) for the full schema plus dependency
examples.

Inspect or lint a plan without running it:

```bash
weft plan validate plan.yaml
weft plan show --ids plan.yaml   # show generated IDs, aliases, hosts, deps
```

> **Agents welcome:** Weft (and the plan syntax in particular) was
> designed for coding agents as well as humans. The YAML shape is easy for an
> agent to emit directly from a prompt, so consider giving your agent runtime a
> skill/instruction that invokes `weft plan submit` with generated plans.
> This lets automated assistants spin up, chain, and monitor jobs using the same
> dependency and queueing logic described below.

### weft job status

Check the status of one or more jobs by ID.

```bash
weft job status <job-id>...
weft job status --wait wj42         # block until the job finishes
weft job status --wait --wait-timeout 30m wj42
weft job status --wait wj42 wj43 wj44   # wait for all (exits 0 only if all succeed)
```

**Job ID syntax:**
- Single IDs: `wj42` (also accepts `42`)
- Ranges: `wj42:wj45`, `wj42:45`, `42:wj45` (inclusive)
- Mixed: `wj42 wj50:53 60` (expands to 42, 50, 51, 52, 53, 60)

Duplicate IDs are automatically removed with a warning.

**Exit codes (single job only):**
- `0`: Job completed successfully
- `1`: Job failed or error
- `2`: Job is still running
- `3`: Job not found

**Examples:**
```bash
weft job status wj42               # Check status of job wj42
weft job status wj42 wj43 wj44     # Check multiple jobs
weft job status wj100:wj105        # Check jobs wj100 through wj105
weft job status wj100...wj105      # Alternative range syntax
weft job status wj42,wj43,wj44     # Comma-separated IDs
```

This command:
- First checks the local database for terminated jobs
- Only queries the remote host if the job is still running
- Updates the database if status has changed
- With no job IDs, reads cached database state by default; add `--sync` to
  refresh all active on-prem hosts before printing the overview.
- Use `--wait` (with optional `--wait-timeout`) to block until jobs finish.
  The command exits with `0` only if every waited-on job succeeds.

### weft info / weft job info

Show detailed job metadata by ID.

```bash
weft info <job-id>...
weft job info <job-id>...
```

`weft info` includes the selected target and a `Lifecycle:` line. For
rental-backed jobs, that line reports the lifecycle evidence Weft has recorded:
`target_kind=rental`, provider, `wi...` instance ID, provider instance ID,
teardown policy, and teardown start/completion timestamps. Use
`weft instance audit <job-id>` when you also need a live provider cleanup check.

For a running rental job, `Started` and `Elapsed` are marked provisional because
the completion record can reconcile the command start time. Cost is marked
provisional until rental teardown; a live instance-total or shared-job estimate
is not the final job cost to record in an experiment.

### weft bug

Record and inspect Weft bug reports. By default, `weft bug` uses GitHub issues
through the `gh` CLI and uses GitHub issue numbers (`#123`). Configure the
legacy local tracker to keep reports in `~/.local/state/weft/bugs.db`, a small
SQLite database that is separate from the main jobs database and uses `wb<id>`
identifiers:

```toml
[bug]
tracker = "local"
```

You can also set it from the CLI:

```bash
weft bug tracker local
weft bug tracker github
```

Bug reports are for Weft runtime defects and invariant violations, not for
normal job failures or feature requests.

Older bug records that were written to `jobs.db` are imported into `bugs.db`
the first time `weft bug` opens the standalone bug database in local-tracker
mode. Imported records keep their original detail and notes, and receive an
import note with the legacy bug id.

```bash
weft bug report --title "runner pending job is missing queue payload" \
  --scope infrastructure \
  --kind invariant \
  --fingerprint "queue.missing_payload:studio" \
  --job wj2454 \
  --host studio \
  --detail "raw maintainer details"

weft bug note '#123' "additional context from a later observation"
printf '%s\n' "context with 'quotes'" | weft bug note --stdin '#123'
weft bug list
weft bug list --all
weft bug show '#123'
weft bug close '#123' --reason "fixed in e95f1f4e"
weft bug reopen '#123'
```

`report` returns a bug number. If another open bug has the same fingerprint,
Weft updates that bug instead of creating a duplicate. If the fingerprint
belongs to a closed bug, `report` fails and tells you to reopen that bug or
choose a different fingerprint. The local tracker also increments an occurrence
count and refreshes the stored context.

Important fields:

- `--scope`: broad user-facing category such as `job-specific`, `network`,
  `infrastructure`, or `other`
- `--kind`: defect category such as `bug` or `invariant`
- `--fingerprint`: stable dedupe key; choose one that groups repeated sightings
  of the same defect
- `--summary`: short user-facing explanation
- `--detail`: raw maintainer evidence, including internal paths or invariant
  failures that should not be shown as normal user remediation advice

When Weft detects an internal invariant failure that might otherwise mislead
users, status output may show a concise `Weft bug #123` or `Weft bug wb123`
message instead of raw implementation details. Use `weft bug show <id>` for the
maintainer record.

### weft job inspect / diff

Normalize job metadata for postmortem inspection, or compare two jobs.

```bash
weft job inspect <job-id> [--json]
weft job diff <job-a> <job-b> [--json]
```

`inspect` prints the behaviorally relevant submission and execution fields:
command, working directory, host / launch target, inputs, outputs, resource
constraints, tags, redacted environment variables, placement metadata, and
attempts.

`diff` compares the same normalized fields between two jobs. This is useful for
mining high-churn retry sequences where the command stayed similar but metadata
changed, such as `HF_HOME`, `HF_HUB_OFFLINE`, input declarations, GPU
constraints, or placement target.

For jobs with declared `hf:` / `hf-dataset:` inputs, Weft's managed staging
download may contact Hugging Face before the job command starts. Runtime offline
flags such as `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`, and
`HF_DATASETS_OFFLINE=1` are preserved for the job process, but Weft overrides
them only for that staging subprocess so declared inputs can be provisioned on
rentals with Hub access.

**Examples:**
```bash
weft job inspect wj1877 --json
weft job diff wj1876 wj1877
weft job diff wj1876 wj1877 --json
```

### weft job anomalies / churn / recommend

Mine recent job history for review targets and improvement suggestions.

```bash
weft job anomalies [--recent N] [--json]
weft job churn [--recent N] [--json]
weft job recommend [--recent N] [--json]
```

`anomalies` lists jobs that look worth reviewing: failed/dead/canceled jobs,
non-zero exits, retries, multiple attempts, suspicious cloud outcomes, and
common metadata gaps such as Hugging Face jobs without explicit cache or
offline-mode overrides.

The output also includes a **Disk telemetry anomalies** section that scans
the entire job-time-series history (not just the recent-N window the
per-job mining uses) for `disk_used_bytes` / peak-disk-used samples above a
2 TB plausibility bound. Such samples are almost always the result of an
agent-side `statfs` probe misreporting on an overlay/fuse container root,
and they will inflate disk requests for any future job whose command
signature matches the affected source job — see
[cloud-instance-debugging.md](../guides/cloud-instance-debugging.md#disk-request-implausibly-large-infra_failure-no-instances-available-with-enough-disk-space)
for the failure pattern this prevents.

`churn` groups recent jobs that look like iterations of the same script or
command. Use the suggested `weft job diff` and `weft source diff` commands to
compare adjacent jobs in a group.

`recommend` turns the anomaly and churn signals into triage suggestions for
Weft or workflow improvements. See
[Iterative Weft Improvement](../guides/iterative-improvement.md) for the review
loop.

**Examples:**
```bash
weft job anomalies --recent 100
weft job churn --recent 200
weft job recommend --recent 100
```

### weft job cost

Show cost for each job that has a recorded cost, with instance context.

```bash
weft job cost
weft cost jobs      # Alias
```

Output is one block per job (matching `weft job status` style) showing the job
cost, the instance it ran on, how many jobs shared that instance, the instance's
total cost, and per-job overhead. Useful for attributing cloud spend to
individual jobs, especially when a job was the sole occupant of an instance.

### weft job list

Query and search job history from the local database. On an interactive
terminal, table output opens the live job-list TUI. When stdout/stdin are not
interactive (for example `weft job list | grep failed`) it renders one plain
table frame and exits.

```bash
weft job list [flags]
weft list jobs [flags]      # Alias
weft jobs list [flags]      # Alias
```

**Flags:**
- `--running`: Show only running jobs
- `--completed`: Show only completed jobs
- `--queued`: Show only queued jobs
- `--dead`: Show only dead jobs
- `--failed`: Show only failed jobs (`failed`, `dead`, or completed with non-zero exit code)
- `--processed`: Show only jobs with the reserved `processed` tag
- `--unprocessed`: Show only jobs without the reserved `processed` tag
- `--rental`: Show jobs tagged for rental placement or assigned to rental instances
- `--inventory`: Show inventory-only jobs and jobs assigned to inventory hosts
- `--status STATUS`: Filter by status (`running`, `completed`, `queued`, `dead`, `processed`, `unprocessed`)
- `--host HOST`: Filter by host (replaces old `check <host>` command)
- `--search QUERY`: Search by description or command
- `--tag TAG`: Filter by tag (can be repeated)
- `--limit N`: Limit results (default: 50; `0` for no limit). Results are
  ordered newest first, so the most recently submitted jobs are the ones
  kept when the limit applies. When it drops rows, the count is reported on
  stderr. The TUI and `weft project jobs` instead order active jobs first.
- `--show ID`: Show detailed info for a specific job
- `--cleanup DAYS`: Delete jobs older than N days
- `--sync`: Sync job statuses from remote hosts before listing
- `--no-sync`: Use cached database state; this is the default for one-shot
  plain output and overrides `--sync`
- `--tui`: Force the live interactive list
- `--plain`: Force one-shot plain output
- `--watch`: Deprecated compatibility alias for `--tui`
- `--group-by status`: Group table output by status sections (`Running`, `Queued`, `Completions`, `Failures`, `Killed/Canceled`)

`--group-by status` only supports table/plain output; combining it with
`--format json` or `--format tsv` returns an error.
When combined with `--unprocessed`, grouped views omit `canceled` jobs (which
were already handled by the agent) but still include `killed` jobs.
One-shot list output reads cached database state by default. Use `--sync` when
you want the command itself to probe remote hosts and cloud state before
returning; the interactive TUI refreshes live state in the background.

**Examples:**
```bash
weft job list                          # Recent jobs
weft job list --running                # Running jobs
weft job list --running --sync         # Running jobs (sync first)
weft job list --host deepthought       # Jobs on deepthought
weft job list --failed                 # Failed jobs
weft job list --unprocessed            # Jobs missing the processed tag
weft job list --rental                 # Rental-tagged or rental-assigned jobs
weft job list --inventory              # Inventory-only or inventory-assigned jobs
weft job list --tag exp-012            # Jobs with a tag
weft job list --status unprocessed     # Jobs missing the processed tag
weft job list --search training        # Search jobs
weft job list --plain | grep failed    # One-shot output for shell pipelines
weft job list --group-by status        # Grouped live/status view on a TTY, plain when piped
weft job list --tui                    # Force the live list
weft job list wj12::wj14                   # List jobs 12 through 14
weft job list wj12...wj13                  # Alternative range syntax
weft job list wj12,wj13,wj14                 # Comma-separated IDs
weft job list --show 42                # Job details
weft job list --cleanup 30             # Remove old jobs
```

### weft session unprocessed

Print the current agent session's recent unprocessed terminal-job inbox as a
versioned JSON object. Use `weft session unprocessed`, optionally with
`--project augur`.

The command resolves the session through Weft's configured submitter-session
environment order and matches the stored id exactly. Its `scope.state` is one
of `scoped_nonempty`, `scoped_empty`, or `unscoped`. An unscoped result means
this process has no attributable session id; its zero counts do not assert
that the session inbox is empty. Each job includes its effective status,
project, age, and the timestamp field used as age provenance.

### weft project jobs

List jobs grouped by project instead of as one flat table.

```bash
weft project jobs [flags]
```

`weft project jobs` uses the same query flags as `weft job list`, but renders
one block per project. Each block shows the project name, directories, and the
matching jobs for that project.

**Examples:**
```bash
weft project jobs
weft project jobs --unprocessed
weft project jobs --failed --tag exp-012
weft project jobs --host cool30
```

### weft project watch

Watch active and recent jobs grouped by project.

```bash
weft project watch [flags]
```

In an interactive terminal this defaults to a read-only TUI. Otherwise it
polls the database, printing a grouped snapshot of running, queued, and
recent terminal jobs every refresh interval, and exits once all jobs reach a
terminal state. Use `--follow` to keep printing snapshots even when nothing
is active.

**Flags:**
- `--tui`: Force TUI mode
- `--plain`: Force plain text output
- `--sync`: Perform a full sync before loading data
- `--no-sync`: Skip syncing before loading data
- `-f`, `--follow`: Keep printing snapshots even when nothing is active
- `--transitions-only`: Emit one line per status change instead of full snapshots (suppresses the seed snapshot)
- `--jsonl`: Emit JSON Lines output (one JSON object per line). Composes with `--transitions-only` for an event stream; without it, emits one snapshot object per poll
- `--until-any-terminal`: Exit on the first terminal status transition (mutually exclusive with `--follow`)
- `--recent DURATION`: Window for recent terminal jobs (default: `24h`)

The `--transitions-only`, `--jsonl`, and `--until-any-terminal` flags are
also available on `weft watch` and `weft job watch`. See [JSON event
schema](#watch-json-schema) below for the stable v1 fields.

**Examples:**
```bash
weft project watch
weft project watch --plain
weft project watch --plain --follow
weft project watch --recent 48h

# Agent-friendly: stream JSON events, exit on first terminal transition.
weft project watch --plain --transitions-only --jsonl --until-any-terminal

# Human-readable transition stream, keep going after first terminal.
weft project watch --plain --transitions-only --follow
```

<a id="watch-json-schema"></a>
**JSON event schema (v1, stable):**

Transition event (`--jsonl --transitions-only`):
```json
{"type":"transition","timestamp":"2026-04-27T10:30:00Z","job_id":"wj1531","id":1531,"status":"completed","prev_status":"running","host":"cool30","project":"structural-probes","instance_id":1542,"exit_code":0}
```

Snapshot event (`--jsonl` without `--transitions-only`):
```json
{"type":"snapshot","timestamp":"...","jobs":[{"job_id":"wj1531","id":1531,"status":"running","host":"cool30","project":"structural-probes","instance_id":1542,"exit_code":null}]}
```

Stable v1 fields are committed: `type`, `timestamp` (RFC3339 UTC), `job_id`
(canonical `wj<id>`), `id` (numeric), `status`, `prev_status` (transitions
only — empty string for jobs first seen after the seed iteration), `host`,
`project`, `instance_id` (nullable), `exit_code` (nullable). Additional
fields may be added in future versions; existing fields will not be renamed
or removed without a major-version bump.

### weft job tag

Attach or remove tags on jobs stored in the local database.

```bash
weft job tag add <job-id>... <tag>
weft job tag rm <job-id>... <tag>
```

**Aliases:** `weft tag add`, `weft tag rm` (deprecated top-level forms)

**Examples:**
```bash
weft job tag add wj42 exp-012
weft job tag rm wj42 exp-012
weft job tag add wj42 wj43 wj44 rental    # tag multiple jobs at once
```

Most tags are user-defined. For the catalog of tags the scheduler and runner
treat specially (`rental`, `inventory`, `benchmark-isolation`, `exclusive`,
`interruptible`, `cpu-intensive`, `provider:<name>`) and the legacy
aliases `cloud`, `on-prem`, `preemptible`, `compute-intensive`, see the
[Placement guide § Reserved tags](../guides/placement.md#reserved-tags).
`cpu-intensive` also affects rental policy: existing rentals must meet the
`WEFT_COMPUTE_CPU_CORES` effective-CPU floor, and automatic new rentals need at
least a 30-minute estimated completion-time advantage over on-prem placement.

### weft job mark-processed / mark-unprocessed

Mark jobs as processed or unprocessed by adding or removing the reserved
`processed` tag.

```bash
weft job mark-processed <job-id>...
weft job mark-unprocessed <job-id>...
```

**Aliases:** `weft mark-processed`, `weft mark-unprocessed` (deprecated
top-level forms)

**Examples:**
```bash
weft job mark-processed wj42
weft job mark-unprocessed wj42 wj43
weft job list --unprocessed
```

### weft sync

Sync job statuses from all remote hosts with running jobs.

```bash
weft sync [flags]
```

**Flags:**
- `-v, --verbose`: Show detailed progress

Automatically finds hosts with running jobs and updates their status in the local database. Connection failures are silently ignored (unreachable hosts are skipped).

**Examples:**
```bash
weft sync              # Sync all hosts
weft sync --verbose    # Show progress
```

### weft sync inspect

Inspect the local source snapshot using the same exclude rules as `weft sync`
source uploads and campaign source tarballs.

```bash
weft sync inspect [dir] [flags]
```

**Flags:**
- `--top-files N`: Show the N largest included files
- `--top-dirs N`: Show the N largest included top-level directories
- `--json`: Emit machine-readable JSON
- `--show-excludes`: Print the effective exclude patterns

**Examples:**
```bash
weft sync inspect
weft sync inspect ~/code/project
weft sync inspect --show-excludes
weft sync inspect --json
```

### weft source

Inspect the exact source snapshot uploaded to R2 for a cloud job attempt.

```bash
weft source ls <job-id> [prefix] [flags]
weft source inspect <job-id> [flags]
weft source cat <job-id> [path] [flags]
weft source diff <job-a> <job-b> [path] [flags]
```

**Flags:**
- `--attempt N`: Use a specific attempt number (default: latest)
- `--json`: Emit the versioned `weft.source.inspect.v1` record (`inspect` only)
- `--attempt-a N`: For `diff`, use a specific attempt for the first job
- `--attempt-b N`: For `diff`, use a specific attempt for the second job

When `path` is omitted, `cat` tries to print the script referenced by the job
command. Pass an explicit path if the command references multiple scripts or no
script can be inferred.

`source inspect` compares the submit-time closure with the identity observed by
the worker, but only when both records use the same identity kind. Its verdict
distinguishes verified, mismatch, unavailable R2 objects, legacy attempts that
cannot be compared, and attempts still awaiting worker verification. It also
reports dispatch mode, agent version, verification time, and every pinned root.

For pinned inventory jobs, `source ls` reads each root tarball and diverted blob
entry named by the stored manifest. Legacy rsync attempts have no immutable file
inventory, so the command reports that limitation explicitly instead of
inspecting the current checkout.

**Examples:**
```bash
weft source inspect wj1443
weft source inspect wj1443 --json
weft source ls wj1443
weft source ls wj1443 scripts/
weft source cat wj1443
weft source cat wj1443 scripts/exp141_pythia_checkpoint_sweep.py
weft source cat wj1443 --attempt 4 scripts/train.py
weft source diff wj1876 wj1877
weft source diff wj1876 wj1877 scripts/exp_002_gain_vs_freeze.py
```

### weft log

View the full log file for a job.

```bash
weft log <job-id> [flags]
weft job log <job-id> [flags]   # Alias
weft log --ops [flags]
weft log --events [flags]
```

**Flags:**
- `-f, --follow`: Follow log in real-time (like `tail -f`)
- `-n, --lines N`: Number of lines to show (default: 50 for running jobs)
- `--tail N`: Alias for `--lines`
- `--from N`: Show lines starting from line N
- `--to N`: Show lines up to line N
- `--grep PATTERN`: Filter lines matching pattern
- `--full`: Show the entire log (`--from 1`)
- `--attempt N`: Show the log from a specific recorded attempt
- `-t, --timeout DURATION`: SSH timeout for slow connections
- `--sync`: Perform a full sync before showing the log
- `--no-sync`: Skip status and cloud-log sync
- `--ops`: Show the operations log instead of a job log
- `--job ID`: Filter `--ops` output by job ID
- `--host HOST`: Filter `--ops` output by host
- `--op NAME`: Filter `--ops` output by operation name
- `--since DURATION`: Filter `--ops` output to a recent time window
- `--errors`: Show only `--ops` entries that include an error
- `--events`: Show structured lifecycle events instead of a job log
- `--kind PREFIX`: Filter `--events` output by event kind or prefix
- `--launch ID`: Filter `--events` output by launch / instance ID
- `--stats`: Show aggregate statistics for `--events`

**Examples:**
```bash
weft log wj42           # Full log (completed jobs) or last 50 lines (running)
weft log wj42 -f        # Follow (like tail -f)
weft log wj42 -n 100    # Last 100 lines
weft log wj42 --tail 30 # Alias for --lines
weft log wj42 --from 100 --to 200  # Lines 100-200
weft log wj42 --from 500           # From line 500 onwards
weft log wj42 --to 100             # First 100 lines
weft log wj42 --grep error         # Lines containing "error"
weft log wj42 -f --grep epoch      # Follow, filter for "epoch"
weft log wj42 --full               # Entire log (explicit)
weft log wj42 --attempt 2          # Log from attempt #2
weft log --ops --job wj42        # Operations for a job
weft log --ops --host vastai:17  # Operations for a rental instance
weft log --events --kind relaunch
```

**Notes:**
- Completed jobs show the full log by default; running jobs show the last 50 lines
- Use `-n` to override the default for either case
- `--from`/`--to` cannot be used with `-n`/`--lines`
- `--follow` cannot be used with `--to`
- `--grep` can be combined with any other option
- `--ops` and `--events` do not take a job ID positional argument
- A finite default read normally selects the current attempt. If the current
  retry is queued and has not started, it instead shows the most recent
  attempt that started and prints the selected attempt number. `--follow`
  never makes this substitution because it follows only the current attempt.
- Use `weft info <job-id> --all-attempts` to inspect retry history, then
  `weft log <job-id> --attempt N` to select an earlier attempt explicitly.
- A missing or empty log for a started attempt is authoritative; Weft does not
  search still earlier attempts based on whether a log object exists.

### weft telemetry

Show telemetry for a job attempt. The latest attempt is selected by default.

```bash
weft telemetry <job-id> [job-id...]
weft job telemetry <job-id> [job-id...]   # Alias
```

The command reads the latest attempt's synced telemetry samples, prints the
time range, and summarizes GPU utilisation, temperature / clock data when
available, plus per-GPU memory, activity, and SM / memory clock ranges from the
richer telemetry stream. JSON output records per-device clock minima, maxima,
and means under `summary.gpus`.

For job diagnostics, `--json --samples` adds the rich per-sample records under
`raw_samples`. Use `--run <attempt-id>` to inspect a specific attempt of a
single job. The command rejects attempt IDs that do not belong to that job.

**Flags:**
- `--json`: Emit machine-readable JSON
- `--samples`: Include raw samples in JSON output (requires `--json`)
- `--run <attempt-id>`: Select a specific attempt (single job only)

**Examples:**
```bash
weft telemetry wj42
weft telemetry wj42 wj43
weft telemetry wj42 --json
weft telemetry wj42 --json --samples
weft telemetry wj42 --run 314 --json --samples
```

### weft job restart

Requeue one or more jobs using the same job ID.

```bash
weft job restart [job-id]...
```

For terminal jobs (`killed`, `dead`, `failed`, `canceled`, `completed`), this
archives the prior run attempt and sets the job back to `queued`.

For jobs that are already `queued`, `restart`/`retry` is a no-op unless you pass
override flags such as `--gpu`, `--gpu-class`, `--gpu-mem`, or
`--gpu-mem-strict`.

Use `--unplaced` to retry all currently queued unplaced jobs:

```bash
weft retry --unplaced
```

### weft retry

Alias for `weft job restart`.

```bash
weft retry [job-id]...
weft job retry [job-id]   # Alias
```

GPU override flags are supported:

```bash
weft retry wj548 wj549 --gpu nvidia>=24GB
weft retry wj548 wj549 --gpu-class nvidia --gpu-mem 24
weft retry wj548 wj549 --gpu-class nvidia --gpu-mem 24 --gpu-mem-strict
```

Memory embedded in `--gpu` (for example `a100>=80GB`) is treated as an
exact hardware capacity floor. It does not receive the `+2GB` workload
headroom that applies to separate `--gpu-mem` values.

`retry` re-syncs project-derived inputs/outputs and re-reads `[tool.weft]`
script metadata for GPU defaults. Explicit `retry` flags (`--gpu`,
`--gpu-class`, `--gpu-mem`, `--gpu-mem-strict`) take precedence over script
metadata.

For terminal jobs, `retry` preserves a concrete destination when one is known:
an explicit command target, stored `placement_host`, current inventory host, or
already-live rental instance. Explicit host placement wins over tags, including
stale `rental` tags from older attempts. If no concrete target exists, the fresh
attempt stays unplaced and requires autopilot or an explicit move/run command to
choose a destination.

### weft job move

Move one or more queued jobs to a different destination. Jobs that are not
queued are skipped with a warning. Unplaced jobs can be moved (placed)
directly.

```bash
weft job move <job-id>... <destination>
weft job move [<job-id>...] --to <destination>
weft job move --from <instance> --to <destination>
```

**Destinations:**
- `<hostname>` — on-prem inventory host (e.g., `cool100`, `studio`)
- `wi<N>` — existing cloud instance (e.g., `wi872`)
- `new` / `create` — launch new instance(s) grouped by GPU affinity
- `distinct` — synonym for `--each --to new` (separate new instance per job)

**Flags:**
- `--each` — with `new`/`create`/`distinct`: launch a separate instance per job
- `--to <destination>` — destination as a flag instead of final positional argument
- `--from <instance>` — select queued jobs from a source cloud instance (e.g., `wi872`)
- `--project <name>` — select all eligible queued jobs in the named project

Exactly one selector mode is required:
- Job IDs (`<job-id>...`)
- `--project <name>`
- `--from <instance>`

**Examples:**
```bash
weft job move wj42 cool100              # Place/move job 42 to cool100
weft job move wj43 wi872                # Submit job 43 to instance wi872
weft job move wj44 new                  # Launch one new instance for job 44
weft job move wj44 wj45 wj46 new       # Launch instance(s) for jobs 44-46
weft job move wj44:wj46 --each new     # Separate new instance per job
weft job move wj44:wj46 --to distinct  # Synonym for --each --to new
weft job move --project myproj --to new
weft job move --from wi872 --to wi900
```

### weft instance new

Launch one new cloud instance and start it with a balanced initial queue of
compatible queued jobs. Unlike `weft instance launch` / `weft place`, this can
include jobs that are already queued on existing cloud instances. Weft opens
move intents before launch so autopilot leaves those jobs alone while the new
instance proves it has accepted the work.

```bash
weft instance new [job-id]... [flags]
weft new instance [job-id]... [flags]   # Verb-noun alias
```

Without arguments, all eligible queued rental jobs are considered. Use
positional job IDs, `--jobs`, or `--project` to narrow the scope. The command
selects one anchor job, adds compatible queued jobs that fit the same launch
group, launches a single instance with that complete job list, and waits for
`agent_ready` before confirming the move intents.

By default, Weft previews the selected anchor, job list, and offer, then asks
for confirmation. Use `--yes` for non-interactive launch or `--dry-run` to
preview without launching.

**Flags:**
- `--jobs IDS`: Comma-separated job IDs/ranges to consider
- `--project NAME`: Restrict queued jobs to a project
- `--strategy cheap|fast|fastest`: Offer selection strategy (default: `fastest`)
- `--min-survival FRACTION`: Minimum Weft learned end-to-end survival probability for offers (default: `0.4`; distinct from provider reliability)
- `--dry-run`: Preview the selected anchor, job list, and offer without launching
- `--yes`: Launch without interactive confirmation
- `--wait`: Wait for `agent_ready` before confirming move intents (default: true)
- `--timeout DURATION`: Maximum `agent_ready` wait (default: `20m`)

**Examples:**
```bash
weft instance new
weft instance new --yes
weft instance new --project myproj --yes
weft instance new wj42 wj43 --dry-run
weft new instance --jobs wj42:wj45 --yes
```

### weft start instance

Launch cloud instances for queued unplaced jobs. This is the primary command
for placing jobs that could not be scheduled on on-prem inventory (for example,
jobs tagged `rental` or jobs whose GPU constraints no inventory host satisfies).

```bash
weft start instance [job-id]... [flags]
weft start instances [job-id]... [flags]  # Plural alias
weft instance launch [job-id]... [flags]  # Equivalent
weft place [job-id]... [flags]            # Short alias
```

> **Skip this if an autopilot is running.** Run `weft autopilot status` first.
> If it reports `running`, the autopilot will pick up unplaced jobs and launch
> instances for them on its own — an explicit launch won't be faster, only
> more controllable. Launch manually when you want to choose offers, cap
> spend, or parallelize differently than the autopilot would. See
> [Coordinating with the autopilot](../guides/instances.md#coordinating-with-the-autopilot)
> for details.

Without arguments, all queued unplaced jobs are considered. You can narrow the
selection with positional job IDs, `--project`, or `--all` (explicit form of
"every queued unplaced job").

**Flags (selection):**
- `--project NAME`: Only jobs from the named project
- `--all`: All queued unplaced jobs (explicit; same as no arguments). Cannot be combined with job IDs or `--project`.
- `--status queued|unplaced`: Filter by effective status (accepts `queued` or the alias `unplaced`)

**Flags (launch mechanics):**
- `--yes`: Skip the interactive confirmation
- `--watch` / `--no-watch`: Explicitly enter or skip watch mode after launching
- `--dry-run`: Preview the plan without launching
- `--max-spend USD`: Hard dollar cap for the launch
- `--max-time DURATION`: Hard wall-clock cap
- `--grace-period DURATION`: Grace period after failure (default: `5m`)
- `--runpod-cloud-type community|secure`: Override RunPod cloud type for this launch. The default comes from `[runpod] cloud_type`, or `community` when unset.
- `--strategy cheap|fast|fastest`: Offer selection strategy (default: `cheap`)
- `--min-survival FRACTION`: Minimum Weft learned end-to-end survival probability for offers (default: `0.4`; `0` disables; distinct from provider reliability)
- `--distinct-machines`: Place selected jobs on different provider physical machines. Vast.ai is filtered before create; RunPod is checked after pod create/readback.
- `--avoid MACHINE|wiID|wjID`: Exclude a physical machine when using `--distinct-machines`; repeatable and comma-separated. Raw unqualified machine ids are Vast.ai-compatible; `runpod/<machine_id>` and instance/job ids use provider-qualified machine ids.
- `--affinity MACHINE|wiID|wjID`: Require placement on the same Vast.ai physical machine; repeatable and comma-separated
- `--plain` / `--tui`: Force output mode

**Examples:**
```bash
weft start instance                 # All queued unplaced jobs (interactive)
weft start instance wj42 wj43       # Only these jobs
weft start instance --project myproj # Unplaced jobs from one project
weft start instance --yes --watch   # Launch everything, then watch
weft start instance --dry-run       # Preview without launching
weft start instance --strategy fastest # Prefer fastest GPUs
weft start instance --min-survival 0 # Disable survival floor
weft start instance --runpod-cloud-type secure # Request RunPod secure cloud
weft start instance --affinity wj789 --jobs wj790 # Run where wj789 last ran
```

A launch is recorded as a **campaign** — a batch row grouping the instances
provisioned together. Per-instance operations live under `weft instance …`;
batch-level operations live under `weft campaign …` (see below).

### weft instance watch / list / status / ssh / terminate / cordon

Per-instance commands.

```bash
weft instance watch [instance-id]                       # Watch active instances (or one)
weft instance list                                      # List instances
weft instance status <instance-id>                      # Single instance details
weft instance audit <job-id>                            # Read-only cleanup audit for a job
weft instance cost                                      # Rate, duration, and actual cost per instance
weft instance ssh <instance-id>                         # SSH in
weft instance terminate <instance-id>                   # Destroy a single instance
weft instance cordon <instance-id> [--reason "..."]    # Stop new jobs from landing here
weft instance uncordon <instance-id>                    # Clear the cordon flag
weft instance mark-weft-bug <instance-id> --detail "…" # Exclude a confirmed Weft-caused termination from survival training
```

`weft instance audit <job-id>` reports the rental rows associated with a job,
their provider IDs, cached teardown timestamps, live provider lookup status, and
a `remaining` / `gone` / `unknown` resource summary. It is read-only; use
`weft cleanup --provider ... --force` or `weft instance terminate ...` for
actual cleanup.

Cordoning lets the current job finish without the autopilot routing new
work to the instance — useful when an instance has a stale agent or you
want to drain just one instance without pausing the autopilot globally.

### weft instance mark-credit-exhausted

Reclassify already-terminated cloud instances to record that they failed
because the provider account ran out of credit. The reclassified rows are
excluded from the bidding survival model so credit-exhaustion failures
don't poison machine/SKU/region priors.

Use this after an incident where the provider destroyed running
instances for non-payment. Weft has no programmatic signal that
distinguishes those from generic provider failures at the moment of
destruction, so the operator labels them retroactively.

For a confirmed Weft-caused termination, use `weft instance mark-weft-bug`
instead. It retains the lifecycle and cost record but changes the termination
reason to `weft_bug`, which excludes the row from survival training:

```bash
weft instance mark-weft-bug wi42 \
  --detail "false watchdog teardown; fixed in revision abc123"
```

```bash
# Auto-detect the most recent incident on any provider.
weft instance mark-credit-exhausted --auto --dry-run
weft instance mark-credit-exhausted --auto

# Reclassify all credit-signature failures in a time window.
weft instance mark-credit-exhausted --since 24h --dry-run
weft instance mark-credit-exhausted --since 24h

# Reclassify rows in the window even when the detail string has no
# credit signature (use when you know every failure in the window was
# credit-related).
weft instance mark-credit-exhausted --since 6h --include-generic

# Reclassify by ID.
weft instance mark-credit-exhausted wi1 wi2 wi3
```

**Selection modes** (mutually exclusive):

- **Positional IDs**: always reclassified if eligible — the operator has
  named them explicitly.
- **`--since DURATION`**: failed/canceled launches in the window. By
  default only rows whose `termination_detail` matches a
  credit-exhaustion signature are included (explicit credit-out phrases
  from the provider, plus generic "provider dead" rows that cluster in
  time with the explicit signals, ~15 min before / ~5 min after). Pass
  `--include-generic` to reclassify all generic-reason rows in the window.
- **`--auto`**: auto-detect the most recent credit-exhaustion incident
  on any provider by finding a mass-destroy burst followed by a silent
  gap until a same-provider recovery launch came up. Provider-scoped:
  silence and recovery on one provider can never satisfy a burst on
  another. Tunables:

  - `--auto-lookback DURATION` (default `168h`) — how far back to search.
  - `--auto-burst-window DURATION` (default `8m`) — max time span for a
    cluster of failures to count as simultaneous.
  - `--auto-min-silence DURATION` (default `90s`) — minimum quiet period
    after the burst before declaring credit exhaustion. Rejects bursts
    followed by a quick recovery, which usually indicates a regional
    outage on the provider rather than a wallet event.

**Eligibility**: status must be `failed` or `canceled`, and the current
`termination_reason` must be generic (`provider_failure`,
`infra_failure`, `unknown`, or empty). Specific reasons such as
`disk_full`, `job_failure`, or `weft_bug` are never overwritten. The
prior `termination_detail` is preserved with a timestamped
"reclassified" prefix.

See [Cloud Instance Debugging](../guides/cloud-instance-debugging.md)
for the recommended incident-response walkthrough.

### weft campaign launch / watch / list / show / terminate

Batch-level commands. `weft campaign launch` is a deprecated alias for
`weft start instance`.

```bash
weft campaign watch [campaign-id] [--plain|--tui]   # Watch a batch (incl. relaunches)
weft campaign list [--plain|--tui]                  # List batches
weft campaign show <campaign-id>                    # Inspect a batch
weft campaign terminate <campaign-id>               # Terminate every instance in the batch
weft campaign cost                                  # Estimated vs actual cost per campaign
```

See [Cloud GPU Instances](../guides/instances.md) for lifecycle, grace
periods, interruptible jobs, and survival-based offer selection, and
[Campaigns](../guides/campaigns.md) for the batching concept.

### weft cost

Cross-cutting cost summaries. Each subcommand also has a noun-verb alias.

```bash
weft cost instances                     # Rate, duration, and actual cost per instance
weft cost jobs                          # Per-job cost with instance context and overhead
weft cost campaigns                     # Estimated vs actual cost per campaign batch

# Noun-verb aliases
weft instance cost
weft job cost
weft campaign cost
```

`weft cost jobs` / `weft job cost` shows a detail block per job (matching
`weft job status` style) with job cost, instance, jobs-on-instance count,
instance total cost, and per-job overhead.

### weft narrate

Stream a human-readable LLM-generated narration of job, instance, campaign,
and autopilot transitions. Uses Anthropic directly or OpenRouter when
configured. Commentary, not authoritative status.

```bash
weft narrate                          # narrate DB-driven transitions to stdout
weft narrate --tick 10s               # maximum interval between checks
weft narrate --quiet-window 3s        # coalesce DB-change bursts for 3s
weft narrate --once                   # single description of current state
weft narrate --project myproj         # scope to one project
weft narrate --slack                  # also post to configured Slack webhook
weft narrate --debug                  # write deltas + cache stats to stderr
```

Passes with no transitions are skipped — no API call, no output. Silence
between paragraphs means no narratable transition has been observed.

See [Activity Narration](../guides/narrate.md) for the full reference,
including caching/cost shape and configuration knobs.

### weft job start

Start a queued job immediately, bypassing its queue order. The job is removed
from the remote queue file, marked as running, and launched right away.

```bash
weft job start <job-id>
weft run <job-id>           # Shorthand (same effect)
```

Examples:
```bash
weft run wj512                # Start queued job 512 immediately
weft job start wj512          # Same as above
weft job start wj9001         # Bypass queue order and run now
```

Only jobs with status `queued` can be started this way. The command preserves
the job's working directory, environment variables, and metadata.

**Note:** For running or completed jobs, use `run --from <id>` to create a new job on the desired host.

### Advanced run options

The `run` command supports several advanced options for more control:

**Copy settings from existing job (`--from`)**:
```bash
weft run --from <job-id> [<host>] [<command>]
```

Copies command, working directory, and description from an existing job. You can override any of these:

```bash
weft run --from wj42                    # Rerun job wj42 with same settings
weft run --from wj42 atlas              # Rerun on different host
weft run --from wj42 --timeout 4h       # Rerun with longer timeout
weft run --from wj42 atlas "python train.py --epochs 200"  # Override everything
```

**Timeout (`--timeout`)**:
```bash
weft run --timeout <duration> <host> <command>
```

Automatically kills the job after the specified duration (e.g., "2h", "30m", "1h30m"):

```bash
weft run --timeout 2h titan "python train.py"
weft run --timeout 30m --from wj42    # Retry with timeout
```

**Environment variables (`-e, --env`)**:
```bash
weft run -e VAR=value <host> <command>
```

Set environment variables for the job. Can be repeated for multiple variables:

```bash
weft run -e CUDA_VISIBLE_DEVICES=0 titan "python train.py"
weft run -e BATCH_SIZE=32 -e LR=0.001 titan "python train.py"
weft queue add -e TMPDIR=/mnt/data/tmp titan "python train.py"
```

**Rental disk sizing (`--disk`, `--disk-max`, `--runtime-disk`)**:
```bash
weft run --disk 120 --runtime-disk 24 --input hf:org/model "uv sync --project scripts/vllm-profiling && python bench.py"
weft queue add --runtime-disk 16 titan "uv run python train.py"
weft run --disk-max 90 --input hf:gpt2 "uv run bench.py"
```

`--disk` sets the total rental instance disk floor. `--runtime-disk` adds
scratch/cache headroom for runtime setup that is not represented by declared
inputs, such as `uv sync`, pip wheels, CUDA wheels, vLLM, and build temp dirs.
Runtime setup headroom is explicit; command text does not automatically add a
runtime cache estimate. Weft combines explicit disk settings with HF input
sizes, cached uv-lock estimates, Docker image overhead, and prior observed disk
usage.

`--disk-max` caps the result. It is the only disk knob that can lower the
request, and it is applied last, overriding both the `--disk` floor and the
built-in minimum. Use it when you know the estimate overshoots — most often
because `--input hf:<model>` prices the whole repository including
serialization formats your loader never fetches. `weft job disk-calibration
--project <name> --rows` shows estimate-vs-actual peak per launch. Capping is
an explicit acceptance of risk: a subsequent out-of-disk failure is
attributable to the cap, and weft logs whenever one binds.

Both are also available as `[tool.weft] disk` / `disk-max` script metadata, and
on `weft restart` / `weft retry` (`--disk-max 0` clears).

Secret values can be stored locally and attached by reference so tokens are not
written into job rows or command output:

```bash
weft secret set hf "$HF_TOKEN"
weft run --hf-token --input hf:meta-llama/Meta-Llama-3-8B "python train.py"
weft run --hf-token-from env:HF_TOKEN --input hf-dataset:org/private-data "python train.py"
weft run --secret WANDB_API_KEY=env:WANDB_API_KEY "python train.py"
```

On macOS, weft stores secrets in the Keychain. On other platforms it uses
`~/.config/weft/secrets.json` with owner-only permissions. Job definitions keep
references such as `HF_TOKEN=secret:hf`; weft resolves them when launching the
job and redacts secret-looking env vars in status output.

When an `hf:` or `hf-dataset:` input is declared and the `hf` secret exists,
weft automatically attaches `HF_TOKEN=secret:hf` unless `HF_TOKEN` is already
set with `--env` or `--secret`.

**Automatic environment file loading**:

The queue runner automatically loads environment files from the job's working directory before executing the command. Files are loaded in this order (later files override earlier ones):

1. `.env` - loaded with auto-export (`set -a`)
2. `.env.local` - loaded with auto-export (`set -a`)
3. `.envrc` - loaded without auto-export (for direnv compatibility)

The job log will show "Loading .env" etc. when these files are found and sourced.

**Weft-provided environment variables**:

Weft also injects runtime context variables. All jobs get `WEFT_JOB_ID`. Rental
jobs additionally get:

| Var | Value |
| --- | --- |
| `WEFT_TARGET_KIND` | `rental` |
| `WEFT_LAUNCH_ID` | Weft launch/instance id |
| `WEFT_PROVIDER` | Cloud provider, such as `vastai` or `runpod` |
| `WEFT_INSTANCE_TYPE` | `on-demand` or `interruptible` |
| `WEFT_RESUMED` | `1` when the agent restarted on the same retained disk after a provider pause/resume |
| `WEFT_RESTAGED` | `1` when Weft restored the job's previous R2-streamed outputs before starting it on a fresh instance |

For interruptible rental behavior and checkpoint resume guidance, see
[Cloud GPU Instances](../guides/instances.md#interruptible-instances).

### weft secret

Store and manage local secret values used by job environment variables.

```bash
weft secret set <name> [value]
weft secret list
weft secret remove <name>
```

`weft secret set` also accepts piped input:

```bash
printf '%s' "$HF_TOKEN" | weft secret set hf
```

Use `secret:<name>` in env vars when you need to attach a stored secret
manually:

```bash
weft run -e HF_TOKEN=secret:hf "python train.py"
```

### weft cleanup

Clean up finished sessions and old log files on inventory hosts, or audit/delete
terminal rental provider resources.

```bash
weft cleanup [host] [flags]
weft cleanup --provider runpod --dry-run
weft cleanup --provider runpod --force
```

**Flags:**
- `--sessions`: Kill finished sessions only
- `--logs`: Remove archived log files only
- `--older-than N`: Only clean items older than N days (default: 7)
- `--dry-run`: Preview without actually deleting
- `--provider vastai|runpod`: Restrict cloud cleanup to a provider
- `--force`: Delete matching terminal cloud provider instances

**Examples:**
```bash
weft cleanup deepthought                    # Clean both
weft cleanup deepthought --sessions         # Only finished sessions
weft cleanup deepthought --logs --older-than 3  # Logs > 3 days old
weft cleanup deepthought --dry-run          # Preview only
weft cleanup --provider runpod --dry-run    # Preview terminal RunPod pods that still exist
weft cleanup --provider runpod --force      # Delete terminal RunPod pods that still exist
```

For a per-job read-only cleanup check, use `weft instance audit <job-id>`.

### weft kill

Kill a running job.

```bash
weft kill <job-id>
```

**Example:**
```bash
weft kill wj42    # Kill job #42
```

### weft pause

Pause a running job (SIGSTOP).

```bash
weft pause <job-id>
```

**Example:**
```bash
weft pause wj42   # Pause job #42
```

### weft resume

Resume a paused job (SIGCONT).

```bash
weft resume <job-id>
```

**Example:**
```bash
weft resume wj42  # Resume job #42
```

### weft job draft

Move a job into draft status and make sure it never runs remotely (queued or otherwise).

```bash
weft job draft <job-id>
```

When you draft a job:
- Running jobs are killed and removed from the remote host queue/state
- Queued jobs are removed from queue files so they won’t start later
- Offline hosts keep the “draft pending” request until the next `weft sync`
- Already-completed jobs simply change status locally

Drafting is handy when you realize a queued job shouldn’t run anymore but you
want to keep its metadata/log references around for editing or cloning later.
You can also trigger the same action from the TUI by pressing `d`.

### weft host

Set up and inspect on-prem inventory hosts.

#### weft host setup

Provision an on-prem host for use with weft:

```bash
weft host setup <host>
```

The setup command:

- Installs the required host tools: `tmux`, `jq`, `rsync`, `rclone`, `curl`, and `uv`.
- Installs or verifies a Go toolchain when the agent is built natively on the host.
- Discovers CPU, memory, GPU, NVIDIA driver, CUDA compatibility, and cache information.
- Writes `~/.config/weft/hosts/<host>.yaml` from the discovered hardware.
- Deploys `weft-agent` to `~/.cache/weft/bin/weft-agent`.
- Deploys R2 and notification configuration when configured.
- Starts the queue runner unless `--no-runner` is set.

Examples:

```bash
weft host setup studio
weft host setup cool30 --no-runner
weft host setup cool30 --skip-prerequisites
```

Use `--skip-prerequisites` only when the target account already has the required
tools on its job PATH. Python jobs submitted through `uv run ...` require `uv`
to be available to the account that runs the queue agent.

### weft queue

Manage job queues for CPU-capped execution on remote hosts.

Jobs added to a queue are scheduled in FIFO order, and the queue runner can run multiple jobs per host while keeping total CPU usage under a target cap. The queue runner runs in a tmux session on the remote host and keeps working when you disconnect. CPU allotments and GPU memory reservations can be set via `weft job describe --cpu <percent>` and `--gpu-mem <gb>`.

#### weft queue add

Add a job to a remote queue.

```bash
weft queue add [flags] <host> <command...>
```

**Flags:**
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-d, --description TEXT`: Description of the job
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--draft`: Save a draft queue entry locally without touching the remote queue file
- `--after, --depends-on ID`: Start job after another job succeeds
- `--after-any ID`: Start job after another job completes (success or failure)

Draft queue entries behave like sticky notes: they keep the command, env vars,
and metadata in your local database while guaranteeing they never reach the
remote queue runner. Convert them later with `weft job draft <id>` (or
the `d` key in the TUI) once you decide they should be eligible to run.

**Examples:**
```bash
weft queue add titan 'python train.py --epochs 100'
weft queue add -d "Training run 1" titan 'python train.py'
weft queue add -e CUDA_VISIBLE_DEVICES=0 titan 'python train.py'
weft queue add --after wj42 titan 'python eval.py'       # Run after job wj42 succeeds
weft queue add --after-any wj42 titan 'python cleanup.py' # Run after job wj42 completes (success or failure)
```

#### weft edit

Edit a queued or draft job’s metadata—description, working directory, command, environment variables, tags, or dependencies.
Draft edits are local-only; queued edits are propagated to the remote queue entry when the job is already placed on a host.
`weft queue edit` is an alias for this command and accepts the same flags.

```bash
weft edit [flags] <job-id>
```

**Flags:**
- `-m, --message TEXT`: Set job description
- `-C, --directory DIR`: Set working directory
- `--command CMD`: Replace the command
- `-e, --env VAR=value`: Replace environment variables (repeat flag to set multiple)
- `--clear-env`: Remove all environment variables
- `--tag TAG`: Replace the job's tags (repeat flag to set multiple)
- `--remove-tag TAG`: Remove one tag without replacing other tags (repeat flag to remove multiple)
- `--clear-tags`: Remove all job tags
- `--depends-on ID[,ID...]`: Require the listed jobs to succeed before running
- `--depends-on-any ID[,ID...]`: Wait for the listed jobs to finish (success or failure)
- `--clear-depends`: Remove all dependencies from the job
- `--provider vastai|runpod`: Change the job's rental provider preference
- `--runpod-cloud-type community|secure`: Set the job's RunPod cloud type; use `default`, `auto`, `none`, or `clear` to remove the per-job override
- `--max-hourly-rate USD`: Reject rental offers above this hourly rate; `0` clears the cap
- `--max-spend USD`: Set the rental instance's hard spend limit; `0` clears the cap
- `--max-time DURATION`: Set the rental instance's hard lifetime; `0`, `default`, `clear`, or `auto` clears the cap
- `--min-survival FRACTION`: Set the job's Weft learned end-to-end survival floor; `0` disables the floor for this job. This is not the provider reliability score.
- `--grace-period DURATION`: Set the post-failure instance grace period; `0` disables it, while `default`, `clear`, or `auto` removes the override
- `--retry`: Requeue a terminal job and apply the requested edits in the same command

IDs can also be suffixed with `+` or `:any` to mark them as completion-based dependencies, e.g. `--depends-on 101+` or `--depends-on 101:any`.

**Examples:**
```bash
weft edit wj1595 --depends-on wj1599
weft edit wj1600 --depends-on wj1400 --depends-on-any wj1401
weft edit wj1700 --clear-depends
weft edit wj1750 --tag benchmark-isolation --tag exp-012
weft edit wj2428 --remove-tag benchmark-isolation
weft edit wj1800 --command "python eval.py" -C ~/project -e FOO=bar
weft edit wj1900 --max-hourly-rate 4.50 --max-spend 12 --max-time 3h
```

`--tag` replaces the complete tag set. Use `--remove-tag` as the edit-form
synonym for `weft job tag rm` when removing a tag from the existing set.

Dependencies can target local-host jobs or rental/ephemeral jobs. For rental
dependencies, the queue entry is held until dependency completion/artifact sync
is visible in local state. If the host is offline, dependency updates are
deferred like other queue operations and reapplied once it reconnects.

#### weft queue start

Start the queue runner on a remote host.

```bash
weft queue start [flags] <host>
```

The queue runner:
- Runs in a tmux session (`weft-queue-default`)
- Processes queued jobs in FIFO order with a CPU cap
- Continues running even when you disconnect
- Sends Slack notifications (if configured)
- Enforces a setup-phase timeout (default: `20m`) before the main command starts

**Examples:**
```bash
weft queue start titan
```

Setup timeout can be overridden per host in the inventory file:

```yaml
# ~/.config/weft/hosts/titan.yaml
name: titan
setup_timeout: 90m
```

#### weft queue stop

Stop the queue runner after the current job completes.

```bash
weft queue stop [flags] <host>
```

**Examples:**
```bash
weft queue stop titan
```

#### weft queue list

Show jobs waiting in the queue and the currently running job.

```bash
weft queue list [flags] <host>
```

**Examples:**
```bash
weft queue list titan
```

#### weft queue status

Show the status of the queue runner.

```bash
weft queue status [flags] <host>
```

**Examples:**
```bash
weft queue status titan
```

#### weft queue upgrade

Redeploy and restart the queue runner if the remote script is out of date. The
CLI records a build number on the first line of the embedded script, compares
it with the version on the host, and restarts the runner only when needed.

```bash
weft queue upgrade titan
```

#### Queue Workflow Example

```bash
# Start the queue runner (does nothing if already running)
weft queue start titan

# Add jobs to the queue - laptop can disconnect after these commands
weft queue add titan "python train.py --epochs 100"
weft queue add titan "python train.py --epochs 200"
weft queue add titan "python evaluate.py"

# Check queue status (when back online)
weft queue status titan

# View what's in the queue
weft queue list titan

# Stop the queue after current job
weft queue stop titan
```

#### Job Dependencies

You can create job chains where one job runs after another completes:

```bash
# Start the queue runner
weft queue start titan

# Job wj42: Training
weft queue add -d "Training" titan "python train.py"

# Job wj43: Evaluate after training succeeds (waits for job wj42)
weft queue add --after wj42 -d "Evaluation" titan "python eval.py"

# Job wj44: Generate report after evaluation (waits for job wj43)
weft queue add --after wj43 -d "Report" titan "python report.py"

# Job wj45: Cleanup runs regardless of whether job wj42 succeeded or failed
weft queue add --after-any wj42 -d "Cleanup" titan "python cleanup.py"

# Disconnect laptop - jobs run in sequence on the remote host
```

**Dependency flags:**
- `--after ID`: Waits for the job to succeed (exit code 0). Skips if parent fails.
- `--after-any ID`: Waits for the job to complete (any exit code). Always runs.

Both flags work entirely on the remote host (no laptop connection needed) and can be used with both `queue add` and `run` commands.

> **Note:** Dependencies must stay on the same host. If you try to start a job on
> `titan` that waits on a job recorded on `studio`, the CLI errors immediately
> instead of queuing work that can never start.

### weft budget

Show your Vast.ai account credit balance.

```bash
weft budget [flags]
```

**Flags:**
- `--open`: Open the Vast.ai billing page in your browser

**Examples:**
```bash
# Check remaining credit
weft budget

# Check balance and open billing page
weft budget --open
```
