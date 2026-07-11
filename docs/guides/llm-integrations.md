# LLM and Claude Integrations

Weft has two different kinds of LLM integration:

- API-backed calls to Anthropic or OpenRouter, configured by `[llm]`.
- Local Claude Code CLI calls, where Weft shells out to `claude -p` or another
  configured command.

These are separate surfaces. Setting an Anthropic API key does not enable
`claude -p`, and installing Claude Code does not give Weft an Anthropic API key.

## Feature Map

| Feature | When it runs | External surface | Config and toggle | Usage accounting |
| --- | --- | --- | --- | --- |
| `weft narrate` | Only when the command is running and a watched DB transition occurs. `--once` performs a single pass. | Anthropic Messages API by default, or OpenRouter when `[llm].provider = "openrouter"`. | `[llm]` selects provider/model/key. `[ai.narrate]` controls tick timing, token limits, compaction, and optional Slack posting. No API key means the command exits without calls. | Anthropic narration and recap compaction calls are written to `llm_calls`. |
| Narration Slack posting | When `weft narrate --slack` is used or `[ai.narrate].slack = true`. | Slack incoming webhook. The LLM call is the normal `weft narrate` call, not a separate summarization pass. | `WEFT_SLACK_WEBHOOK` or `SLACK_WEBHOOK`, plus the narrate settings above. | Same as `weft narrate`. |
| Campaign and job-log Slack summaries | When Slack campaign notifications include job logs to summarize. | Anthropic or OpenRouter through the shared `[llm]` configuration. | Shared `[llm]` provider/model/key. There is no separate summary toggle; with no API key, Weft sends no LLM summary. | Not currently written to `llm_calls`. |
| TUI AI assist | User-triggered from the job TUI for job summaries, suggestions, and selected remediation actions. | Local Claude Code CLI: `claude -p --permission-mode auto`. | No config-file toggle. `WEFT_CLAUDE_BIN` overrides the binary name/path; otherwise Weft runs `claude`. | Not written to `llm_calls`; any token/accounting behavior belongs to the local Claude Code tool. |
| Automatic remediation coding agent | Only when remediation classifies a failure as a code error and `[remediation].coding_agent` is non-empty. | Whatever command is configured, often `claude -p`. Weft sends the prompt on stdin and captures output plus any resulting diff. | Opt-in with `[remediation].coding_agent`, for example `"claude -p"`. `[remediation].coding_agent_dir` sets the working directory. Empty by default. | Not written to `llm_calls`. |
| Dashboard Usage and Cost tabs | Never calls an LLM. Reads stored call records. | Local SQLite only. | No LLM config required to view existing rows. | Shows records from `llm_calls`; today that primarily means Anthropic `weft narrate` calls. |
| Claude Code channels | Sends Weft job events into a Claude Code session. | Local channel/MCP notification path, not an Anthropic API call by Weft. | Installed with `weft channel install`; see [Claude Code Channels](claude-code-channels.md). | Not written to `llm_calls`. |

## Shared API Configuration

API-backed features use `~/.config/weft/config.toml`:

```toml
[llm]
provider = "anthropic" # or "openrouter"
model = "claude-sonnet-4-20250514"
# api_key = "..." # environment variables are preferred
```

For OpenRouter:

```toml
[llm]
provider = "openrouter"
model = "anthropic/claude-sonnet-4.6"
```

API key lookup prefers environment variables over config:

- Anthropic: `ANTHROPIC_API_KEY`
- OpenRouter: `OPENROUTER_API_KEY`, `CLAUDE_OPENROUTER_API_KEY`, or
  `CLAUDEM_OPENROUTER_API_KEY`

As a fallback, `[llm].api_key` may be set in `config.toml`, or a matching
`KEY=VALUE` entry may be placed in the legacy `~/.config/weft/config` file.

There is no global `llm.enabled = false` switch. API-backed features are only
active when the corresponding feature runs and a key is available.

## Narration Configuration

`weft narrate` has its own feature settings under `[ai.narrate]`:

```toml
[ai.narrate]
tick_seconds = 30
quiet_seconds = 5
max_output_tokens = 1600
compaction_threshold_tokens = 15000
slack = false
slack_min_interval_seconds = 300
```

Slack posting for narration uses `WEFT_SLACK_WEBHOOK` or `SLACK_WEBHOOK`.
The webhook controls the destination channel.

## Claude Code CLI Configuration

The TUI AI assist path invokes Claude Code directly:

```sh
claude -p --permission-mode auto
```

Set `WEFT_CLAUDE_BIN` to use a different binary or wrapper:

```sh
export WEFT_CLAUDE_BIN=/path/to/claude
```

There is currently no `config.toml` toggle for TUI AI assist. It is
user-triggered; if the binary is missing, the action fails when invoked.

Automatic remediation is separate and is off by default:

```toml
[remediation]
coding_agent = ""          # set to "claude -p" to opt in
coding_agent_dir = ""      # optional working directory override
```

Because this command may edit the working tree and retry work, leave
`coding_agent` empty unless that behavior is intended.

## Data Sent Out

- `weft narrate` sends structured job, instance, campaign, and autopilot
  snapshots plus deltas.
- Campaign/job-log Slack summaries send the log excerpts selected for the
  notification.
- TUI AI assist sends selected job status, log, artifact, and remediation
  context to the local Claude Code CLI.
- Automatic remediation sends the failure diagnosis and log excerpt to the
  configured coding-agent command, then inspects any produced diff.

