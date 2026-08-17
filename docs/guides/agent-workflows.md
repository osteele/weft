# Agent-Oriented Workflows

Weft includes CLI and notification surfaces that are designed for coding agents
and other unattended automation. These features favor durable job-level state,
bounded waits, explicit exit codes, and machine-readable output so agents do
not need to infer scheduler health from local processes or ad hoc polling.

## Job-Level Monitoring

Use job-level commands as the primary contract:

```bash
weft status wj42 --wait
weft status wj42 wj43 wj44 --wait
weft info wj42
weft log wj42
```

`weft status --wait` blocks until the named jobs reach terminal states and exits
successfully only when they succeed. It is designed to replace shell polling
loops such as `while true; do weft status ...; sleep ...; done`.

When a queued job is still waiting for placement or dispatch, `weft run`,
`weft status`, `weft status --wait`, and `weft info` print expectation lines:

```text
Normal range: rental placement commonly takes 5-40m and may retry 1-6 provider offers or launches
Action: wait; autopilot owns placement, monitor with weft status wj42 --wait
```

These lines are intentionally population-level guidance, not a job-specific
ETA. Job runtime estimates can be noisy, so the agent-facing contract is:
ordinary rental placement, launch, and retry states are not evidence of a stall
unless Weft reports a concrete blocker, terminal failure, or diagnostic action.

## Claude Code Channels

Claude Code channels let Weft push job events into an active Claude Code
session. After channel delivery is confirmed, an agent can submit work and keep
working instead of blocking on `status --wait`.

See [Claude Code Channels](claude-code-channels.md) for installation and the
event payload shape.

## Job-Completion Notifications

The `[notifications] command` in `config.toml` runs once per job reaching a
terminal status, with job context in environment variables: `WEFT_JOB_ID`,
`WEFT_JOB_STATUS`, `WEFT_JOB_EXIT_CODE`, `WEFT_JOB_DIR`,
`WEFT_JOB_DESCRIPTION`, `WEFT_JOB_HOST`, `WEFT_JOB_SUMMARY`, and
`WEFT_JOB_SUBMITTER_SESSION`.

`WEFT_JOB_SUBMITTER_SESSION` identifies the agent session that submitted the
job, so a notifier can wake that one session instead of every session working
in the directory. Weft captures it at submit time from the first non-empty of
`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, `AGENT_SESSION_ID` — override the
list, in priority order, with `[notifications]
submitter_session_env_vars`. The value is opaque to Weft: it is stored and
re-exported unchanged.

It is empty whenever no session can be identified — a job submitted from a
plain shell, or one recorded by a process that is not the submitter. Treat
empty as "no addressee" and fall back to whatever unaddressed delivery you
would otherwise have done, rather than dropping the notification; the session
that submitted a long job may well have exited before it finished.

With [agent-mail](https://github.com/osteele/agent-mail), which broadcasts on
an empty or unresolvable `--session`:

```toml
[notifications]
  command = "agent-mail notify --from weft --project \"$WEFT_JOB_DIR\" --message \"$WEFT_JOB_SUMMARY\" --session \"$WEFT_JOB_SUBMITTER_SESSION\" --no-slack"
```

## Machine-Readable Commands

Several commands are intended for scripts and agents:

- `weft autopilot status --quiet` returns exit codes for idle, running, stale,
  and paused autopilot states.
- `weft autopilot status --json` exposes the same state as structured JSON.
- `weft plan submit --wait <duration>` gives bounded execution for plan-driven
  batches.
- `weft status --wait --wait-timeout <duration>` gives bounded blocking
  behavior for agents that must eventually return control to the caller.

Prefer these first-class command modes over process-list inspection, provider
CLI polling, or repeated retries.

## Plans

YAML job plans are shaped for agent emission as well as human editing. They let
an agent describe a batch, dependencies, runtime metadata, and expected outputs
in one file instead of issuing many loosely-related commands.

See [Job Plans](../reference/job-plans.md) for the plan format.

## Agent-Safe Instance Operations

For workflows that intentionally create or move jobs to rentals, use explicit
non-interactive flags:

```bash
weft start instance --jobs wj42 --yes
weft move wj42 --new --yes
```

These commands still leave lifecycle ownership with Weft: launch retries,
bootstrap failure detection, job requeueing, grace periods, and teardown are
tracked in the database and surfaced through `weft status`, `weft info`, and
`weft instance audit`.

## Recommended Agent Loop

1. Submit work with `weft run`, `weft plan submit`, or a workflow-specific
   command.
2. Capture the returned job IDs.
3. If channel notifications are active, continue other work and react to pushed
   terminal events.
4. Otherwise, use one `weft status <ids> --wait` call, optionally with
   `--wait-timeout`.
5. Use `weft info`, `weft log`, `weft diagnose`, and `weft artifact` only when
   status reports a concrete failure, blocker, or completed result.

Do not treat unplaced jobs, provider retries, launch replacement, or script log
retry/backoff as infrastructure failure by themselves. Weft records those states
so agents can wait at the job level instead of intervening manually.
