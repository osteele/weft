# Stay-Attached Run Mode

This design describes how the CLI can let a user start a remote job and remain
attached to its live output without risking the job if the user later kills the
`remote-jobs` process.

## Goals

- Provide a `remote-jobs run` flag that keeps the CLI process attached to the
  job's stdout stream until the user explicitly interrupts the CLI.
- Ensure the remote job continues running inside tmux even if the local CLI
  process is interrupted or the network connection drops.
- Make it obvious how to detach and later reattach (e.g., via
  `remote-jobs log <id> -f`).
- Reuse the existing log and wrapper infrastructure so no new data paths are
  required on the remote machine.

## CLI Experience

```
remote-jobs run --allow cool30 "python train.py --lr 1e-4"
```

1. `remote-jobs run` prints the usual job metadata (ID, host, working dir).
2. After the tmux session is confirmed running, the CLI prints a short banner:
   ```
   Following live output (Ctrl+C to stop streaming; job keeps running)...
   ```
3. Stdout/stderr from the remote job stream directly to the user's terminal
   until they interrupt the CLI (`Ctrl+C`) or close the terminal.
4. When the stream stops, the CLI prints reminders to view logs or check status.

### Flag Semantics

- Flag name: `--allow`. This is an opt-in alternative to the
  current `--follow` flag so existing scripts keep their behavior.
- Mutually exclusive with `--queue`, `--after`, and `--after-any` because the
  user cannot stay attached to a job that starts later in a queue.
- Works with `--from`, `--timeout`, and env vars since it piggybacks on the
  regular run flow.
- Exit status: returns `0` if streaming completed cleanly, `130` if the user
  interrupted with Ctrl+C, or the SSH exit code if streaming failed for other
  reasons. These codes do **not** reflect the job's exit status; users still
  call `remote-jobs job status` for that.

## Implementation

### 1. Flag Parsing

Add `--allow` to `cmd/run.go`. Validate the same way `--follow` is
currently validated against queue/dependency flags. Because the semantics are
different (this mode does not exit automatically when the job finishes), it is a
separate boolean from `runFollow`.

### 2. Start Job (Existing Flow)

No change through the point where a tmux session is started and the job is
marked running. The wrapper already writes stdout/stderr to a log file at
`~/.cache/remote-jobs/logs/<job>.log`.

### 3. Log Streaming Pipeline

After the job is running:

1. Build a remote shell snippet:
   ```bash
   sh -c 'while [ ! -f ~/.cache/remote-jobs/logs/<job>.log ]; do sleep 1; done; tail -n +1 -F ~/.cache/remote-jobs/logs/<job>.log'
   ```
   - `-n +1` streams from the beginning so users don't miss early output.
   - The loop waits for the log file if the wrapper has not yet created it,
     keeping the pipeline portable on both GNU and BSD userlands.
2. Run it via `ssh` using `exec.CommandContext`.
   ```go
   streamCmd := exec.CommandContext(ctx, "ssh", host, tailCmd)
   streamCmd.Stdout = os.Stdout
   streamCmd.Stderr = os.Stderr
   ```
3. Disable stdin on this process so keyboard input does not go to the remote
   job. (Interactivity is out of scope for this mode.)

The CLI simply relays whatever the log contains. Because the wrapper already
prefixes log entries with start/end markers and ensures line buffering, the user
sees the same text they would see via `remote-jobs log -f`.

### 4. Signal Handling

Requirement: killing `remote-jobs` must **not** kill the remote job. We achieve
this by only tailing the log file:

- Install a signal handler (`SIGINT`, `SIGTERM`) that cancels the streaming
  context. This sends the signal to the local `tail` SSH process, which exits.
- Do **not** forward signals to the tmux session; the job keeps running.
- When streaming stops because of a signal, print a short message reminding the
  user that the job is still running and how to resume following logs.

If the user loses network connectivity, the `ssh tail` command exits with a
non-zero code. In that case, emit the error and still remind the user how to
check logs when they reconnect.

### 5. Completion Behavior

- If the job finishes while the user is attached, the streaming process keeps
  `tail -F` alive. The CLI prompts the user to press `Ctrl+C` to exit (exactly
  like `tail -f`). This ensures the CLI lifecycle is under user control.
- After the user detaches, print:
  ```
  Job 123 continues running on cool30.
  View logs later: remote-jobs log 123 -f
  Check status:   remote-jobs job status 123
  ```

- Add unit tests verifying the new flag validation rules (allow incompatible
  with queue/dependency flags).
- Add integration-style tests for the helper that builds the wait-and-tail
  command string.

### 6. Tests

- Add unit tests verifying the new flag validation rules (allow incompatible
  with queue/dependency flags).
- Add integration-style tests for the helper that builds the tail command
  (ensures `--retry -n +1 -F` structure).

## Future Enhancements

- Allow `--allow` to reconnect automatically when the SSH connection drops.
- Optionally expose a `remote-jobs attach <job-id>` helper that reuses the same
  streaming pipeline without starting a new job.
- Support interactive passthrough mode by attaching to the tmux session
  directly and trapping `SIGINT` to detach instead of killing the job.
