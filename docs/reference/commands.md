# Command Reference

This is the canonical CLI reference for `weft` commands. For end-to-end workflows, see [Workflow Guide](../guides/workflow-guide.md) and [Cloud GPU Instances](../guides/instances.md).

## Commands

### weft run

Queue a job on a remote host for managed execution.

```bash
weft run [flags] [host] <command...>
```

The host is optional when a coordinator is running — the placement engine
automatically selects the best host based on GPU constraints, data locality,
current utilization, and queue depth.

By default, jobs are added to a queue and scheduled by the queue runner. It can run multiple jobs on a host while keeping total CPU usage under a target cap. Use `--immediate` (`-i`) to start a job immediately.

Use `start <job-id>` to start a queued job immediately.

**Flags:**
- `-i, --immediate`: Start job immediately instead of queuing
- `-C, --directory DIR`: Working directory (default: current directory path)
- `-m, --message TEXT`: Description of the job (for logging and queries)
- `-e, --env VAR=value`: Set environment variable (can be repeated)
- `--tag TAG`: Tag to attach to the job (can be repeated). Reserved tags: `exclusive` makes the job run alone; `benchmark` is like `exclusive` but also waits for system-wide idle and skips hosts marked `shared = true` during auto-placement; `rental` skips local placement and sends the job toward rental-GPU workflows; `inventory` blocks rental placement, keeps the job on inventory hosts only, and allows benchmark jobs to use shared inventory hosts; `interruptible` allows interruptible cloud offers for that job (`preemptible` is accepted as a synonym). Legacy aliases `cloud` and `on-prem` are accepted for compatibility.
- `--draft`: Record the job locally in draft status (never contacts the host until you later promote it)
- `-f, --follow`: Follow log output after starting (requires `--immediate`)
- `--allow`: Stream the job log live and stay attached (requires `--immediate`)
- `--from ID`: Copy settings from existing job ID (allows overriding)
- `--timeout DURATION`: Kill job after duration (e.g., "2h", "30m", "1h30m")
- `--after, --depends-on ID`: Start job after another job succeeds. For inventory jobs this is enforced by the per-host queue; for rental jobs it is a placement gate — the downstream is held back from `weft start instance` selection until the upstream succeeds, but co-location is not forced
- `--after-any ID`: Start job after another job completes, success or failure. Same gating behavior as `--after` for rentals
- `--kill ID`: Kill a job by ID (synonym for `weft kill`)
- `--input ASSET`: Declare a data input. Accepts HF refs (`hf:model-id`), project-relative directories (`local:data/conllu/`), or absolute/tilde paths. HF assets influence placement scoring and trigger downloads; `local:` paths are synced via rsync before the job runs
- `--output ASSET`: Declare a data output (e.g., `checkpoint:llama-ft-v1`, `local:cache/representations/`). Recorded on successful completion for downstream jobs
- `--gpu CLASS`: GPU constraint with optional memory (e.g., `a100`, `ampere+`, `nvidia>=24GB`)
- `--gpu-class CLASS`: Require a specific GPU class or generation (e.g., `a100`, `ampere+`)
- `--gpu-mem GB`: Requested GPU memory in GB (weft adds `+2GB` headroom by default)
- `--gpu-mem-strict`: Use exact `--gpu-mem` matching (disable default `+2GB` headroom)
- `--produces PATH`: Artifact path this job produces (repeatable, e.g., `output/model.pt`)
- `--needs PATH:VERSION`: Artifact path:version this job needs (repeatable, e.g., `output/model.pt:100`; rental/ephemeral producers are staged from cloud artifact storage, including producers that completed hours or days earlier — no need to pre-fetch with `weft artifact get` and pass `--input local:`)
- `--dry-run`: Show placement scores without submitting the job
- `--no-sync`: Skip source sync before submission
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

If an immediate run can't reach the host, the CLI automatically records the job
locally and defers it to the remote queue. The next sync (or any command that
touches that host) will append the saved entry so it runs as soon as the host is
reachable again.

Draft jobs (`--draft`) stay entirely local. They’re useful for capturing a job
definition you want to tweak later or to keep certain jobs from ever syncing to
the host. When you’re ready to discard the draft, run `weft job draft <id>`
to toggle the status (or do it from the TUI, described below).

**Examples:**
```bash
# Queue a job (default behavior)
weft run deepthought 'python train.py'

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
weft run --tag benchmark deepthought 'python bench_encode.py'

# Force rental placement (legacy alias: --tag cloud)
weft run --tag rental --gpu a100 'python train.py'

# Keep a job on inventory hosts only (legacy alias: --tag on-prem)
# Inventory-tagged benchmark jobs may still use shared inventory hosts
weft run --tag inventory --gpu a100 'python train.py'

# Run job after another succeeds
weft run --after 42 deepthought 'python eval.py'

# Run cleanup job after another completes (success or failure)
weft run --after-any 42 deepthought 'python cleanup.py'

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
  --after 1046 \
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
weft data where hf:meta-llama/Llama-3-8B

# Download a model to a specific host
weft data fetch hf:meta-llama/Llama-3-8B --host cool100

# Download a model onto the local machine
weft data fetch hf:meta-llama/Llama-3-8B --host localhost

# Download a dataset revision to a host
weft data fetch hf-dataset:HuggingFaceFW/fineweb --host cool30 --revision main

# Show request history
weft data requests
weft data requests --host cool100
```

