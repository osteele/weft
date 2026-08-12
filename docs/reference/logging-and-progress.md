# Logging & Progress Features

This document collects the details behind two related usability features:
staying attached to a job’s live logs without risking the remote process, and
surfacing structured progress information in the TUI.

## Live Log Streaming (`weft run --allow`)

`--allow` lets you keep the CLI process attached to a job’s output after it has
been launched in tmux. Unlike traditional `ssh <cmd>` sessions, the job keeps
running even if you interrupt the CLI or lose connectivity—the stream only ever
tails the log file the wrapper script already writes.

### Goals

- Provide an opt-in flag that mirrors `tail -f` behavior while preserving the
  resilience guarantees of tmux-based execution.
- Make it obvious how to detach (`Ctrl+C`) and how to resume watching logs later
  (`weft log <id> -f`).
- Avoid inventing new on-host plumbing by reusing the existing log + wrapper
  workflow.

### CLI Experience

```
weft run --allow titan "python train.py --lr 1e-4"
```

1. The command prints the usual metadata (job ID, host, working dir).
2. Once tmux is running, the CLI emits:
   ```
   Following live output (Ctrl+C to stop streaming; job keeps running)...
   ```
3. Stdout/stderr stream directly until you interrupt the CLI or close the
   terminal.
4. When the stream stops, hints remind you to use `weft log` / `status`.

### Flag Semantics

- Works only with immediate runs (mutually exclusive with queued runs,
  `--after`, `--after-any`).
- Compatible with `--from`, `--timeout`, env vars, etc.
- Exit codes reflect the streaming session: `0` means the SSH/tail command
  finished cleanly, `130` indicates you interrupted with Ctrl+C, and other codes
  bubble up from the SSH process. They do **not** reflect the job's exit
  status.

### Implementation Notes

- After tmux starts, the CLI spawns an `ssh` process that waits for the log file
  and tails it from the beginning (`tail -n +1 -F ...`).
- Stdin is disconnected so keystrokes never reach the remote process.
- Signal handlers (`SIGINT`, `SIGTERM`) simply cancel the local tail command.
  The job continues to run, and the CLI prints reminders on how to
  reattach later.
- If the network drops, the SSH tail process exits with a non-zero status; the
  CLI reports the error and reminds the user that the job is still running.

## Job Progress Reporting

The TUI can show incremental progress for running jobs when their log output
includes `Progress:` lines. This section documents the supported formats and how
to emit them from your jobs.

### Supported Formats

Explicit `Progress:` lines:

```
Progress: 75%
Progress: 9/14
Progress: 9 of 14
Progress: 9 out of 14
```

tqdm progress bars (detected automatically):

```
 45%|████▌     | 450/1000 [01:23<01:42, 5.38it/s]
100%|██████████| 500/500 [00:45<00:00, 11.11it/s, loss=0.234]
```

Epoch counters (fallback when no other format matches):

```
Epoch 3/10
Epoch [3/10]
```

The TUI scans the tail of each job’s log and surfaces the most recent progress
line both in the list view (status column) and the details pane (progress bar
plus step counts).

Structured progress is also the semantic signal for the rental task-stall
watchdog. Once a running job has reported progress, Weft records when its
`(job, phase, percent)` tuple last changed. Reprinting the same value, growing a
log, consuming CPU, or sending an agent heartbeat does not reset that clock.
Weft warns after 20 minutes without a change and terminates the rental after 60
minutes so the job can retry. Jobs that never report structured progress do not
enter this watchdog; bound them explicitly with `--max-time` and `--max-spend`.

Emit a new value only after meaningful work completes. For an opaque child
process, translate real milestones (for example, checkpoint shards loaded,
compilation completed, and service ready) into monotonic `Progress:` values and
give the child its own bounded no-milestone timeout. Log byte growth is useful
diagnostic activity, but it is not task progress.

### Examples

**Shell**

```bash
#!/bin/bash
total=10
for i in $(seq 1 $total); do
    echo “Progress: $i/$total”
    # ... work ...
    sleep 1
done
```

**Python**

```python
for i in range(100):
    print(f”Progress: {i+1}%”)
    # ... work ...
```

**Python with tqdm** (no changes needed — tqdm output is detected automatically):

```python
from tqdm import tqdm
for batch in tqdm(dataloader):
    train_step(batch)
```

**Python with named steps**

```python
steps = [“Loading”, “Training”, “Evaluating”, “Saving”]
for i, step in enumerate(steps, 1):
    print(f”Progress: {i} of {len(steps)}”)
    print(f”Step: {step}”)
    # ... work ...
```

### Multi-Phase Jobs

Some jobs run multiple training phases sequentially, each with its own 0→100%
progress bar (e.g., a baseline run followed by several fine-tuning configs). Weft
detects when progress resets and adjusts the displayed percentage to reflect
overall job progress.

When a multi-phase job is running, progress is shown with an approximate prefix:

```
  403  running ≈52%  my-project  sweep over regularization strengths
```

The `≈` indicates that the percentage is estimated — the total number of phases
isn’t known in advance, so weft infers it from the number of restarts observed
so far. A plain percentage (without `≈`) means the job has a single phase and
the value is exact.

#### How it works

The agent monitors the job’s log output for progress lines. When it detects a
significant drop in reported progress (more than 50 percentage points), it
treats this as a phase restart and increments a phase counter. The agent reports
both the phase number and raw percentage to R2 (e.g., `2:45` meaning “phase 2,
45% through this phase”).

The display layer (TUI and web dashboard) estimates the total number of phases
using a Bayesian model: a Poisson prior (λ=2) on the total phase count,
truncated to N > k after observing k completed phases. The posterior mean E[N |
N > k] is computed from partial sums of the Poisson PMF. This gives a
progressively better estimate as more phases are observed:

| Phases seen | Estimated total | Progress at start of current phase |
|---|---|---|
| 1 (no restarts) | 1 | 0% (exact) |
| 2 | ~3.3 | ~30% |
| 3 | ~4.3 | ~46% |
| 4 | ~5.4 | ~56% |
| 5 | ~6.5 | ~62% |

The estimate is conservative — it assumes more phases may follow. This avoids
the problem of showing 100% partway through a job. Progress is capped at 99%
for multi-phase jobs; the job status changes to “completed” when truly done.

### Display Notes

- The Jobs list shows inline percentages (e.g., `● 60%` or `● ≈52%`).
- The Details pane renders a progress bar and, when available, “step N of M”.
- `≈` prefix means the percentage is estimated across multiple detected phases.
- Parsing is case-insensitive; `progress:` works too.
- The parser scans roughly the last 500 lines of the log on each refresh.
- Progress data is ephemeral—once a job finishes, only the textual log remains.
