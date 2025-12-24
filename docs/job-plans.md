# Job Execution Plans

Plans let you describe a batch of jobs in YAML and submit them all at once.
The CLI reads the plan, kills any requested jobs, and then decides which
entries should run immediately and which must be queued with dependency
rules that reuse the existing `--after` / `--after-any` behaviors.

## File structure

```yaml
version: 1                     # required for forwards-compatibility
kill: [12, 19]                 # optional list of job IDs to kill first
jobs:                          # required list of plan items
  - job:                       # a single job entry
      name: prep-dataset       # optional identifier for logs/references
      host: cool42             # required
      dir: ~/code/train        # optional working directory
      command: python prep.py  # required
      description: Prepare dataset
      env:                     # optional environment variables
        DATASET: imagenet
        CUDA_VISIBLE_DEVICES: "0"

  - parallel:                  # jobs that may start immediately
      name: launch-trainers    # optional label for output clarity
      dir: ~/code/train        # defaults shared with nested jobs
      env:
        DATASET: imagenet
      jobs:
        - host: cool42
          command: python train.py --shard 0
        - host: cool43
          command: python train.py --shard 1

  - series:                    # queue-backed sequential block
      name: evaluate
      wait: success            # "success" (default) or "any"
      queue: default           # optional queue name on the host
      dir: ~/code/eval
      env:
        CUDA_VISIBLE_DEVICES: "0"
      jobs:
        - host: cool42
          command: python eval.py
        - host: cool42
          command: python clean.py
```

Rules:
- `version` must be `1` for this initial format. Newer versions will add
  backwards-compatible syntax.
- `kill` (optional) is a list of numeric job IDs. The CLI will call the
  existing `remote-jobs kill` logic for each ID before scheduling new work.
- `jobs` is an ordered list. Entries run in the listed order, except that
  all jobs inside a `parallel` block run independently.
- Inside `parallel.jobs` and `series.jobs`, you list raw job definitions
  (no extra `job:` key). Each job entry must at least define `host` and
  `command`.
- `parallel` and `series` blocks can set `dir` and `env` to provide defaults
  for every nested job. A nested `job` entry can still override either field.
- `series` blocks enforce sequential execution on the remote queue runner.
  Every job in the block is queued on the specified host & queue name. The
  `wait` field decides how the queue runner encodes dependencies:
    - `success` (default): later jobs use `--after` semantics (run only if
      the previous job exits with code 0).
    - `any`: later jobs use `--after-any` semantics, so they run after the
      previous job completes whether it succeeded or failed.
- All jobs in a `series` block must target the same host (and queue name if
  provided). This ensures the remote queue runner can inspect the prior
  job's status files.

### Job fields

| Field | Type | Description |
| --- | --- | --- |
| `name` | string | Optional handle that appears in CLI output. |
| `host` | string | SSH host as used by other commands (required). |
| `command` | string | Command to execute (required). |
| `dir` | string | Working directory (`-C` equivalent). |
| `description` | string | Same as `-d`/`--description`. |
| `env` | map[string]string | Environment variables (`-e`). |
| `queue` | string | Queue name for non-series jobs that should be enqueued (optional). |
| `queue_only` | bool | Force a non-series job into queue mode instead of starting immediately. |
| `when` | object | Reserved for future resource triggers (see below). |

Unless `queue_only` or a containing `series` block says otherwise, jobs are
started immediately via `remote-jobs run`. They behave exactly like a manual
invocation with matching host, command, directory, description, and env vars.
Jobs inside `parallel` blocks simply have no automatic dependencies, so they
can run simultaneously as soon as their host accepts connections.

Jobs inside a `series` block are always queued on the remote host. The plan
uses the queue name declared on the block (or `default`). The first job in the
block is queued without a dependency; subsequent jobs specify the prior job's
ID via the same mechanism that backs `remote-jobs run --after` and
`--after-any`.

Jobs with `queue_only: true` behave like `remote-jobs queue add`. They are
written to the specified (or default) remote queue, and the CLI automatically
starts the queue runner on that host unless you pass `--no-queue-start` to
`remote-jobs plan submit`.

### Resource-trigger syntax (reserved)

To prepare for resource-aware scheduling, each job may include an optional
`when` block. The syntax is documented now so we can keep the YAML schema
stable, but the CLI currently rejects plans that try to use it.

```yaml
job:
  host: cool42
  command: python train.py
  when:
    cpu_below: 30          # percent utilization threshold
    ram_free_gb: 16        # gigabytes of RAM that must be free
    gpu:
      device: any          # "any" or a numeric GPU index
      util_below: 40       # percent utilization threshold
      memory_free_gb: 12   # gigabytes of free VRAM required
```

Future stages will let the CLI delay dispatching this job until the host meets
these thresholds. For now, attempting to use `when` produces a validation
error so that plans do not silently ignore resource constraints.

### CLI usage

Run a plan file with:

```bash
remote-jobs plan submit plan.yaml
remote-jobs plan submit --host studio plan.yaml   # provide default host via CLI
remote-jobs plan submit - < generated-plan.yaml   # stdin / heredoc
cat <<'EOF' | remote-jobs plan submit --host studio -
version: 1
jobs:
  - job:
      command: hostname
EOF
```

Every submission prints a "Command to job IDs" map so downstream tooling can
attach, stream logs, or build additional dependencies.

Add `--watch 10m` (or any Go duration) to keep the CLI running for up to that
amount of time while syncing statuses. The watch summary reports which plan
items have succeeded, failed, or remain queued/running when the timer expires.

Use `--host <hostname>` to supply a default for any job whose YAML omits the
`host` field, keeping small snippets readable when everything runs on the same
machine.