### weft artifact

Track and retrieve job outputs through a durable local artifact store.

Artifacts are declared by writing a manifest on the remote host. The CLI
syncs those files into `~/.config/weft/artifacts/` so they survive
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

# List cached artifacts
weft artifact list wj2073

# Retrieve by name or path
weft artifact get wj2073 selectivity_results -o ./results.json
weft artifact get wj2073 output/selectivity_results.json -o ./results.json

# Write artifact to stdout
weft artifact get wj2073 selectivity_results -o -
weft artifact cat wj2073 selectivity_results | jq '.metric'

# Resolve latest job by tag
weft artifact get --tag exp-012 --latest selectivity_results -o ./results.json
```

#### Automatic output collection

Jobs automatically track files written to `output/` or `outputs/` in the
working directory. On successful completion, the runner records discovered files
in the completion record and syncs them back:

- **On-prem jobs**: Auto-syncs outputs via rsync if total size is under the
  configured limit (default: 100 MB). Larger outputs must be retrieved manually
  with `weft artifact sync`.
- **Cloud jobs**: All outputs are uploaded to R2 regardless of size.

Customize output directories, auto-sync behavior, cloud image, and
project-specific source excludes in `.weft.toml`:

```toml
[outputs]
dirs = ["results/"]          # Default: ["output/", "outputs/"]
max_auto_sync_mb = 200       # Default: 100

[sync]
exclude_dirs = ["data"]      # Additional project-specific source excludes

[cloud]
image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"  # Override default Docker image
```

Use `weft artifact list <job-id>` to see discovered outputs and
`weft artifact sync <job-id>` to pull them from the remote host on demand.

The command:
- Creates a job ID and adds it to the remote queue (or starts immediately with `-i`)
- Queue runner schedules queued jobs in FIFO order (subject to CPU allotments)
- Saves job metadata and logs to `~/.cache/weft/logs/` on the remote host
- Records the job in a local SQLite database (`~/.config/weft/jobs.db`)
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
- Use `--wait` (with optional `--wait-timeout`) to block until jobs finish.
  The command exits with `0` only if every waited-on job succeeds.

### weft job list

Query and search job history from the local database.

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
- `--limit N`: Limit results (default: 50)
- `--show ID`: Show detailed info for a specific job
- `--cleanup DAYS`: Delete jobs older than N days
- `--sync`: Sync job statuses from remote hosts before listing
- `--group-by status`: Group table output by status sections (`Running`, `Queued`, `Completions`, `Failures`, `Killed/Canceled`)

`--group-by status` only supports table/plain output; combining it with
`--format json` or `--format tsv` returns an error.
When combined with `--unprocessed`, grouped views omit `canceled` jobs (which
were already handled by the agent) but still include `killed` jobs.

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
weft job list --group-by status        # Grouped status view
weft job list --group-by status --watch # Grouped live view (TUI on interactive terminals)
weft job list wj12::wj14                   # List jobs 12 through 14
weft job list wj12...wj13                  # Alternative range syntax
weft job list wj12,wj13,wj14                 # Comma-separated IDs
weft job list --show 42                # Job details
weft job list --cleanup 30             # Remove old jobs
```

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
- `--recent DURATION`: Window for recent terminal jobs (default: `24h`)

**Examples:**
```bash
weft project watch
weft project watch --plain
weft project watch --plain --follow
weft project watch --recent 48h
```

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

Reserved placement tags use the preferred names `rental` and `inventory`.
Legacy aliases `cloud` and `on-prem` are accepted on input and mapped to the
preferred names in CLI/TUI output.

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
weft log --ops --job 42          # Operations for a job
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

### weft telemetry

Show telemetry for a job's latest attempt.

```bash
weft telemetry <job-id> [job-id...]
weft job telemetry <job-id> [job-id...]   # Alias
```

The command reads the latest attempt's synced telemetry samples, prints the
time range, and summarizes GPU utilisation, temperature / clock data when
available, plus per-GPU memory and activity data from the richer telemetry
stream.

**Flags:**
- `--json`: Emit machine-readable JSON

**Examples:**
```bash
weft telemetry wj42
weft telemetry wj42 wj43
weft telemetry wj42 --json
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

`retry` re-syncs project-derived inputs/outputs and re-reads `[tool.weft]`
script metadata for GPU defaults. Explicit `retry` flags (`--gpu`,
`--gpu-class`, `--gpu-mem`, `--gpu-mem-strict`) take precedence over script
metadata.

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
- `--strategy cheap|fast|fastest`: Offer selection strategy (default: `cheap`)
- `--min-survival FRACTION`: Minimum survival probability for offers (default: `0.4`; `0` disables)
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
```

