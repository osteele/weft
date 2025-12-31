# Logging & Progress Features

This document collects the details behind two related usability features:
staying attached to a job’s live logs without risking the remote process, and
surfacing structured progress information in the TUI.

## Live Log Streaming (`remote-jobs run --allow`)

`--allow` lets you keep the CLI process attached to a job’s output after it has
been launched in tmux. Unlike traditional `ssh <cmd>` sessions, the job keeps
running even if you interrupt the CLI or lose connectivity—the stream only ever
tails the log file the wrapper script already writes.

### Goals

- Provide an opt-in flag that mirrors `tail -f` behavior while preserving the
  resilience guarantees of tmux-based execution.
- Make it obvious how to detach (`Ctrl+C`) and how to resume watching logs later
  (`remote-jobs log <id> -f`).
- Avoid inventing new on-host plumbing by reusing the existing log + wrapper
  workflow.

### CLI Experience

```
remote-jobs run --allow cool30 "python train.py --lr 1e-4"
```

1. The command prints the usual metadata (job ID, host, working dir).
2. Once tmux is running, the CLI emits:
   ```
   Following live output (Ctrl+C to stop streaming; job keeps running)...
   ```
3. Stdout/stderr stream directly until you interrupt the CLI or close the
   terminal.
4. When the stream stops, hints remind you to use `remote-jobs log` / `status`.

### Flag Semantics

- Works only with immediate runs (mutually exclusive with `--queue`,
  `--after`, `--after-any`).
- Compatible with `--from`, `--timeout`, env vars, etc.
- Exit codes reflect the streaming session: `0` means the SSH/tail command
  finished cleanly, `130` indicates you interrupted with Ctrl+C, and other codes
  bubble up from the SSH process. They do **not** reflect the remote job’s exit
  status.

### Implementation Notes

- After tmux starts, the CLI spawns an `ssh` process that waits for the log file
  and tails it from the beginning (`tail -n +1 -F ...`).
- Stdin is disconnected so keystrokes never reach the remote process.
- Signal handlers (`SIGINT`, `SIGTERM`) simply cancel the local tail command.
  The remote job continues to run, and the CLI prints reminders on how to
  reattach later.
- If the network drops, the SSH tail process exits with a non-zero status; the
  CLI reports the error and reminds the user that the job is still running.

## Job Progress Reporting

The TUI can show incremental progress for running jobs when their log output
includes `Progress:` lines. This section documents the supported formats and how
to emit them from your jobs.

### Supported Formats

```
Progress: 75%
Progress: 9/14
Progress: 9 of 14
Progress: 9 out of 14
```

The TUI scans the tail of each job’s log and surfaces the most recent progress
line both in the list view (status column) and the details pane (progress bar
plus step counts).

### Examples

**Shell**

```bash
#!/bin/bash
total=10
for i in $(seq 1 $total); do
    echo "Progress: $i/$total"
    # ... work ...
    sleep 1
done
```

**Python**

```python
for i in range(100):
    print(f"Progress: {i+1}%")
    # ... work ...
```

**Python with named steps**

```python
steps = ["Loading", "Training", "Evaluating", "Saving"]
for i, step in enumerate(steps, 1):
    print(f"Progress: {i} of {len(steps)}")
    print(f"Step: {step}")
    # ... work ...
```

### Display Notes

- The Jobs list shows inline percentages (e.g., `● 60%`).
- The Details pane renders a progress bar and, when available, “step N of M”.
- Parsing is case-insensitive; `progress:` works too.
- The parser scans roughly the last 500 lines of the log on each refresh.
- Progress data is ephemeral—once a job finishes, only the textual log remains.