A launch is recorded as a **campaign** — a batch row grouping the instances
provisioned together. Per-instance operations live under `weft instance …`;
batch-level operations live under `weft campaign …` (see below).

### weft instance watch / list / status / ssh / terminate

Per-instance commands.

```bash
weft instance watch [instance-id]       # Watch active instances (or one)
weft instance list                       # List instances
weft instance status <instance-id>       # Single instance details
weft instance ssh <instance-id>          # SSH in
weft instance terminate <instance-id>    # Destroy a single instance
```

### weft campaign launch / watch / list / show / terminate

Batch-level commands. `weft campaign launch` is a deprecated alias for
`weft start instance`.

```bash
weft campaign watch [campaign-id] [--plain|--tui]   # Watch a batch (incl. relaunches)
weft campaign list [--plain|--tui]                  # List batches
weft campaign show <campaign-id>                    # Inspect a batch
weft campaign terminate <campaign-id>               # Terminate every instance in the batch
```

See [Cloud GPU Instances](../guides/instances.md) for lifecycle, grace
periods, interruptible jobs, and survival-based offer selection, and
[Campaigns](../guides/campaigns.md) for the batching concept.

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
weft run --from 42                    # Rerun job 42 with same settings
weft run --from 42 atlas            # Rerun on different host
weft run --from 42 --timeout 4h       # Rerun with longer timeout
weft run --from 42 atlas "python train.py --epochs 200"  # Override everything
```

**Timeout (`--timeout`)**:
```bash
weft run --timeout <duration> <host> <command>
```

Automatically kills the job after the specified duration (e.g., "2h", "30m", "1h30m"):

```bash
weft run --timeout 2h titan "python train.py"
weft run --timeout 30m --from 42      # Retry with timeout
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

**Automatic environment file loading**:

The queue runner automatically loads environment files from the job's working directory before executing the command. Files are loaded in this order (later files override earlier ones):

1. `.env` - loaded with auto-export (`set -a`)
2. `.env.local` - loaded with auto-export (`set -a`)
3. `.envrc` - loaded without auto-export (for direnv compatibility)

The job log will show "Loading .env" etc. when these files are found and sourced.

### weft cleanup

Clean up finished sessions and old log files.

```bash
weft cleanup <host> [flags]
```

**Flags:**
- `--sessions`: Kill finished sessions only
- `--logs`: Remove archived log files only
- `--older-than N`: Only clean items older than N days (default: 7)
- `--dry-run`: Preview without actually deleting

**Examples:**
```bash
weft cleanup deepthought                    # Clean both
weft cleanup deepthought --sessions         # Only finished sessions
weft cleanup deepthought --logs --older-than 3  # Logs > 3 days old
weft cleanup deepthought --dry-run          # Preview only
```

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
weft queue add --after 42 titan 'python eval.py'       # Run after job 42 succeeds
weft queue add --after-any 42 titan 'python cleanup.py' # Run after job 42 completes (success or failure)
```

#### weft edit

Edit a queued job’s metadata—description, working directory, command, environment variables, tags, or dependencies.  
`weft queue edit` is an alias for this command and accepts the same flags.

```bash
weft edit [flags] <job-id>
```

**Flags:**
- `-m, --message TEXT`: Set job description
- `-C, --directory DIR`: Set working directory
- `--command CMD`: Replace the queued command
- `-e, --env VAR=value`: Replace environment variables (repeat flag to set multiple)
- `--clear-env`: Remove all environment variables
- `--tag TAG`: Replace the job's tags (repeat flag to set multiple)
- `--clear-tags`: Remove all job tags
- `--depends-on ID[,ID...]`: Require the listed jobs to succeed before running
- `--depends-on-any ID[,ID...]`: Wait for the listed jobs to finish (success or failure)
- `--clear-depends`: Remove all dependencies from the job

IDs can also be suffixed with `+` or `:any` to mark them as completion-based dependencies, e.g. `--depends-on 101+` or `--depends-on 101:any`.

**Examples:**
```bash
weft edit wj1595 --depends-on wj1599
weft edit wj1600 --depends-on wj1400 --depends-on-any wj1401
weft edit wj1700 --clear-depends
weft edit wj1750 --tag benchmark --tag exp-012
weft edit wj1800 --command "python eval.py" -C ~/project -e FOO=bar
```

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

# Job 42: Training
weft queue add -d "Training" titan "python train.py"

# Job 43: Evaluate after training succeeds (waits for job 42)
weft queue add --after 42 -d "Evaluation" titan "python eval.py"

# Job 44: Generate report after evaluation (waits for job 43)
weft queue add --after 43 -d "Report" titan "python report.py"

# Job 45: Cleanup runs regardless of whether job 42 succeeded or failed
weft queue add --after-any 42 -d "Cleanup" titan "python cleanup.py"

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
